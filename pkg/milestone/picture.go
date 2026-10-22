package milestone

import "github.com/AlexxIT/go2rtc/pkg/reorder"

// picture describes the first slice of a coded picture.
type picture struct {
	ok    bool // slice header parsed
	b     bool // bi-predicted, so display order differs from decode order
	reset bool // POC restarts here (IDR)
	poc   int
}

// pictureParser adapts the shared reorder.Parser.
type pictureParser struct {
	h265     bool
	interval float64 // H.264 VUI frame interval in ms, 0 if not signalled
	parser   reorder.Parser
}

func (p *pictureParser) parse(avcc []byte) picture {
	p.parser.H265 = p.h265
	pic := p.parser.Parse(avcc)
	p.interval = p.parser.Interval
	return picture{ok: pic.OK, b: pic.B, reset: pic.Reset, poc: pic.POC}
}
