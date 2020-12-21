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
	// Remote stream processing
	if app.Opts.StreamProcessor != nil {
		ctx.RunAsChild(func(childCtx utils.GracefulContext) {
			utils.RunWithPerseverance(func(attempt utils.AttemptContext) {
				app.runStreamProcess(baby, attempt)
			}, childCtx, utils.PerseverenceOpts{
				RunnerID:       fmt.Sprintf("stream-processor-%v", baby.UID),
				ResetThreshold: 2 * time.Second,
				Cooldown: []time.Duration{
					2 * time.Second,
					30 * time.Second,
					2 * time.Minute,
					15 * time.Minute,
					1 * time.Hour,
				},
			})
		})
	}

	// Websocket connection
	if app.Opts.LocalStreaming != nil || app.MQTTConnection != nil {
		// Websocket connection
		ws := client.NewWebsocketConnectionManager(baby.CameraUID, app.SessionStore.Session, app.RestClient)

		ws.WithReadyConnection(func(conn *client.WebsocketConnection, childCtx utils.GracefulContext) {
			app.runWebsocket(baby, conn, childCtx)
		})

		ctx.RunAsChild(func(childCtx utils.GracefulContext) {
			ws.RunWithinContext(childCtx)
		})
	}

	<-ctx.Done()
}

func (app *App) runStreamProcess(baby baby.Baby, attempt utils.AttemptContext) {
	// Reauthorize if it is not a first try or we assume we don't have a valid token
	app.RestClient.MaybeAuthorize(attempt.GetTry() > 1)

	logFilename := filepath.Join(app.Opts.DataDirectories.LogDir, fmt.Sprintf("process-%v-%v.log", baby.UID, time.Now().Format(time.RFC3339)))
	url := fmt.Sprintf("rtmps://media-secured.nanit.com/nanit/%v.%v", baby.UID, app.SessionStore.Session.AuthToken)

	r := strings.NewReplacer("{sourceUrl}", url, "{babyUid}", baby.UID)
	cmdTokens := strings.Split(r.Replace(app.Opts.StreamProcessor.CommandTemplate), " ")

	logFile, fileErr := os.Create(logFilename)
	if fileErr != nil {
		log.Fatal().Str("filename", logFilename).Err(fileErr).Msg("Unable to create log file")
	}

	defer logFile.Close()

	log.Info().Str("cmd", strings.Join(cmdTokens, " ")).Str("logfile", logFilename).Msg("Starting stream processor")

	cmd := exec.Command(cmdTokens[0], cmdTokens[1:]...)
	cmd.Stderr = logFile
	cmd.Stdout = logFile
	cmd.Dir = app.Opts.DataDirectories.VideoDir

	err := cmd.Start()
	if err != nil {
		log.Fatal().Err(err).Msg("Unable to start stream processor")
	}

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Error().Err(err).Msg("Stream processor exited")
			attempt.Fail(err)
			return
		}

		log.Warn().Msg("Stream processor exited with status 0")
		attempt.Fail(errors.New("Stream processor exited with status 0"))
		return

	case <-attempt.Done():
		log.Info().Msg("Terminating stream processor")
		if err := cmd.Process.Kill(); err != nil {
			log.Error().Err(err).Msg("Unable to kill process")
		}
	}
}

func (app *App) runWebsocket(baby baby.Baby, conn *client.WebsocketConnection, childCtx utils.GracefulContext) {
	// Reading sensor data
	conn.RegisterMessageHandler(func(m *client.Message, conn *client.WebsocketConnection) {
		// Sensor request initiated by us on start (or some other client, we don't care)
		if *m.Type == client.Message_RESPONSE && m.Response != nil {
			if *m.Response.RequestType == client.RequestType_GET_SENSOR_DATA && len(m.Response.SensorData) > 0 {
				processSensorData(baby.UID, m.Response.SensorData, app.BabyStateManager)
			}
		} else

		// Communication initiated from a cam
		// Note: it sends the updates periodically on its own + whenever some significant change occurs
		if *m.Type == client.Message_REQUEST && m.Request != nil {
			if *m.Request.Type == client.RequestType_PUT_SENSOR_DATA && len(m.Request.SensorData_) > 0 {
				processSensorData(baby.UID, m.Request.SensorData_, app.BabyStateManager)
			}
		}
	})

	// Ask for sensor data (initial request)
	conn.SendRequest(client.RequestType_GET_SENSOR_DATA, &client.Request{
		GetSensorData: &client.GetSensorData{
			All: utils.ConstRefBool(true),
		},
	})

	// Ask for logs
	// conn.SendRequest(client.RequestType_GET_LOGS, &client.Request{
	// 	GetLogs: &client.GetLogs{
	// 		Url: utils.ConstRefStr("http://192.168.3.234:8080/log"),
	// 	},
	// })

	// Local stream
	if app.Opts.LocalStreaming != nil {
		babyState := app.BabyStateManager.GetBabyState(baby.UID)

		if !babyState.GetLocalStreamingInitiated() {
			r := strings.NewReplacer("{babyUid}", baby.UID)
			localStreamURL := r.Replace(app.Opts.LocalStreaming.PushTargetURLTemplate)
			go requestLocalStreaming(baby.UID, localStreamURL, conn, app.BabyStateManager)
		}
	}

	<-childCtx.Done()
}
