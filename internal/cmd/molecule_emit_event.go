package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/channelevents"
)

var (
	emitEventChannel string
	emitEventType    string
	emitEventPayload []string
)

var moleculeEmitEventCmd = &cobra.Command{
	Use:   "emit-event",
	Short: "Emit a file-based event on a named channel",
	Long: `Emit an event file to ~/gt/events/<channel>/ for subscribers to pick up.

This is the Go counterpart to emit-event.sh. Events are JSON files consumed
by await-event subscribers (e.g., the refinery watching for MERGE_READY events).

EVENT FORMAT:
Creates a JSON file at ~/gt/events/<channel>/<timestamp>.event:
  {"type": "...", "channel": "...", "timestamp": "...", "payload": {...}}

EXAMPLES:
  # Emit a MERGE_READY event for the refinery
  gt mol step emit-event --channel refinery-gastown --type MERGE_READY \
    --payload rig=gastown --payload source=witness --payload polecat=nux

  # Emit a PATROL_WAKE event
  gt mol step emit-event --channel refinery-gastown --type PATROL_WAKE \
    --payload rig=gastown --payload source=witness --payload queue_depth=3

  # Emit an MQ_SUBMIT event
  gt mol step emit-event --channel refinery-gastown --type MQ_SUBMIT \
    --payload rig=gastown --payload source=sling --payload mr_id=bd-42`,
	RunE: runMoleculeEmitEvent,
}

// EmitEventResult is returned when an event is emitted.
type EmitEventResult struct {
	Path    string `json:"path"`
	Channel string `json:"channel"`
	Type    string `json:"type"`
}

func init() {
	moleculeEmitEventCmd.Flags().StringVar(&emitEventChannel, "channel", "",
		"Event channel name (required, e.g., 'refinery-gastown')")
	moleculeEmitEventCmd.Flags().StringVar(&emitEventType, "type", "",
		"Event type (required, e.g., 'MERGE_READY')")
	moleculeEmitEventCmd.Flags().StringArrayVar(&emitEventPayload, "payload", nil,
		"Payload key=value pairs (repeatable)")
	moleculeEmitEventCmd.Flags().BoolVar(&moleculeJSON, "json", false,
		"Output as JSON")
	_ = moleculeEmitEventCmd.MarkFlagRequired("channel")
	_ = moleculeEmitEventCmd.MarkFlagRequired("type")

	moleculeStepCmd.AddCommand(moleculeEmitEventCmd)
}

func runMoleculeEmitEvent(cmd *cobra.Command, args []string) error {
	if err := channelevents.ValidateRigRolePayload(emitEventChannel, emitEventPayload); err != nil {
		return err
	}
	path, err := channelevents.Emit(emitEventChannel, emitEventType, emitEventPayload)
	if err != nil {
		return err
	}

	if moleculeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(EmitEventResult{
			Path:    path,
			Channel: emitEventChannel,
			Type:    emitEventType,
		})
	}

	fmt.Println(path)
	return nil
}
