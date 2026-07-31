package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
)

const activeWorkRecoveryStateVersion = 1

type activeWorkItem struct {
	ID        string
	Status    string
	Assignee  string
	UpdatedAt time.Time
}

func (i activeWorkItem) fingerprintPart() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", i.Status, i.ID, i.Assignee, i.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

type activeWorkInventory struct {
	ByRig       map[string][]activeWorkItem
	Unrouteable []activeWorkItem
}

type activeWorkRecoveryState struct {
	Version int                            `json:"version"`
	Rigs    map[string]*activeWorkRigState `json:"rigs"`
}

type activeWorkRigState struct {
	Fingerprint              string    `json:"fingerprint,omitempty"`
	LastAttempt              time.Time `json:"last_attempt,omitempty"`
	LastSuccess              time.Time `json:"last_success,omitempty"`
	LastOutcome              string    `json:"last_outcome,omitempty"`
	ConsecutiveFailures      int       `json:"consecutive_failures,omitempty"`
	LastFailure              string    `json:"last_failure,omitempty"`
	LastEscalatedFingerprint string    `json:"last_escalated_fingerprint,omitempty"`
}

type patrolScanSummary struct {
	Zombies struct {
		Found   int      `json:"found"`
		Errors  []string `json:"errors,omitempty"`
		Zombies []struct {
			Error string `json:"error,omitempty"`
		} `json:"zombies,omitempty"`
	} `json:"zombies"`
	Stalls struct {
		Found  int      `json:"found"`
		Errors []string `json:"errors,omitempty"`
		Stalls []struct {
			Error string `json:"error,omitempty"`
		} `json:"stalls,omitempty"`
	} `json:"stalls"`
	Completions struct {
		Found     int      `json:"found"`
		Errors    []string `json:"errors,omitempty"`
		Completed []struct {
			Error string `json:"error,omitempty"`
		} `json:"completed,omitempty"`
	} `json:"completions"`
	Orphans struct {
		Found   int      `json:"found"`
		Errors  []string `json:"errors,omitempty"`
		Orphans []struct {
			Recovered bool   `json:"recovered"`
			Error     string `json:"error,omitempty"`
		} `json:"orphans,omitempty"`
	} `json:"orphans"`
	Errors []string `json:"errors,omitempty"`
}

func (s *patrolScanSummary) unresolvedErrors() []string {
	errs := append([]string{}, s.Errors...)
	errs = append(errs, s.Zombies.Errors...)
	errs = append(errs, s.Stalls.Errors...)
	errs = append(errs, s.Completions.Errors...)
	errs = append(errs, s.Orphans.Errors...)
	for _, zombie := range s.Zombies.Zombies {
		if zombie.Error != "" {
			errs = append(errs, zombie.Error)
		}
	}
	for _, stall := range s.Stalls.Stalls {
		if stall.Error != "" {
			errs = append(errs, stall.Error)
		}
	}
	for _, completion := range s.Completions.Completed {
		if completion.Error != "" {
			errs = append(errs, completion.Error)
		}
	}
	for _, orphan := range s.Orphans.Orphans {
		if orphan.Error != "" {
			errs = append(errs, orphan.Error)
		}
	}
	return errs
}

func (s *patrolScanSummary) outcome() string {
	actions := s.Zombies.Found + s.Stalls.Found + s.Completions.Found + s.Orphans.Found
	if actions == 0 {
		return "healthy"
	}
	return fmt.Sprintf("recovered:%d", actions)
}

type activeWorkRecovery struct {
	mu        sync.Mutex
	townRoot  string
	gtPath    string
	logger    *log.Logger
	ctx       context.Context
	interval  time.Duration
	timeout   time.Duration
	semaphore chan struct{}
	now       func() time.Time
	state     activeWorkRecoveryState
	inFlight  map[string]bool
	runScan   func(context.Context, string) (*patrolScanSummary, error)
	sendMail  func(to, subject, body string) error
}

