package exacq

import (
	"errors"
	"testing"

	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
	"github.com/stretchr/testify/require"
)

func TestSourceSelectLowest(t *testing.T) {
	source, err := ParseURL("exacq://user:password@recorder/101?select=lowest")
	require.NoError(t, err)
	require.True(t, source.SelectLowest)
	for _, raw := range []string{
		"exacq://user:password@recorder/101?select=lowest&stream=7",
		"exacq://user:password@recorder/101?select=highest",
		"exacq://user:password@recorder/101?reorder=gop&gop=15",
	} {
		_, err = ParseURL(raw)
		var typed *native.Error
		require.True(t, errors.As(err, &typed), raw)
		require.Equal(t, native.CodeInvalidRequest, typed.Code, raw)
	}
}

func TestLowestStream(t *testing.T) {
	inventory := &native.Inventory{Streams: []native.Stream{
		{ID: 101, ParentCameraID: 101, Format: 5, Width: 1920, Height: 1080, Enabled: true},
		{ID: 102, ParentCameraID: 101, Format: 6, Width: 640, Height: 360, Enabled: true},
		{ID: 103, ParentCameraID: 101, Format: 5, Width: 320, Height: 180, Enabled: false},
		{ID: 104, ParentCameraID: 101, Format: 0, Width: 160, Height: 90, Enabled: true},
		{ID: 201, ParentCameraID: 201, Format: 0, Width: 640, Height: 360, Enabled: true},
		{ID: 301, ParentCameraID: 301, Format: 5, Width: 640, Height: 360, Enabled: false},
	}}
	streams, err := streamCandidates(inventory, &Source{Camera: 101, SelectLowest: true})
	stream := streams[0]
	require.NoError(t, err)
	require.Equal(t, uint32(102), stream.ID, "smallest enabled H.264/H.265 stream")
	for camera, code := range map[uint32]native.Code{201: native.CodeUnsupportedCodec, 301: native.CodeStreamUnavailable, 999: native.CodeCameraNotFound} {
		_, err = streamCandidates(inventory, &Source{Camera: camera, SelectLowest: true})
		var typed *native.Error
		require.True(t, errors.As(err, &typed))
		require.Equal(t, code, typed.Code, "camera %d", camera)
	}
}

func TestSilentStreamsAreTriedLast(t *testing.T) {
	streams := []native.Stream{{ID: 1}, {ID: 2}, {ID: 3}}
	markSilent("serial-a", 1)
	ordered := preferResponsive("serial-a", streams)
	require.Equal(t, []uint32{2, 3, 1}, []uint32{ordered[0].ID, ordered[1].ID, ordered[2].ID})
	ordered = preferResponsive("serial-b", streams)
	require.Equal(t, uint32(1), ordered[0].ID, "memory is per recorder")
}
