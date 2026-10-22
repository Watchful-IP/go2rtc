package exacq

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
)

type webhookOwnerLock struct {
	token chan struct{}
	refs  int
}

var webhookOwners = struct {
	sync.Mutex
	entries map[[32]byte]*webhookOwnerLock
}{entries: make(map[[32]byte]*webhookOwnerLock)}

// Server-assigned Event IDs make concurrent creates non-idempotent. Serialize
// one recorder/route/owner across clients, retaining only hashes while in use.
func lockWebhookOwner(ctx context.Context, options Options, owner string) (func(), error) {
	key := sha256.Sum256([]byte(strings.ToLower(options.Address) + "\x00" + options.ProxyURL + "\x00" + strings.ToLower(owner)))
	webhookOwners.Lock()
	entry := webhookOwners.entries[key]
	if entry == nil {
		if len(webhookOwners.entries) >= 1024 {
			webhookOwners.Unlock()
			return nil, errors.New("exacq: too many concurrent webhook owners")
		}
		entry = &webhookOwnerLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		webhookOwners.entries[key] = entry
	}
	entry.refs++
	webhookOwners.Unlock()
	dereference := func() {
		webhookOwners.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(webhookOwners.entries, key)
		}
		webhookOwners.Unlock()
	}
	select {
	case <-ctx.Done():
		dereference()
		return nil, ctx.Err()
	case <-entry.token:
		var once sync.Once
		release := func() { once.Do(func() { entry.token <- struct{}{}; dereference() }) }
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}
