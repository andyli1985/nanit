package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gitlab.com/adam.stanek/nanit/pkg/baby"
	"gitlab.com/adam.stanek/nanit/pkg/client"
	"gitlab.com/adam.stanek/nanit/pkg/mqtt"
	"gitlab.com/adam.stanek/nanit/pkg/player"
	"gitlab.com/adam.stanek/nanit/pkg/rtmpserver"
	"gitlab.com/adam.stanek/nanit/pkg/session"
	"gitlab.com/adam.stanek/nanit/pkg/utils"
)

// App - application container
type App struct {
	Opts             Opts
	SessionStore     *session.Store
	BabyStateManager *baby.StateManager
	RestClient       *client.NanitClient
	MQTTConnection   *mqtt.Connection
}

// NewApp - constructor
func NewApp(opts Opts) *App {
	sessionStore := session.InitSessionStore(opts.SessionFile)

	instance := &App{
		Opts:             opts,
		BabyStateManager: baby.NewStateManager(),
		SessionStore:     sessionStore,
		RestClient: &client.NanitClient{
			Email:        opts.NanitCredentials.Email,
			Password:     opts.NanitCredentials.Password,
			SessionStore: sessionStore,
		},
	}

	if opts.MQTT != nil {
		instance.MQTTConnection = mqtt.NewConnection(*opts.MQTT)
	}

	return instance
}

// Run - application main loop
func (app *App) Run(ctx utils.GracefulContext) {
	// Reauthorize if we don't have a token or we assume it is invalid
	app.RestClient.MaybeAuthorize(false)

	// Fetches babies info if they are not present in session
	app.RestClient.EnsureBabies()

	// RTMP
	if app.Opts.RTMP != nil {
		go rtmpserver.StartRTMPServer(app.Opts.RTMP.ListenAddr, app.BabyStateManager)
	}

	// MQTT
	if app.MQTTConnection != nil {
		ctx.RunAsChild(func(childCtx utils.GracefulContext) {
			app.MQTTConnection.Run(app.BabyStateManager, childCtx)
		})
	}

	// Start reading the data from the stream
	for _, babyInfo := range app.SessionStore.Session.Babies {
		ctx.RunAsChild(func(childCtx utils.GracefulContext) {
			app.handleBaby(babyInfo, childCtx)
		})
	}

	// Start serving content over HTTP
	if app.Opts.HTTPEnabled {
		go serve(app.SessionStore.Session.Babies, app.Opts.DataDirectories)
	}

	<-ctx.Done()
}

func (app *App) handleBaby(baby baby.Baby, ctx utils.GracefulContext) {
	// Websocket connection
	if app.Opts.RTMP != nil || app.MQTTConnection != nil {
		// Websocket connection
		ws := client.NewWebsocketConnectionManager(baby.CameraUID, app.SessionStore.Session, app.RestClient)

		ws.WithReadyConnection(func(conn *client.WebsocketConnection, childCtx utils.GracefulContext) {
			app.runWebsocket(baby.UID, conn, childCtx)
		})

		ctx.RunAsChild(func(childCtx utils.GracefulContext) {
			ws.RunWithinContext(childCtx)
		})
	}

	<-ctx.Done()
}

func (app *App) runStreamProcess(babyUID string, name string, commandTemplate string, attempt utils.GracefulContext) {
	sublog := log.With().Str("processor", name).Logger()

	logFilename := filepath.Join(app.Opts.DataDirectories.LogDir, fmt.Sprintf("%v-%v-%v.log", name, babyUID, time.Now().Format(time.RFC3339)))

	r := strings.NewReplacer("{remoteStreamUrl}", app.getRemoteStreamURL(babyUID), "{localStreamUrl}", app.getLocalStreamURL(babyUID), "{babyUid}", babyUID)
	cmdTokens := strings.Split(r.Replace(commandTemplate), " ")

	logFile, fileErr := os.Create(logFilename)
	if fileErr != nil {
		sublog.Fatal().Str("filename", logFilename).Err(fileErr).Msg("Unable to create log file")
	}

	defer logFile.Close()

	sublog.Info().Str("cmd", strings.Join(cmdTokens, " ")).Str("logfile", logFilename).Msg("Starting stream processor")

	cmd := exec.Command(cmdTokens[0], cmdTokens[1:]...)
	cmd.Stderr = logFile
	cmd.Stdout = logFile
	cmd.Dir = app.Opts.DataDirectories.VideoDir

	err := cmd.Start()
	if err != nil {
		sublog.Fatal().Err(err).Msg("Unable to start stream processor")
	}

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			sublog.Error().Err(err).Msg("Stream processor exited")
			attempt.Fail(err)
			return
		}

		sublog.Warn().Msg("Stream processor exited with status 0")
		attempt.Fail(errors.New("Stream processor exited with status 0"))
		return

	case <-attempt.Done():
		sublog.Info().Msg("Terminating stream processor")
		if err := cmd.Process.Kill(); err != nil {
			sublog.Error().Err(err).Msg("Unable to kill process")
		}
	}
}

