package exacq

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiagnoseStopsAtFirstFailingStep(t *testing.T) {
	d := Diagnose(context.Background(), Options{Address: closedPort(t), Timeout: time.Second})
	require.False(t, d.OK)
	require.Len(t, d.Steps, 1)
	require.Equal(t, "dial", d.Steps[0].Step)
	requireCode(t, d.Steps[0].Err, CodeUnreachable)

	address := peer(t, func(conn net.Conn) { _, _ = conn.Write([]byte("HTTP/1.0 200 OK\r\n\r\n")) })
	d = Diagnose(context.Background(), Options{Address: address, Timeout: time.Second})
	require.False(t, d.OK)
	require.Len(t, d.Steps, 2)
	require.True(t, d.Steps[0].OK)
	require.Equal(t, "connected directly to "+address, d.Steps[0].Detail)
	require.Equal(t, "handshake", d.Steps[1].Step)
	requireCode(t, d.Steps[1].Err, CodeNotExacq)
	require.Nil(t, d.Recorder)
}
