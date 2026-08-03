package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestRouteActiveWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		store       string
		assignee    string
		wantRig     string
		wantPolecat bool
		wantControl bool
	}{
		{name: "qualified polecat", store: "hq", assignee: "gastown/polecats/rust", wantRig: "gastown", wantPolecat: true},
		{name: "rig fallback", store: "shortener", wantRig: "shortener"},
		{name: "witness ignored", store: "gastown", assignee: "gastown/witness", wantControl: true},
		{name: "hq unassigned", store: "hq"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rigName, polecat, control := routeActiveWork(tc.store, tc.assignee)
			if rigName != tc.wantRig || polecat != tc.wantPolecat || control != tc.wantControl {
				t.Fatalf("route = %q/%v/%v, want %q/%v/%v", rigName, polecat, control, tc.wantRig, tc.wantPolecat, tc.wantControl)
			}
		})
	}
}

func TestInventoryActiveWorkRoutesOnlyActionableIssues(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		ctx:    context.Background(),
		beadsStores: map[string]beadsdk.Storage{
			"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
				"in_progress": {
					{ID: "gt-work", Assignee: "gastown/polecats/rust", UpdatedAt: now},
					{ID: "gt-witness", Assignee: "gastown/witness", UpdatedAt: now},
					{ID: "hq-unassigned", UpdatedAt: now},
					{ID: "hq-malformed", Assignee: "gastown/polecats/", UpdatedAt: now},
					{ID: "hq-wisp", Ephemeral: true, UpdatedAt: now},
				},
			}},
			"shortener": &searchStorage{results: map[string][]*beadsdk.Issue{
				"hooked": {{ID: "sc-work", UpdatedAt: now}},
			}},
		},
	}

	inventory := d.inventoryActiveWork()
	if !inventory.Available {
		t.Fatal("inventory unexpectedly unavailable")
	}
	if got := len(inventory.ByRig["gastown"]); got != 1 || inventory.ByRig["gastown"][0].ID != "gt-work" {
		t.Fatalf("gastown inventory = %+v, want only gt-work", inventory.ByRig["gastown"])
	}
	if got := len(inventory.ByRig["shortener"]); got != 1 || inventory.ByRig["shortener"][0].ID != "sc-work" {
		t.Fatalf("shortener inventory = %+v, want only sc-work", inventory.ByRig["shortener"])
	}
	if got := len(inventory.Unrouteable); got != 1 || inventory.Unrouteable[0].ID != "hq-malformed" {
		t.Fatalf("unrouteable = %+v, want only hq-malformed", inventory.Unrouteable)
	}
}

func TestInventoryActiveWorkReportsGlobalStoreOutage(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		ctx:    context.Background(),
	}

	inventory := d.inventoryActiveWork()
	if inventory.Available {
		t.Fatal("inventory reported available with no open beads stores")
	}
	if len(inventory.ByRig) != 0 {
		t.Fatalf("global store outage produced per-rig recovery work: %+v", inventory.ByRig)
	}
}

func TestScheduleActiveWorkRecoveryDeduplicatesGlobalStoreOutage(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logs, "", 0),
		ctx:    context.Background(),
	}

	d.scheduleActiveWorkRecovery()
	d.scheduleActiveWorkRecovery()
	if got := strings.Count(logs.String(), "active-work recovery deferred"); got != 1 {
		t.Fatalf("deferred log count = %d, want 1; logs: %s", got, logs.String())
	}
	if d.activeWorkRecovery != nil {
		t.Fatal("global store outage initialized active-work recovery")
	}

	d.beadsStores = map[string]beadsdk.Storage{"hq": &searchStorage{}}
	d.scheduleActiveWorkRecovery()
	if got := strings.Count(logs.String(), "active-work recovery resumed"); got != 1 {
		t.Fatalf("resumed log count = %d, want 1; logs: %s", got, logs.String())
	}
	if d.activeWorkRecovery == nil {
		t.Fatal("available store did not initialize active-work recovery")
	}
}

