package cmd

import "testing"

func TestReconcileRigWorkSignalsUsesWorkingAgentState(t *testing.T) {
	status := RigStatus{
		Agents: []AgentRuntime{{
			Address: "gastown/rust",
			State:   "working",
		}},
		Hooks: []AgentHookInfo{{
			Agent: "gastown/rust",
		}},
	}
	reconcileRigWorkSignals(&status)
	if !status.Agents[0].HasWork || !status.Hooks[0].HasWork {
		t.Fatalf("working assignment not reconciled: %+v", status)
	}
}
