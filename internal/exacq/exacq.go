package exacq

import (
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/exacq"
)

func Init() {
	streams.HandleFunc("exacq", exacq.Dial)
}
