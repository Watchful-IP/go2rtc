package streams

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/stretchr/testify/require"
)

func TestFiniteProducerDoesNotReconnect(t *testing.T) {
	data, err := os.ReadFile("../../pkg/mp4/testdata/progressive.mp4")
	require.NoError(t, err)
	var opens atomic.Int32
	HandleFunc("finite-test", func(string) (core.Producer, error) {
		opens.Add(1)
		return mp4.OpenFile(bytes.NewReader(data), int64(len(data)))
	})
	defer delete(handlers, "finite-test")
	s := NewStream("finite-test:clip")
	c := mp4.NewConsumer(nil)
	require.NoError(t, s.AddConsumer(c))
	defer s.RemoveConsumer(c)
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo(io.Discard); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(5 * time.Second):
		t.Fatal("file failed to finish")
	}
	// A late subscriber must see completion rather than trigger an implicit replay.
	late := mp4.NewConsumer(nil)
	require.NoError(t, s.AddConsumer(late))
	defer s.RemoveConsumer(late)
	lateDone := make(chan error, 1)
	go func() { _, err := late.WriteTo(io.Discard); lateDone <- err }()
	select {
	case err := <-lateDone:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(time.Second):
		t.Fatal("late consumer did not complete")
	}
	require.EqualValues(t, 1, opens.Load())
}

type failedFile struct{ core.Connection }

func (p *failedFile) IsFinite() bool { return true }
func (p *failedFile) Start() error   { return errors.New("truncated source") }

func TestFiniteFailureDoesNotReconnect(t *testing.T) {
	var opens atomic.Int32
	HandleFunc("failed-file", func(string) (core.Producer, error) { opens.Add(1); return nil, errors.New("must not redial") })
	defer delete(handlers, "failed-file")
	p := NewProducer("failed-file:clip")
	p.state = stateStart
	p.workerID = 1
	p.worker(&failedFile{}, 1)
	require.Zero(t, opens.Load())
}
