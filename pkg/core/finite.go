package core

// FiniteProducer must not be reconnected after completion or failure: replaying
// the same file would duplicate samples in attached recordings.
type FiniteProducer interface{ IsFinite() bool }

// End marks a track terminal after its final packet has been dispatched.
func (r *Receiver) End(err error) {
	r.endOnce.Do(func() { r.endErr = err; close(r.ended) })
}

func (r *Receiver) Done() <-chan struct{} { return r.ended }

// EndError is read only after Done closes.
func (r *Receiver) EndError() error { return r.endErr }
