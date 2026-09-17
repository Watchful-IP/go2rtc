package mp4

import (
	"io"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func (c *Consumer) watchEnd(track *core.Receiver, sender *core.Sender) {
	select {
	case <-c.done:
		return
	case <-track.Done():
	}
	select {
	case <-c.done:
		return
	case <-c.started:
	}
	// Draining the sender before closing output retains the final fragment.
	sender.Close()
	sender.Wait()
	c.mu.Lock()
	c.ended++
	if err := track.EndError(); err != nil && c.endErr == nil {
		c.endErr = err
	}
	complete := c.ended == len(c.Senders)
	err := c.endErr
	c.mu.Unlock()
	if complete {
		if err == nil {
			err = io.EOF
		}
		c.wr.Finish(err)
	}
}

func (c *Consumer) Stop() error {
	c.stopOnce.Do(func() { close(c.done) })
	return c.Connection.Stop()
}
