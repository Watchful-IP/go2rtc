package exacq

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacyLinkInventory adds this owner's log-only link from the polling era.
func legacyLinkInventory(t *testing.T, inv *Inventory, camera, id uint32) {
	t.Helper()
	prefix, err := motionOwnerPrefix(testMotionOwner)
	require.NoError(t, err)
	data := string(motionChangeXML(655360, motionChange{guid: motionGUID(prefix, camera), camera: camera}))
	parsed, err := ParseConfig([]byte(strings.Replace(data, `<Event ID="0"`, `<Event ID="`+strconv.FormatUint(uint64(id), 10)+`"`, 1)))
	require.NoError(t, err)
	require.Len(t, parsed.EventLinks, 1)
	require.Equal(t, 0, parsed.EventLinks[0].Target)
	inv.EventLinks = append(inv.EventLinks, parsed.EventLinks[0])
}

func TestWebhookPlanExplainsChangesAndFoldsLegacyCleanup(t *testing.T) {
	config := webhookTestConfig()
	inv := webhookTestInventory(t, config, false)
	legacyLinkInventory(t, inv, 459520, 3)
	plan, writes, err := planWebhook(inv, config)
	require.NoError(t, err)
	require.Equal(t, []string{"create callback target", "add camera 459264", "remove legacy log-only link for camera 459520"}, plan.Changes)
	require.Len(t, writes, 3)
	require.Contains(t, string(writes[2]), `<Event ID="3"`)
	require.Contains(t, string(writes[2]), `Delete="1"`)
	require.Equal(t, 1, plan.Delete)

	inv = webhookTestInventory(t, config, true)
	config.Authorization = "Bearer rotated"
	plan, _, err = planWebhook(inv, config)
	require.NoError(t, err)
	require.Equal(t, []string{"rotate callback authorization"}, plan.Changes)
	config.URL = "https://other.invalid/publish"
	plan, _, err = planWebhook(inv, config)
	require.NoError(t, err)
	require.Equal(t, []string{"update callback URL"}, plan.Changes)

	config = webhookTestConfig()
	config.CameraIDs = []uint32{459264, 459520}
	plan, _, err = planWebhook(inv, config)
	require.NoError(t, err)
	require.Equal(t, []string{"add camera 459520"}, plan.Changes)
	config.CameraIDs, config.URL, config.Authorization = nil, "", ""
	plan, writes, err = planWebhook(inv, config)
	require.NoError(t, err)
	require.Equal(t, []string{"remove camera 459264", "remove callback target"}, plan.Changes)
	require.Contains(t, string(writes[len(writes)-1]), `<Target ID=`, "target removal must be the last write")

	// A legacy-identified link that was edited is a conflict, never deleted.
	inv = webhookTestInventory(t, webhookTestConfig(), true)
	legacyLinkInventory(t, inv, 459520, 3)
	inv.EventLinks[len(inv.EventLinks)-1].Target = -1
	_, _, err = planWebhook(inv, webhookTestConfig())
	requireCode(t, err, CodeConfigConflict)

	_, _, err = planWebhook(inv, WebhookConfig{OwnerID: testMotionOwner, CameraIDs: []uint32{1}, URL: "https://r.invalid/", Authorization: "Bearer x"})
	requireCode(t, err, CodeCameraNotFound)
}

type fakeConfigSession struct {
	inv    *Inventory
	writes *[][]byte
	closed chan struct{}
	once   sync.Once
}

func (f *fakeConfigSession) Inventory() *Inventory { return f.inv }
func (f *fakeConfigSession) writeConfig(data []byte) error {
	*f.writes = append(*f.writes, data)
	return nil
}
func (f *fakeConfigSession) drainConfig(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.closed:
		return context.Canceled
	}
}
func (f *fakeConfigSession) Close() error { f.once.Do(func() { close(f.closed) }); return nil }

func TestApplyWebhookBacksOffVerificationAndReportsChanges(t *testing.T) {
	saved := webhookVerifyDelay
	t.Cleanup(func() { webhookVerifyDelay = saved })
	var delays []int
	webhookVerifyDelay = func(attempt int) time.Duration { delays = append(delays, attempt); return time.Millisecond }

	config := webhookTestConfig()
	pending, done := webhookTestInventory(t, config, false), webhookTestInventory(t, config, true)
	var writes [][]byte
	connects := 0
	connect := func(context.Context, Options) (configSession, error) {
		connects++
		inv := pending
		if connects > 3 { // worker plus two stale readbacks
			inv = done
		}
		return &fakeConfigSession{inv: inv, writes: &writes, closed: make(chan struct{})}, nil
	}
	plan, fresh, err := applyWebhook(context.Background(), Options{Address: "recorder:1"}, config, connect)
	require.NoError(t, err)
	require.Same(t, done, fresh)
	require.Equal(t, 4, connects, "one worker and three verification readbacks")
	require.Equal(t, []int{0, 1, 2}, delays)
	require.Len(t, writes, 2, "each mutation is sent once")
	require.True(t, plan.Configured)
	require.Equal(t, []string{"create callback target", "add camera 459264"}, plan.Changes)
	require.Equal(t, []WebhookLink{{EventID: 7, CameraID: 459264}}, plan.Links)

	connects = 0
	plan, _, err = applyWebhook(context.Background(), Options{Address: "recorder:1"}, config, func(context.Context, Options) (configSession, error) {
		connects++
		return &fakeConfigSession{inv: done, writes: &writes, closed: make(chan struct{})}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, connects, "an already configured owner needs only the worker session")
	require.Empty(t, plan.Changes)
}

func TestApplyWebhookTimesOutAsNotPersisted(t *testing.T) {
	saved := webhookVerifyDelay
	t.Cleanup(func() { webhookVerifyDelay = saved })
	webhookVerifyDelay = func(int) time.Duration { return 5 * time.Millisecond }
	config := webhookTestConfig()
	var writes [][]byte
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, _, err := applyWebhook(ctx, Options{Address: "recorder:1"}, config, func(context.Context, Options) (configSession, error) {
		return &fakeConfigSession{inv: webhookTestInventory(t, config, false), writes: &writes, closed: make(chan struct{})}, nil
	})
	requireCode(t, err, CodeConfigNotPersisted)
}

func TestWebhookVerifyDelayBacksOff(t *testing.T) {
	var got []time.Duration
	for attempt := 0; attempt < 6; attempt++ {
		got = append(got, webhookVerifyDelay(attempt))
	}
	require.Equal(t, []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 2 * time.Second, 2 * time.Second}, got)
}
