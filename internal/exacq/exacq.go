package exacq

import (
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/exacq"
	"github.com/rs/zerolog"
)

func Init() {
	log := app.GetLogger("exacq")
	streams.HandleFunc("exacq", func(url string) (core.Producer, error) {
		prod, err := exacq.Dial(url)
		if p, ok := prod.(*exacq.Producer); ok {
			logStartup(log, p)
		}
		return prod, err
	})
}

// logStartup records where a start's time went, without the source URL
// (it carries credentials).
func logStartup(log zerolog.Logger, p *exacq.Producer) {
	s := p.Startup
	phases := zerolog.Dict()
	for _, phase := range s.Phases {
		phases.Int64(phase.Phase, phase.Duration.Milliseconds())
	}
	log.Info().Str("host", p.RemoteAddr).Uint32("stream", s.Stream).Bool("playback", p.IsFinite()).
		Int64("total_ms", s.Total.Milliseconds()).Int64("video_ms", s.Video.Milliseconds()).
		Dict("phase_ms", phases).Int("sessions", s.Sessions).Int("bytes", s.Bytes).Msg("[exacq] started")
}