func waitForRecoveryState(t *testing.T, r *activeWorkRecovery, rigName, outcome string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		state := r.state.Rigs[rigName]
		inFlight := r.inFlight[rigName]
		got := ""
		if state != nil {
			got = state.LastOutcome
		}
		r.mu.Unlock()
		if !inFlight && got == outcome {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s outcome %q", rigName, outcome)
}

func TestActiveWorkRecoveryFingerprintCooldownAndPersistence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.DaemonThresholds{ActiveWorkScanInterval: "10m"}
	r := newActiveWorkRecovery(root, "gt", log.New(io.Discard, "", 0), context.Background(), cfg)
	r.sendMail = func(_, _, _ string) error { return nil }
	runs := make(chan string, 3)
	r.runScan = func(_ context.Context, rigName string) (*patrolScanSummary, error) {
		runs <- rigName
		return &patrolScanSummary{}, nil
	}

	item := activeWorkItem{ID: "gt-1", Status: "in_progress", Assignee: "gastown/polecats/rust", UpdatedAt: time.Unix(100, 0)}
	r.schedule(activeWorkInventory{ByRig: map[string][]activeWorkItem{"gastown": {item}}})
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("initial fingerprint did not scan immediately")
	}
	waitForRecoveryState(t, r, "gastown", "healthy")

	r.schedule(activeWorkInventory{ByRig: map[string][]activeWorkItem{"gastown": {item}}})
	select {
	case <-runs:
		t.Fatal("unchanged fingerprint bypassed cooldown")
	case <-time.After(50 * time.Millisecond):
	}

	item.UpdatedAt = item.UpdatedAt.Add(time.Second)
	r.schedule(activeWorkInventory{ByRig: map[string][]activeWorkItem{"gastown": {item}}})
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("changed fingerprint did not scan immediately")
	}
	waitForRecoveryState(t, r, "gastown", "healthy")

	info, err := os.Stat(r.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions = %o, want 600", info.Mode().Perm())
	}
	reloaded := newActiveWorkRecovery(root, "gt", log.New(io.Discard, "", 0), context.Background(), cfg)
	if reloaded.state.Rigs["gastown"].Fingerprint == "" {
		t.Fatal("persisted fingerprint was not reloaded")
	}
}

func TestActiveWorkRecoveryEscalatesRepeatedFailureOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.DaemonThresholds{ActiveWorkScanInterval: "1ms"}
	r := newActiveWorkRecovery(root, "gt", log.New(io.Discard, "", 0), context.Background(), cfg)
	r.runScan = func(context.Context, string) (*patrolScanSummary, error) {
		return nil, errors.New("same failure")
	}
	var mu sync.Mutex
	mailCount := 0
	r.sendMail = func(to, _, _ string) error {
		if to != "gastown/witness" {
			t.Fatalf("mail target = %q, want gastown/witness", to)
		}
		mu.Lock()
		mailCount++
		mu.Unlock()
		return nil
	}
	items := activeWorkInventory{ByRig: map[string][]activeWorkItem{
		"gastown": {{ID: "gt-1", Status: "in_progress", Assignee: "gastown/polecats/rust"}},
	}}

	for attempt := 1; attempt <= 3; attempt++ {
		r.schedule(items)
		waitForRecoveryState(t, r, "gastown", "failed")
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	got := mailCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("escalation mail count = %d, want 1", got)
	}
}

func TestActiveWorkRecoveryRequiresFingerprintProgressAfterReportedRecovery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.DaemonThresholds{ActiveWorkScanInterval: "1ms"}
	r := newActiveWorkRecovery(root, "gt", log.New(io.Discard, "", 0), context.Background(), cfg)
	r.runScan = func(context.Context, string) (*patrolScanSummary, error) {
		summary := &patrolScanSummary{}
		summary.Zombies.Found = 1
		return summary, nil
	}
	var mu sync.Mutex
	mailCount := 0
	r.sendMail = func(_, _, _ string) error {
		mu.Lock()
		mailCount++
		mu.Unlock()
		return nil
	}
	items := activeWorkInventory{ByRig: map[string][]activeWorkItem{
		"gastown": {{ID: "gt-1", Status: "in_progress", Assignee: "gastown/polecats/rust"}},
	}}

	r.schedule(items)
	waitForRecoveryState(t, r, "gastown", "verification-pending:recovered:1")
	for attempt := 1; attempt <= 2; attempt++ {
		time.Sleep(2 * time.Millisecond)
		r.schedule(items)
		waitForRecoveryState(t, r, "gastown", "failed")
	}
	mu.Lock()
	got := mailCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("unchanged reported recovery escalation count = %d, want 1", got)
	}
}

func TestActiveWorkRecoveryHonorsTimeout(t *testing.T) {
	t.Parallel()
	cfg := &config.DaemonThresholds{ActiveWorkScanTimeout: "20ms"}
	r := newActiveWorkRecovery(t.TempDir(), "gt", log.New(io.Discard, "", 0), context.Background(), cfg)
	r.sendMail = func(_, _, _ string) error { return nil }
	r.runScan = func(ctx context.Context, _ string) (*patrolScanSummary, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.schedule(activeWorkInventory{ByRig: map[string][]activeWorkItem{
		"gastown": {{ID: "gt-1", Status: "in_progress", Assignee: "gastown/polecats/rust"}},
	}})
	waitForRecoveryState(t, r, "gastown", "failed")
}
