package h265

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestValidSEI(t *testing.T) {
	tests := []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"user data with prevention byte", []byte{5, 4, 0, 0, 3, 1, 2, 0x80}, true},
		{"Uniview size counts prevention byte", []byte{5, 5, 0, 0, 3, 1, 2, 0x80}, false},
		{"multiple messages", []byte{144, 1, 7, 5, 1, 8, 0x80}, true},
		{"extended type", []byte{255, 255, 5, 1, 7, 0x80}, true},
		{"empty payload", []byte{200, 0, 0x80}, true},
		{"empty NAL", nil, false},
		{"no message", []byte{0x80}, false},
		{"truncated type", []byte{255, 255, 255}, false},
		{"truncated size", []byte{5, 255, 255}, false},
		{"truncated second message", []byte{5, 1, 7, 5, 2, 8, 0x80}, false},
		{"missing trailing bits", []byte{5, 1, 7}, false},
		{"bad trailing bits", []byte{5, 1, 7, 0}, false},
	}
	payload := bytes.Repeat([]byte{7}, 300)
	tests = append(tests, struct {
		name  string
		body  []byte
		valid bool
	}{"extended size", append(append([]byte{5, 255, 45}, payload...), 0x80), true})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nalu := append([]byte{78, 1}, tt.body...)
			original := bytes.Clone(nalu)
			require.Equal(t, tt.valid, validSEI(nalu))
			require.Equal(t, original, nalu)
		})
	}
}

func TestRTPDepayFiltersMalformedSEI(t *testing.T) {
	// Reproduces the camera's EBSP-sized SEI without retaining device metadata.
	bad := []byte{78, 1, 5, 5, 0, 0, 3, 1, 2, 0x80}
	good := []byte{78, 1, 5, 4, 0, 0, 3, 1, 2, 0x80}
	frame := []byte{2, 1, 0x80, 7}
	for _, transport := range []string{"single", "aggregation", "fragmented"} {
		t.Run(transport, func(t *testing.T) {
			var got [][]byte
			depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
			seq := uint16(1)
			send := func(b []byte, marker bool) { depay(lossPacket(seq, 100, marker, b)); seq++ }
			send(good, false)
			switch transport {
			case "single":
				send(bad, false)
				send(frame, true)
			case "aggregation":
				send(buildAP(bad, frame), true)
			case "fragmented":
				send(append([]byte{98, 1, 128 | 39}, bad[2:6]...), false)
				send(append([]byte{98, 1, 64 | 39}, bad[6:]...), false)
				send(frame, true)
			}
			require.Equal(t, [][]byte{lossNALs(good, frame)}, got)
			// Bad metadata must not put intact dependent video into keyframe recovery.
			send(frame, true)
			require.Len(t, got, 2)
			require.Equal(t, lossNALs(frame), got[1])
		})
	}
}

func TestRTPDepayMalformedSEIOnly(t *testing.T) {
	calls := 0
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { calls++; require.NotEmpty(t, p.Payload) })
	bad := []byte{80, 1, 5, 5, 0, 0, 3, 1, 2, 0x80}
	depay(lossPacket(1, 100, true, buildAP(bad)))
	require.Zero(t, calls)
	depay(lossPacket(2, 200, true, []byte{2, 1, 0x80}))
	require.Equal(t, 1, calls)
}

func TestFilterSEIPreservesValidSuffixAndParameterSets(t *testing.T) {
	ps := []byte{64, 1, 7}
	frame := []byte{38, 1, 0x80}
	suffix := []byte{80, 1, 144, 1, 7, 0x80}
	bad := []byte{80, 1, 5, 9, 7, 0x80}
	au := lossNALs(ps, bad, frame, suffix, bad)
	require.Equal(t, lossNALs(ps, frame, suffix), filterSEI(au))
}
