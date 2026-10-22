package exacq

import (
	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/exacq"
)

func Init() {
	log = app.GetLogger("exacq")
	streams.HandleFunc("exacq", exacq.Dial)
	api.HandleFunc("api/exacq", controlHandler)
}
