package exacq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWebhookOwnerLockSerializesClientsAndReleasesWaiters(t *testing.T) {
	options := Options{Address: "recorder.invalid:22609", Username: "first", Password: "private", ProxyURL: "socks5://route.invalid:1080"}
	first, err := lockWebhookOwner(context.Background(), options, testMotionOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	options.Username, options.Password = "second", "different"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, acquired := make(chan struct{}), make(chan func(), 1)
	go func() {
		close(started)
		release, e := lockWebhookOwner(ctx, options, strings.ToUpper(testMotionOwner))
		if e == nil {
			acquired <- release
		}
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for {
		webhookOwners.Lock()
		refs := 0
		for _, entry := range webhookOwners.entries {
			refs += entry.refs
		}
		webhookOwners.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second client did not reach mutation barrier")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case release := <-acquired:
		release()
		t.Fatal("concurrent client entered the same owner mutation")
	default:
	}
	other, err := lockWebhookOwner(context.Background(), options, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	other()
	first()
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("same-owner waiter did not resume")
	}
	webhookOwners.Lock()
	remaining := len(webhookOwners.entries)
	webhookOwners.Unlock()
	if remaining != 0 {
		t.Fatal("completed locks retained registry entries")
	}
}

func TestWebhookOwnerLockBoundsActiveRegistry(t *testing.T) {
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 1024; i++ {
		release, err := lockWebhookOwner(context.Background(), Options{Address: "bounded.invalid"}, fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := lockWebhookOwner(context.Background(), Options{Address: "bounded.invalid"}, "overflow"); err == nil {
		t.Fatal("unbounded active owner registry")
	}
}

func TestWebhookOwnerLockCancellationCleansRegistry(t *testing.T) {
	options := Options{Address: "cancel.invalid:22609"}
	release, err := lockWebhookOwner(context.Background(), options, testMotionOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = lockWebhookOwner(ctx, options, testMotionOwner); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled waiter did not stop")
	}
	release()
	webhookOwners.Lock()
	remaining := len(webhookOwners.entries)
	webhookOwners.Unlock()
	if remaining != 0 {
		t.Fatal("cancelled locks retained registry entries")
	}
}
