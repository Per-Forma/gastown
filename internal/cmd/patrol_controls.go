package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	patrolControlsRig  string
	patrolControlsJSON bool
)

type PatrolControlRoleStatus struct {
	Session string `json:"session"`
	Running bool   `json:"running"`
}

type PatrolMayorControlStatus struct {
	Session   string `json:"session"`
	Running   bool   `json:"running"`
	Transport string `json:"transport"`
}

type PatrolHeartbeatControlStatus struct {
	Timestamp      time.Time `json:"timestamp"`
	AgeSeconds     float64   `json:"age_seconds"`
	Classification string    `json:"classification"`
	Cycle          int64     `json:"cycle"`
	LastAction     string    `json:"last_action,omitempty"`
	PolicyWarning  string    `json:"policy_warning,omitempty"`
}

type PatrolDeaconControlStatus struct {
	Session        string                        `json:"session"`
	Running        bool                          `json:"running"`
	HeartbeatState string                        `json:"heartbeat_state"`
	Heartbeat      *PatrolHeartbeatControlStatus `json:"heartbeat,omitempty"`
}

type PatrolControlsOutput struct {
	Rig               string                    `json:"rig"`
	Operational       bool                      `json:"operational"`
	OperationalState  string                    `json:"operational_state"`
	OperationalSource string                    `json:"operational_source"`
	Witness           PatrolControlRoleStatus   `json:"witness"`
	Refinery          PatrolControlRoleStatus   `json:"refinery"`
	Mayor             PatrolMayorControlStatus  `json:"mayor"`
	Deacon            PatrolDeaconControlStatus `json:"deacon"`
	ObservedAt        time.Time                 `json:"observed_at"`
}

type patrolControlsDeps struct {
	sessionRunning   func(string) (bool, error)
	mayorStatus      func() (bool, string, error)
	heartbeat        func() *deacon.Heartbeat
	heartbeatPolicy  func() (time.Duration, time.Duration, error)
	operationalState func() (string, string)
	now              func() time.Time
}

var patrolControlsCmd = &cobra.Command{
	Use:   "controls",
	Short: "Report canonical control-plane liveness for one rig",
	Long: `Report the registered Witness and Refinery sessions plus Town-level
Mayor and Deacon liveness. This command is read-only. A stopped control agent
is represented in the output and does not make the command fail.`,
	Args: cobra.NoArgs,
	RunE: runPatrolControls,
}

func init() {
	patrolControlsCmd.Flags().StringVar(&patrolControlsRig, "rig", "", "Rig to inspect (required)")
	patrolControlsCmd.Flags().BoolVar(&patrolControlsJSON, "json", false, "Output as JSON")
	_ = patrolControlsCmd.MarkFlagRequired("rig")
	patrolCmd.AddCommand(patrolControlsCmd)
}

func runPatrolControls(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	rigPath := filepath.Join(townRoot, patrolControlsRig)
	info, err := os.Stat(rigPath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("unknown rig %q", patrolControlsRig)
	}
	if _, err := rig.LoadRigConfig(rigPath); err != nil {
		return fmt.Errorf("unknown or invalid rig %q: %w", patrolControlsRig, err)
	}

	t := tmux.NewTmux()
	mayorManager := mayor.NewManager(townRoot)
	deps := patrolControlsDeps{
		sessionRunning: t.HasSession,
		mayorStatus: func() (bool, string, error) {
			status, err := mayorManager.CombinedStatus()
			if err != nil {
				return false, "none", err
			}
			transport := "none"
			switch status.Mode {
			case mayor.ModeACP, mayor.ModeBoth:
				transport = "acp"
			case mayor.ModeTMUX:
				transport = "tmux"
			}
			return status.Active, transport, nil
		},
		heartbeat: func() *deacon.Heartbeat { return deacon.ReadHeartbeat(townRoot) },
		heartbeatPolicy: func() (time.Duration, time.Duration, error) {
			return config.LoadOperationalConfig(townRoot).GetDeaconConfig().HeartbeatThresholdsD()
		},
		operationalState: func() (string, string) {
			return getRigOperationalState(townRoot, patrolControlsRig)
		},
		now: time.Now,
	}

	out, err := gatherPatrolControls(patrolControlsRig, deps)
	if err != nil {
		return err
	}
	if patrolControlsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Printf("Rig: %s (%s)\n", out.Rig, out.OperationalState)
	fmt.Printf("Witness:  %s running=%t\n", out.Witness.Session, out.Witness.Running)
	fmt.Printf("Refinery: %s running=%t\n", out.Refinery.Session, out.Refinery.Running)
	fmt.Printf("Mayor:    %s running=%t transport=%s\n", out.Mayor.Session, out.Mayor.Running, out.Mayor.Transport)
	fmt.Printf("Deacon:   %s running=%t heartbeat=%s\n", out.Deacon.Session, out.Deacon.Running, out.Deacon.HeartbeatState)
	return nil
}

func gatherPatrolControls(rigName string, deps patrolControlsDeps) (*PatrolControlsOutput, error) {
	prefix := session.PrefixFor(rigName)
	witnessSession := session.WitnessSessionName(prefix)
	refinerySession := session.RefinerySessionName(prefix)
	deaconSession := session.DeaconSessionName()

	witnessRunning, err := deps.sessionRunning(witnessSession)
	if err != nil {
		return nil, fmt.Errorf("checking witness session %s: %w", witnessSession, err)
	}
	refineryRunning, err := deps.sessionRunning(refinerySession)
	if err != nil {
		return nil, fmt.Errorf("checking refinery session %s: %w", refinerySession, err)
	}
	deaconRunning, err := deps.sessionRunning(deaconSession)
	if err != nil {
		return nil, fmt.Errorf("checking deacon session %s: %w", deaconSession, err)
	}
	mayorRunning, mayorTransport, err := deps.mayorStatus()
	if err != nil {
		return nil, fmt.Errorf("checking mayor status: %w", err)
	}

	state, source := deps.operationalState()
	out := &PatrolControlsOutput{
		Rig:               rigName,
		Operational:       state == "OPERATIONAL",
		OperationalState:  state,
		OperationalSource: source,
		Witness:           PatrolControlRoleStatus{Session: witnessSession, Running: witnessRunning},
		Refinery:          PatrolControlRoleStatus{Session: refinerySession, Running: refineryRunning},
		Mayor:             PatrolMayorControlStatus{Session: session.MayorSessionName(), Running: mayorRunning, Transport: mayorTransport},
		Deacon:            PatrolDeaconControlStatus{Session: deaconSession, Running: deaconRunning, HeartbeatState: "missing"},
		ObservedAt:        deps.now().UTC(),
	}

	if hb := deps.heartbeat(); hb != nil {
		staleAfter, veryStaleAfter, policyErr := deps.heartbeatPolicy()
		age := deps.now().Sub(hb.Timestamp)
		classification := "fresh"
		switch {
		case age < 0:
			classification = "future"
		case age >= veryStaleAfter:
			classification = "very_stale"
		case age >= staleAfter:
			classification = "stale"
		}
		warning := ""
		if policyErr != nil {
			warning = policyErr.Error()
		}
		out.Deacon.Heartbeat = &PatrolHeartbeatControlStatus{
			Timestamp:      hb.Timestamp,
			AgeSeconds:     age.Seconds(),
			Classification: classification,
			Cycle:          hb.Cycle,
			LastAction:     hb.LastAction,
			PolicyWarning:  warning,
		}
		out.Deacon.HeartbeatState = classification
	}
	return out, nil
}