func (app *App) runWebsocket(babyUID string, conn *client.WebsocketConnection, childCtx utils.GracefulContext) {
	// Reading sensor data
	conn.RegisterMessageHandler(func(m *client.Message, conn *client.WebsocketConnection) {
		// Sensor request initiated by us on start (or some other client, we don't care)
		if *m.Type == client.Message_RESPONSE && m.Response != nil {
			if *m.Response.RequestType == client.RequestType_GET_SENSOR_DATA && len(m.Response.SensorData) > 0 {
				processSensorData(babyUID, m.Response.SensorData, app.BabyStateManager)
			}
		} else

		// Communication initiated from a cam
		// Note: it sends the updates periodically on its own + whenever some significant change occurs
		if *m.Type == client.Message_REQUEST && m.Request != nil {
			if *m.Request.Type == client.RequestType_PUT_SENSOR_DATA && len(m.Request.SensorData_) > 0 {
				processSensorData(babyUID, m.Request.SensorData_, app.BabyStateManager)
			}
		}
	})

	// Ask for sensor data (initial request)
	conn.SendRequest(client.RequestType_GET_SENSOR_DATA, &client.Request{
		GetSensorData: &client.GetSensorData{
			All: utils.ConstRefBool(true),
		},
	})

	// Ask for status
	// conn.SendRequest(client.RequestType_GET_STATUS, &client.Request{
	// 	GetStatus_: &client.GetStatus{
	// 		All: utils.ConstRefBool(true),
	// 	},
	// })

	// Ask for logs
	// conn.SendRequest(client.RequestType_GET_LOGS, &client.Request{
	// 	GetLogs: &client.GetLogs{
	// 		Url: utils.ConstRefStr("http://192.168.3.234:8080/log"),
	// 	},
	// })

	var cleanup func()

	// Local streaming
	if app.Opts.RTMP != nil {
		initializeLocalStreaming := func() {
			requestLocalStreaming(babyUID, app.getLocalStreamURL(babyUID), client.Streaming_STARTED, conn, app.BabyStateManager)
		}

		// Watch for stream liveness change
		unsubscribe := app.BabyStateManager.Subscribe(func(updatedBabyUID string, stateUpdate baby.State) {
			// Do another streaming request if stream just turned unhealthy
			if updatedBabyUID == babyUID && stateUpdate.StreamState != nil && *stateUpdate.StreamState == baby.StreamState_Unhealthy {
				// Prevent duplicate request if we already received failure
				if app.BabyStateManager.GetBabyState(babyUID).GetStreamRequestState() != baby.StreamRequestState_RequestFailed {
					go initializeLocalStreaming()
				}
			}
		})

		cleanup = func() {
			// Stop listening for stream liveness change
			unsubscribe()

			// Stop local streaming
			requestLocalStreaming(babyUID, app.getLocalStreamURL(babyUID), client.Streaming_STOPPED, conn, app.BabyStateManager)
		}

		// Initialize local streaming upon connection if we know that the stream is not alive
		babyState := app.BabyStateManager.GetBabyState(babyUID)
		if babyState.GetStreamState() != baby.StreamState_Alive {
			if babyState.GetStreamRequestState() != baby.StreamRequestState_Requested || babyState.GetStreamState() == baby.StreamState_Unhealthy {
				go initializeLocalStreaming()
			}
		}
	}

	<-childCtx.Done()
	if cleanup != nil {
		cleanup()
	}
}

func (app *App) runWatchDog(babyUID string, ctx utils.GracefulContext) {
	timer := time.NewTimer(0)

	for {
		select {
		case <-timer.C:
			// Note: Normally we could stop trying here, but there can be a situation when the first player attempt fails due
			// RTMP server not being ready yet. Streaming can already be requested by some previous run of the application and therefore
			// it gets rejected (StreamRequestState_RequestFailed). However cam can still try to attempt stream to the server later, because
			// it tries several attempts before giving up. The stream can then become alive.
			// if app.BabyStateManager.GetBabyState(babyUID).GetStreamRequestState() != baby.StreamRequestState_RequestFailed {
			log.Debug().Str("baby_uid", babyUID).Msg("Starting local stream watch dog")

			player.Run(player.Opts{
				BabyUID:          babyUID,
				URL:              app.getLocalStreamURL(babyUID),
				BabyStateManager: app.BabyStateManager,
			}, ctx)

			app.BabyStateManager.Update(babyUID, *baby.NewState().SetStreamState(baby.StreamState_Unhealthy))
			if app.BabyStateManager.GetBabyState(babyUID).GetStreamRequestState() == baby.StreamRequestState_RequestFailed {
				log.Error().Str("baby_uid", babyUID).Msg("Stream is dead and we failed to request it")
			}
			// }

			timer.Reset(5 * time.Second)

		case <-ctx.Done():
			log.Debug().Str("baby_uid", babyUID).Msg("Terminating watchdog")
			return
		}
	}
}

func (app *App) getRemoteStreamURL(babyUID string) string {
	return fmt.Sprintf("rtmps://media-secured.nanit.com/nanit/%v.%v", babyUID, app.SessionStore.Session.AuthToken)
}

func (app *App) getLocalStreamURL(babyUID string) string {
	if app.Opts.RTMP != nil {
		tpl := "rtmp://{publicAddr}/local/{babyUid}"
		return strings.NewReplacer("{publicAddr}", app.Opts.RTMP.PublicAddr, "{babyUid}", babyUID).Replace(tpl)
	}

	return ""
}
