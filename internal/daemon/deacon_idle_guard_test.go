package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

func writeDeferredModelWatchdogState(t *testing.T, townRoot, status string, checkedAt time.Time) {
	t.Helper()
	stateDir := filepath.Join(townRoot, "deacon")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create watchdog state dir: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"status":                       status,
		"checked_at":                   checkedAt,
		"last_capability_probe_result": "deferred",
	})
	if err != nil {
		t.Fatalf("marshal watchdog state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "lmstudio-watchdog.json"), data, 0o600); err != nil {
		t.Fatalf("write watchdog state: %v", err)
	}
}

// writeFakeTmuxWithSession creates a fake tmux binary that reports the Deacon
// session as existing (has-session returns 0). Used for deacon idle guard tests
// where the session must be present so checkDeaconHeartbeat reaches the nudge path.
func writeFakeTmuxWithSession(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail

cmd=""
skip_next=0
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    skip_next=0
    continue
  fi
  if [[ "$arg" == "-u" ]]; then
    continue
  fi
  if [[ "$arg" == "-L" ]]; then
    skip_next=1
    continue
  fi
  cmd="$arg"
  break
done

if [[ -n "${TMUX_LOG:-}" ]]; then
  printf "%s %s\n" "$cmd" "$*" >> "$TMUX_LOG"
fi

if [[ "${1:-}" == "-V" ]]; then
  echo "tmux 3.3a"
  exit 0
fi

# Session exists: has-session returns 0 so the nudge path is reachable.
if [[ "$cmd" == "has-session" ]]; then
  exit 0
fi

exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

// TestCheckDeaconHeartbeat_MechanicalOnly verifies that the stale band never
// spends a model turn, regardless of active-work or store state.
func TestCheckDeaconHeartbeat_MechanicalOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	tests := []struct {
		name           string
		heartbeatAge   time.Duration
		stores         map[string]beadsdk.Storage
		wantStaleLog   bool
		wantRestartLog bool
	}{
		{
			name:         "fresh boundary remains silent",
			heartbeatAge: 19*time.Minute + 59*time.Second,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
		},
		{
			name:         "stale idle town logs transition without nudge",
			heartbeatAge: 20 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantStaleLog: true,
		},
		{
			name:         "stale active work logs transition without nudge",
			heartbeatAge: 25 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "sc-abc"}},
				}},
			},
			wantStaleLog: true,
		},
		{
			name:         "stale store error still does not nudge",
			heartbeatAge: 25 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{err: fmt.Errorf("db offline")},
			},
			wantStaleLog: true,
		},
		{
			name:         "very stale enters restart path",
			heartbeatAge: 30 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantRestartLog: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			fakeBinDir := t.TempDir()
			tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
			if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
				t.Fatalf("create tmux log: %v", err)
			}

			writeFakeTmuxWithSession(t, fakeBinDir)
			t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMUX_LOG", tmuxLog)

			writeDeaconHeartbeat(t, townRoot, tc.heartbeatAge)

			d := newTestDaemonWithStores(t, townRoot, tc.stores)
			if tc.wantRestartLog {
				rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
				rt.state.Agents["deacon"] = &AgentRestartInfo{BackoffUntil: time.Now().Add(time.Hour)}
				d.restartTracker = rt
			}

			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			d.checkDeaconHeartbeat()

			logOutput := logBuf.String()

			if strings.Contains(logOutput, "HEALTH_CHECK") || strings.Contains(logOutput, "nudging session") {
				t.Fatalf("stale heartbeat emitted a model nudge:\n%s", logOutput)
			}
			if got := strings.Contains(logOutput, "entered stale band"); got != tc.wantStaleLog {
				t.Errorf("stale transition log present=%v, want=%v\nlog:\n%s", got, tc.wantStaleLog, logOutput)
			}
			if got := strings.Contains(logOutput, "STUCK DEACON"); got != tc.wantRestartLog {
				t.Errorf("restart log present=%v, want=%v\nlog:\n%s", got, tc.wantRestartLog, logOutput)
			}
			if tc.wantStaleLog {
				d.checkDeaconHeartbeat()
				if got := strings.Count(logBuf.String(), "entered stale band"); got != 1 {
					t.Errorf("stale transition logged %d times, want exactly once", got)
				}
			}
		})
	}
}

func TestCheckDeaconHeartbeat_DefersDestructiveRestartForFreshMLXLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	tests := []struct {
		name            string
		watchdogStatus  string
		watchdogAge     time.Duration
		postStart       bool
		wantDeferredLog string
		wantRestart     bool
	}{
		{
			name:            "very stale heartbeat is preserved while healthy watchdog reports busy",
			watchdogStatus:  "healthy",
			watchdogAge:     30 * time.Second,
			wantDeferredLog: "very stale but MLX capability verification is deferred",
		},
		{
			name:            "post-start heartbeat is preserved while capability-stale watchdog reports busy",
			watchdogStatus:  "capability-stale",
			watchdogAge:     30 * time.Second,
			postStart:       true,
			wantDeferredLog: "post-start heartbeat deferred by active MLX inference load",
		},
		{
			name:           "stale watchdog evidence does not suppress restart",
			watchdogStatus: "healthy",
			watchdogAge:    3 * time.Minute,
			postStart:      true,
			wantRestart:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			fakeBinDir := t.TempDir()
			tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
			if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
				t.Fatalf("create tmux log: %v", err)
			}
			writeFakeTmuxWithSession(t, fakeBinDir)
			t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMUX_LOG", tmuxLog)

			writeDeaconHeartbeat(t, townRoot, 30*time.Minute)
			writeDeferredModelWatchdogState(t, townRoot, tc.watchdogStatus, time.Now().Add(-tc.watchdogAge))

			d := newTestDaemonWithStores(t, townRoot, map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			})
			if tc.postStart {
				d.deaconLastStarted = time.Now().Add(-6 * time.Minute)
			}
			rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
			rt.state.Agents["deacon"] = &AgentRestartInfo{BackoffUntil: time.Now().Add(time.Hour)}
			d.restartTracker = rt

			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)
			d.checkDeaconHeartbeat()

			logOutput := logBuf.String()
			if tc.wantDeferredLog != "" && !strings.Contains(logOutput, tc.wantDeferredLog) {
				t.Fatalf("missing deferral log %q:\n%s", tc.wantDeferredLog, logOutput)
			}
			if got := strings.Contains(logOutput, "STUCK DEACON"); got != tc.wantRestart {
				t.Fatalf("restart log present=%v, want=%v:\n%s", got, tc.wantRestart, logOutput)
			}
		})
	}
}
