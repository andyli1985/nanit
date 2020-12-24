package app

import (
	"io"
	"os/exec"

	"github.com/rs/zerolog/log"
	"github.com/tevino/abool"
	"github.com/yutopp/go-flv"
	flvtag "github.com/yutopp/go-flv/tag"
	"gitlab.com/adam.stanek/nanit/pkg/baby"
	"gitlab.com/adam.stanek/nanit/pkg/utils"
)

// dummyPlayer - dummy player based on the ffmpeg which we use to determine liveness of the stream
func (app *App) dummyPlayer(babyUID string, ctx utils.GracefulContext) {
	sublog := log.With().Str("player", babyUID).Logger()
	url := app.getLocalStreamURL(babyUID)

	cmd := exec.Command("ffmpeg", "-i", url, "-f", "flv", "-")

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		sublog.Fatal().Err(err).Msg("Failed to prepare stderr pipe")
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		sublog.Fatal().Err(err).Msg("Failed to prepare stdout pipe")
	}

	err = cmd.Start()
	if err != nil {
		sublog.Fatal().Err(err).Msg("Unable to start")
	} else {
		sublog.Info().Str("url", url).Msg("Player started")
	}

	exitedC := make(chan struct{}, 1)
	go func() {
		cmd.Wait()
		exitedC <- struct{}{}
	}()

	exitingFlag := abool.New()

	// Tail standard error
	stderrC := make(chan utils.LogTailer, 1)
	go func() {
		tailer := utils.NewLogTailer(3)
		tailer.Tail(stderrPipe)
		stderrC <- *tailer
	}()

	// Decode standard output
	decoderC := make(chan error, 1)
	go func() {
		dec, err := flv.NewDecoder(stdoutPipe)

		if err != nil {
			if !exitingFlag.IsSet() {
				if err == io.EOF {
					sublog.Warn().Msg("Closed pipe")
				} else {
					sublog.Warn().Err(err).Msg("Unable to decode")
				}

				decoderC <- err
			}
			return
		}

		// fmt.Printf("Header: %+v\n", dec.Header())

		sublog.Debug().Msg("Successfully decoded stream header")
		sublog.Info().Str("url", url).Msg("Stream is alive")

		streamingStoppedUpdate := baby.State{}
		streamingStoppedUpdate.SetIsStreamAlive(true)
		app.BabyStateManager.Update(babyUID, streamingStoppedUpdate)

		var flvTag flvtag.FlvTag
		for {
			if err := dec.Decode(&flvTag); err != nil {
				if !exitingFlag.IsSet() {
					if err == io.EOF {
						sublog.Warn().Msg("Closed pipe")
					} else {
						sublog.Warn().Err(err).Msg("Failed to decode FLV tag")
						decoderC <- err
						return
					}
				}
			}

			flvTag.Close() // Discard unread buffers
		}
	}()

	for {
		select {
		case <-exitedC:
			exitingFlag.Set()
			exitCode := cmd.ProcessState.ExitCode()
			if exitCode == -1 {
				sublog.Warn().Msg("Player terminated")
			} else {
				tailer := <-stderrC
				sublog.Error().Int("code", exitCode).Str("logtail", tailer.String()).Msg("Player exited")
			}

			return

		case <-ctx.Done():
			if !exitingFlag.IsSet() {
				exitingFlag.Set()
				sublog.Debug().Msg("Cancel request received, killing the process")
				cmd.Process.Kill()
			}
		case <-decoderC:
			sublog.Debug().Msg("Decoder failure, killing the process")
			cmd.Process.Kill()
		}
	}
}