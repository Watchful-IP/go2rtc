package mjpeg

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebSocketReportsTheEndOfFinitePlayback(t *testing.T) {
	require.Equal(t, "end", endMessage(io.EOF).Type)
	failed := endMessage(errors.New("recorder closed the connection"))
	require.Equal(t, "error", failed.Type)
	require.Equal(t, "mjpeg: input failed", failed.Value, "source errors are not echoed to the client")
	dropped := fmt.Errorf("recorder connection lost: %w", io.EOF)
	require.Equal(t, "error", endMessage(dropped).Type, "a truncated clip is not a clean end")
	require.Nil(t, endMessage(io.ErrClosedPipe), "a client that left gets nothing")
	require.Nil(t, endMessage(nil))
}
