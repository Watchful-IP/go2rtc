package exacq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/reorder"
	"github.com/stretchr/testify/require"
)

type bPictureObservation struct {
	Packet     *MediaPacket
	ReceivedMS int64
	POC        int
	B          bool
	Parsed     bool
	VCLSHA256  string
}

// Captures only explicitly selected synthetic camera media. It does not
// register devices, change recording configuration, or use SDK/HTTP media.
func TestLabBPictureCorrelation(t *testing.T) {
	id, directory := os.Getenv("EXACQ_TEST_B_CAMERA"), os.Getenv("EXACQ_TEST_B_OUT")
	if id == "" || directory == "" {
		t.Skip("set authorized synthetic camera ID and private evidence directory")
	}
	camera, err := strconv.ParseUint(id, 10, 32)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	var stream Stream
	for _, s := range c.Inventory().Streams {
		if s.ID == uint32(camera) {
			stream = s
			break
		}
	}
	require.NotZero(t, stream.WireID)
	require.Contains(t, []uint32{5, 6}, stream.Format)
	require.NoError(t, c.StartLive(uint32(camera)))
	var live []*MediaPacket
	var received []int64
	for len(live) < 105 {
		packet, readErr := c.ReadMedia(ctx)
		require.NoError(t, readErr)
		if packet.StreamID != stream.WireID || packet.Type == MediaAudio {
			continue
		}
		live = append(live, packet)
		received = append(received, time.Now().UnixMilli())
	}
	require.NoError(t, c.Close())
	require.NoError(t, os.MkdirAll(directory, 0700))
	write := func(name string, packets []*MediaPacket, received []int64) {
		parser := reorder.Parser{H265: stream.Format == 6}
		observations := make([]bPictureObservation, 0, len(packets))
		counts := map[uint8]int{}
		bCount := 0
		for i, packet := range packets {
			nalus, splitErr := splitAnnexB(packet.Data)
			require.NoError(t, splitErr)
			picture := parser.Parse(h264.JoinNALU(nalus...))
			hash := sha256.New()
			for _, nalu := range nalus {
				kind := nalu[0] & 31
				vcl := kind == 1 || kind == 5
				if stream.Format == 6 {
					vcl = nalu[0]>>1&63 <= 31
				}
				if vcl {
					_, _ = hash.Write(nalu)
				}
			}
			row := bPictureObservation{Packet: packet, POC: picture.POC, B: picture.B, Parsed: picture.OK, VCLSHA256: hex.EncodeToString(hash.Sum(nil))}
			if received != nil {
				row.ReceivedMS = received[i]
			}
			observations = append(observations, row)
			counts[packet.Type]++
			if picture.B {
				bCount++
			}
		}
		data, marshalErr := json.MarshalIndent(observations, "", "  ")
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(directory, name+".json"), data, 0600))
		t.Logf("%s frames=%d wireKinds=%v parsedB=%d", name, len(packets), counts, bCount)
	}
	write("live", live, received)
	var begin, end time.Time
	for _, packet := range live[:75] {
		stamp := packet.Timestamp()
		if stamp.IsZero() {
			continue
		}
		if begin.IsZero() || stamp.Before(begin) {
			begin = stamp
		}
		if end.IsZero() || stamp.After(end) {
			end = stamp
		}
	}
	require.False(t, begin.IsZero())
	fresh := controlLabConnect(t, ctx)
	archive, err := fresh.Search(ctx, uint32(camera), begin, end)
	require.NoError(t, err)
	write("archive", archive, nil)
}
