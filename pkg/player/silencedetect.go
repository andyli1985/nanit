package player

import "regexp"

// Example: [silencedetect @ 0x7fc1f3625880]
var ffmpegSilenceDetectPrefixRX = regexp.MustCompile(`^\[silencedetect\s@\s0x[0-9a-f]+\]\s+`)

// Example: silence_start: 0
var ffmpegSilenceDetectStartRX = regexp.MustCompile(`^silence_start:\s(-?[0-9]+(\.[0-9]+)?)$`)

// Example: silence_end: 2.21243 | silence_duration: 2.21243
var ffmpegSilenceDetectEndRX = regexp.MustCompile(`^silence_end:\s(-?[0-9]+(\.[0-9]+)?)`)

type ffmpegSilenceDetect int8

const (
	ffmpegSilenceDetectEvent_unknown ffmpegSilenceDetect = iota
	ffmpegSilenceDetectEvent_start
	ffmpegSilenceDetectEvent_end
)

func parseSilenceDetectLog(line string) ffmpegSilenceDetect {
	prefix := ffmpegSilenceDetectPrefixRX.FindString(line)
	if prefix != "" {
		withoutPrefix := line[len(prefix):]

		if ffmpegSilenceDetectStartRX.MatchString(withoutPrefix) {
			return ffmpegSilenceDetectEvent_start
		}

		if ffmpegSilenceDetectEndRX.MatchString(withoutPrefix) {
			return ffmpegSilenceDetectEvent_end
		}
	}

	return ffmpegSilenceDetectEvent_unknown
}
