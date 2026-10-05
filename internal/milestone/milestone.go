package milestone

import (
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/milestone"
)

func Init() {
	streams.HandleFunc("milestone", milestone.Dial)
	streams.HandleFunc("milestones", milestone.Dial)
	streams.HandleFunc("milestonex", milestone.Dial)
}
