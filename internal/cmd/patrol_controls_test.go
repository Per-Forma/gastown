package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/session"
)

func TestGatherPatrolControlsCanonicalSessionsAndDownIsData(t *testing.T) {
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("cy", "canary")
	reg.Register("sh", "shortener")
	reg.Register("gt", "gastown")
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })

	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for rigName, expected := range map[string][2]string{
		"canary":    {"cy-witness", "cy-refinery"},
		"shortener": {"sh-witness", "sh-refinery"},
		"gastown":   {"gt-witness", "gt-refinery"},
	} {
		out, err := gatherPatrolControls(rigName, patrolControlsDeps{
			sessionRunning:   func(string) (bool, error) { return false, nil },
			mayorStatus:      func() (bool, string, error) { return false, "none", nil },
			heartbeat:        func() *deacon.Heartbeat { return nil },
			heartbeatPolicy:  func() (time.Duration, time.Duration, error) { return 20 * time.Minute, 30 * time.Minute, nil },
			operationalState: func() (string, string) { return "DOCKED", "global - synced" },
			now:              func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("%s: stopped controls should be valid observations: %v", rigName, err)
		}
		if out.Witness.Session != expected[0] || out.Refinery.Session != expected[1] {
			t.Fatalf("%s: got %s/%s, want %s/%s", rigName, out.Witness.Session, out.Refinery.Session, expected[0], expected[1])
		}
		if out.Operational || out.Deacon.HeartbeatState != "missing" || out.Mayor.Transport != "none" {
			t.Fatalf("%s: unexpected stopped output: %+v", rigName, out)
		}
	}
}

func TestGatherPatrolControlsACPAndHeartbeatClassifications(t *testing.T) {
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("cy", "canary")
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })

	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		age  time.Duration
		want string
	}{
		"fresh":      {5 * time.Minute, "fresh"},
		"stale":      {25 * time.Minute, "stale"},
		"very_stale": {35 * time.Minute, "very_stale"},
		"future":     {-time.Minute, "future"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := gatherPatrolControls("canary", patrolControlsDeps{
				sessionRunning:   func(string) (bool, error) { return true, nil },
				mayorStatus:      func() (bool, string, error) { return true, "acp", nil },
				heartbeat:        func() *deacon.Heartbeat { return &deacon.Heartbeat{Timestamp: now.Add(-tc.age), Cycle: 7} },
				heartbeatPolicy:  func() (time.Duration, time.Duration, error) { return 20 * time.Minute, 30 * time.Minute, nil },
				operationalState: func() (string, string) { return "OPERATIONAL", "default" },
				now:              func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Mayor.Transport != "acp" || !out.Mayor.Running || out.Deacon.HeartbeatState != tc.want {
				t.Fatalf("unexpected output: %+v", out)
			}
		})
	}
}

func TestGatherPatrolControlsObservationError(t *testing.T) {
	_, err := gatherPatrolControls("canary", patrolControlsDeps{
		sessionRunning:   func(string) (bool, error) { return false, errors.New("tmux unavailable") },
		mayorStatus:      func() (bool, string, error) { return false, "none", nil },
		heartbeat:        func() *deacon.Heartbeat { return nil },
		heartbeatPolicy:  func() (time.Duration, time.Duration, error) { return time.Minute, 2 * time.Minute, nil },
		operationalState: func() (string, string) { return "OPERATIONAL", "default" },
		now:              time.Now,
	})
	if err == nil {
		t.Fatal("observation errors must fail the command")
	}
}
