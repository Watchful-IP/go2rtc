package core

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSampleTimingIsLocalAndCloneSafe(t *testing.T) {
	p := &Packet{}
	timing := SampleTiming{DecodeTime: 1 << 40, Duration: 9000, CompositionOffset: 18000}
	SetSampleTiming(p, timing)
	got, ok := GetSampleTiming(p)
	require.True(t, ok)
	require.Equal(t, timing, got)
	clone := *p
	SetSampleTiming(&clone, SampleTiming{DecodeTime: 2, Duration: 3})
	got, ok = GetSampleTiming(p)
	require.True(t, ok)
	require.Equal(t, timing, got)
	p.Version = 2
	_, ok = GetSampleTiming(p)
	require.False(t, ok)
}