func newActiveWorkRecovery(townRoot, gtPath string, logger *log.Logger, ctx context.Context, daemonConfig *config.DaemonThresholds) *activeWorkRecovery {
	if ctx == nil {
		ctx = context.Background()
	}
	r := &activeWorkRecovery{
		townRoot:  townRoot,
		gtPath:    gtPath,
		logger:    logger,
		ctx:       ctx,
		interval:  daemonConfig.ActiveWorkScanIntervalD(),
		timeout:   daemonConfig.ActiveWorkScanTimeoutD(),
		semaphore: make(chan struct{}, daemonConfig.ActiveWorkScanConcurrencyV()),
		now:       time.Now,
		state: activeWorkRecoveryState{
			Version: activeWorkRecoveryStateVersion,
			Rigs:    make(map[string]*activeWorkRigState),
		},
		inFlight: make(map[string]bool),
	}
	r.runScan = r.runPatrolScan
	r.sendMail = r.sendRecoveryMail
	if err := r.load(); err != nil && logger != nil {
		logger.Printf("Active-work recovery state could not be loaded; starting fresh: %v", err)
	}
	return r
}

func (r *activeWorkRecovery) statePath() string {
	return filepath.Join(r.townRoot, "deacon", "active-work-recovery.json")
}

func (r *activeWorkRecovery) load() error {
	data, err := os.ReadFile(r.statePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state activeWorkRecoveryState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Version != activeWorkRecoveryStateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.Rigs == nil {
		state.Rigs = make(map[string]*activeWorkRigState)
	}
	r.state = state
	return nil
}

func (r *activeWorkRecovery) saveLocked() error {
	return atomicfile.EnsureDirAndWriteJSONWithPerm(r.statePath(), &r.state, 0o600)
}

func activeWorkFingerprint(items []activeWorkItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.fingerprintPart())
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func (r *activeWorkRecovery) schedule(inventory activeWorkInventory) {
	now := r.now()
	current := make(map[string]bool, len(inventory.ByRig))

	r.mu.Lock()
	for rigName, items := range inventory.ByRig {
		if len(items) == 0 {
			continue
		}
		current[rigName] = true
		fingerprint := activeWorkFingerprint(items)
		state := r.state.Rigs[rigName]
		if state == nil {
			state = &activeWorkRigState{}
			r.state.Rigs[rigName] = state
		}
		changed := state.Fingerprint != fingerprint
		if changed {
			state.Fingerprint = fingerprint
			state.ConsecutiveFailures = 0
			state.LastFailure = ""
			state.LastEscalatedFingerprint = ""
		}
		if r.inFlight[rigName] || (!changed && !state.LastAttempt.IsZero() && now.Sub(state.LastAttempt) < r.interval) {
			continue
		}
		state.LastAttempt = now
		state.LastOutcome = "scheduled"
		r.inFlight[rigName] = true
		_ = r.saveLocked()
		go r.execute(rigName, fingerprint)
	}
	for rigName, state := range r.state.Rigs {
		if strings.HasPrefix(rigName, "_") || current[rigName] || r.inFlight[rigName] {
			continue
		}
		if state.Fingerprint != "" {
			state.Fingerprint = ""
			state.ConsecutiveFailures = 0
			state.LastFailure = ""
			state.LastEscalatedFingerprint = ""
			state.LastOutcome = "idle"
		}
	}
	_ = r.saveLocked()
	r.mu.Unlock()

	r.recordUnrouteable(inventory.Unrouteable)
}

func (r *activeWorkRecovery) execute(rigName, fingerprint string) {
	select {
	case r.semaphore <- struct{}{}:
		defer func() { <-r.semaphore }()
	case <-r.ctx.Done():
		r.finish(rigName, fingerprint, nil, r.ctx.Err())
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	summary, err := r.runScan(ctx, rigName)
	r.finish(rigName, fingerprint, summary, err)
}

func (r *activeWorkRecovery) finish(rigName, fingerprint string, summary *patrolScanSummary, scanErr error) {
	now := r.now()
	failure := ""
	if scanErr != nil {
		failure = scanErr.Error()
	} else if summary == nil {
		failure = "patrol scan returned no result"
	} else if errs := summary.unresolvedErrors(); len(errs) > 0 {
		failure = strings.Join(errs, "; ")
	}

	var escalate bool
	r.mu.Lock()
	delete(r.inFlight, rigName)
	state := r.state.Rigs[rigName]
	if state == nil || state.Fingerprint != fingerprint {
		_ = r.saveLocked()
		r.mu.Unlock()
		return
	}
	if failure == "" {
		state.LastSuccess = now
		state.LastOutcome = summary.outcome()
		state.ConsecutiveFailures = 0
		state.LastFailure = ""
	} else {
		if state.LastFailure == failure {
			state.ConsecutiveFailures++
		} else {
			state.ConsecutiveFailures = 1
		}
		state.LastFailure = failure
		state.LastOutcome = "failed"
		escalationFingerprint := fingerprint + "|" + failure
		if state.ConsecutiveFailures >= 2 && state.LastEscalatedFingerprint != escalationFingerprint {
			state.LastEscalatedFingerprint = escalationFingerprint
			escalate = true
		}
	}
	_ = r.saveLocked()
	r.mu.Unlock()

	if failure == "" {
		if r.logger != nil {
			r.logger.Printf("Mechanical active-work recovery for %s completed: %s", rigName, summary.outcome())
		}
		return
	}
	if r.logger != nil {
		r.logger.Printf("Mechanical active-work recovery for %s failed: %s", rigName, failure)
	}
	if escalate {
		subject := fmt.Sprintf("MECHANICAL_RECOVERY_FAILED: %s", rigName)
		body := fmt.Sprintf("The daemon's model-free patrol scan failed twice for the same active work.\n\nRig: %s\nFingerprint: %s\nFailure: %s\n\nInspect with: gt patrol scan --rig %s --json", rigName, fingerprint, failure, rigName)
		if err := r.sendMail(rigName+"/witness", subject, body); err != nil {
			if r.logger != nil {
				r.logger.Printf("Failed to send active-work recovery escalation for %s: %v", rigName, err)
			}
			r.clearEscalationMarker(rigName, fingerprint+"|"+failure)
		}
	}
}

func (r *activeWorkRecovery) recordUnrouteable(items []activeWorkItem) {
	const stateKey = "_unrouteable"
	if len(items) == 0 {
		r.mu.Lock()
		if state := r.state.Rigs[stateKey]; state != nil && state.Fingerprint != "" {
			state.Fingerprint = ""
			state.ConsecutiveFailures = 0
			state.LastOutcome = "idle"
			state.LastEscalatedFingerprint = ""
			_ = r.saveLocked()
		}
		r.mu.Unlock()
		return
	}

	fingerprint := activeWorkFingerprint(items)
	var escalate bool
	r.mu.Lock()
	state := r.state.Rigs[stateKey]
	if state == nil {
		state = &activeWorkRigState{}
		r.state.Rigs[stateKey] = state
	}
	if state.Fingerprint != fingerprint {
		state.Fingerprint = fingerprint
		state.ConsecutiveFailures = 1
		state.LastEscalatedFingerprint = ""
	} else {
		state.ConsecutiveFailures++
	}
	state.LastAttempt = r.now()
	state.LastOutcome = "unrouteable"
	if state.ConsecutiveFailures >= 2 && state.LastEscalatedFingerprint != fingerprint {
		state.LastEscalatedFingerprint = fingerprint
		escalate = true
	}
	_ = r.saveLocked()
	r.mu.Unlock()

	if escalate {
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		subject := "ACTIVE_WORK_UNROUTABLE"
		body := fmt.Sprintf("Active work could not be mapped to a responsible polecat rig after two daemon scans.\n\nIssues: %s\nFingerprint: %s", strings.Join(ids, ", "), fingerprint)
		if err := r.sendMail("mayor/", subject, body); err != nil {
			if r.logger != nil {
				r.logger.Printf("Failed to send unrouteable-work escalation: %v", err)
			}
			r.clearEscalationMarker(stateKey, fingerprint)
		}
	}
}

func (r *activeWorkRecovery) clearEscalationMarker(rigName, fingerprint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.state.Rigs[rigName]; state != nil && state.LastEscalatedFingerprint == fingerprint {
		state.LastEscalatedFingerprint = ""
		_ = r.saveLocked()
	}
}

func (r *activeWorkRecovery) runPatrolScan(ctx context.Context, rigName string) (*patrolScanSummary, error) {
	cmd := exec.CommandContext(ctx, r.gtPath, "patrol", "scan", "--rig", rigName, "--json", "--notify=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("patrol scan timed out after %s: %w", r.timeout, ctx.Err())
		}
		return nil, fmt.Errorf("patrol scan failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var summary patrolScanSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		return nil, fmt.Errorf("parsing patrol scan JSON: %w", err)
	}
	return &summary, nil
}

func (r *activeWorkRecovery) sendRecoveryMail(to, subject, body string) error {
	router := mail.NewRouter(r.townRoot)
	msg := &mail.Message{
		From:     "deacon/",
		To:       to,
		Subject:  subject,
		Body:     body,
		Priority: mail.PriorityHigh,
		Type:     mail.TypeEscalation,
	}
	if err := router.Send(msg); err != nil {
		return err
	}
	router.WaitPendingNotifications()
	return nil
}

func (d *Daemon) inventoryActiveWork() activeWorkInventory {
	inventory := activeWorkInventory{ByRig: make(map[string][]activeWorkItem)}
	stores := d.beadsStores
	if len(stores) == 0 {
		if reopened, err := d.openBeadsStores(); err == nil && len(reopened) > 0 {
			d.beadsStores = reopened
			stores = reopened
		}
	}
	if len(stores) == 0 {
		for _, rigName := range d.getKnownRigs() {
			inventory.ByRig[rigName] = []activeWorkItem{{ID: "inventory-unavailable", Status: "error", Assignee: rigName}}
		}
		return inventory
	}

	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	for storeName, store := range stores {
		for _, statusName := range []string{"in_progress", "hooked"} {
			status := beadsdk.Status(statusName)
			issues, err := store.SearchIssues(ctx, "", beadsdk.IssueFilter{Status: &status, Limit: 0})
			if err != nil {
				targets := []string{storeName}
				if storeName == "hq" {
					targets = d.getKnownRigs()
				}
				for _, rigName := range targets {
					if rigName == "hq" || rigName == "" {
						continue
					}
					inventory.ByRig[rigName] = append(inventory.ByRig[rigName], activeWorkItem{ID: "inventory-error:" + storeName, Status: statusName, Assignee: rigName})
				}
				continue
			}
			for _, issue := range issues {
				if issue == nil || issue.Ephemeral || issue.WispType != "" {
					continue
				}
				item := activeWorkItem{ID: issue.ID, Status: statusName, Assignee: issue.Assignee, UpdatedAt: issue.UpdatedAt}
				rigName, polecatAssigned, controlAssigned := routeActiveWork(storeName, issue.Assignee)
				switch {
				case controlAssigned:
					continue
				case polecatAssigned || (rigName != "" && storeName != "hq"):
					inventory.ByRig[rigName] = append(inventory.ByRig[rigName], item)
				case issue.Assignee != "" && strings.Contains(issue.Assignee, "polecat"):
					inventory.Unrouteable = append(inventory.Unrouteable, item)
				}
			}
		}
	}
	return inventory
}

func routeActiveWork(storeName, assignee string) (rigName string, polecatAssigned, controlAssigned bool) {
	parts := strings.Split(assignee, "/")
	if len(parts) == 3 && parts[0] != "" && parts[1] == "polecats" && parts[2] != "" {
		return parts[0], true, false
	}
	if len(parts) >= 2 {
		role := parts[1]
		if role == "witness" || role == "refinery" || role == "crew" || role == "mayor" || role == "deacon" || role == "dogs" {
			return "", false, true
		}
	}
	if storeName != "hq" {
		return storeName, false, false
	}
	return "", false, false
}

func (d *Daemon) scheduleActiveWorkRecovery() {
	if d.activeWorkRecovery == nil {
		d.activeWorkRecovery = newActiveWorkRecovery(d.config.TownRoot, d.gtPath, d.logger, d.ctx, d.loadOperationalConfig().GetDaemonConfig())
	}
	d.activeWorkRecovery.schedule(d.inventoryActiveWork())
}
