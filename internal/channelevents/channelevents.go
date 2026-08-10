// Package channelevents provides file-based event emission for named channels.
//
// Channel events are JSON files written to ~/gt/events/<channel>/*.event
// and consumed by await-event subscribers (e.g., the refinery watching for
// MERGE_READY events). This is distinct from the activity feed events in
// the events package (~/gt/.events.jsonl).
package channelevents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/steveyegge/gastown/internal/workspace"
)

// ValidChannelName restricts channel names to safe characters (no path traversal).
var ValidChannelName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	RoleWitness  = "witness"
	RoleRefinery = "refinery"
)

// emitSeq is an atomic counter to ensure unique event filenames even when
// time.Now().UnixNano() has low resolution.
var emitSeq atomic.Uint64

// Emit creates an event file in the channel directory, resolving the town
// root from the current working directory.
func Emit(channel, eventType string, payloadPairs []string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		home, _ := os.UserHomeDir()
		townRoot = filepath.Join(home, "gt")
	}
	eventDir := filepath.Join(townRoot, "events", channel)
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return "", fmt.Errorf("creating event directory: %w", err)
	}

	return emitToDir(eventDir, channel, eventType, payloadPairs)
}

// EmitToTown creates an event file using an explicit town root.
// Used by internal callers that already know the town root.
func EmitToTown(townRoot, channel, eventType string, payloadPairs []string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}

	eventDir := filepath.Join(townRoot, "events", channel)
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return "", fmt.Errorf("creating event directory: %w", err)
	}
	return emitToDir(eventDir, channel, eventType, payloadPairs)
}

// RigRoleChannel returns the single-consumer event channel for a rig role.
// Rig-scoped channels prevent one rig's patrol agent from consuming an event
// intended for another rig.
func RigRoleChannel(role, rigName string) (string, error) {
	if role != RoleWitness && role != RoleRefinery {
		return "", fmt.Errorf("invalid rig event role %q: want witness or refinery", role)
	}
	if rigName == "" || !ValidChannelName.MatchString(rigName) {
		return "", fmt.Errorf("invalid rig name %q: must match [a-zA-Z0-9_-]+", rigName)
	}
	return role + "-" + rigName, nil
}

// EmitRigRoleToTown emits a role event with mandatory rig and source metadata.
// Canonical values are appended last so callers cannot override them.
func EmitRigRoleToTown(townRoot, role, rigName, eventType, source string, payloadPairs []string) (string, error) {
	if source == "" {
		return "", fmt.Errorf("rig event source is required")
	}
	channel, err := RigRoleChannel(role, rigName)
	if err != nil {
		return "", err
	}
	payload := append([]string(nil), payloadPairs...)
	payload = append(payload, "rig="+rigName, "source="+source)
	return EmitToTown(townRoot, channel, eventType, payload)
}

// ValidateRigRolePayload enforces the public emit-event contract for
// witness-<rig> and refinery-<rig> channels. Other channels are unchanged.
func ValidateRigRolePayload(channel string, payloadPairs []string) error {
	role := ""
	rigName := ""
	for _, candidate := range []string{RoleWitness, RoleRefinery} {
		prefix := candidate + "-"
		if strings.HasPrefix(channel, prefix) {
			role = candidate
			rigName = strings.TrimPrefix(channel, prefix)
			break
		}
	}
	if role == "" {
		return nil
	}
	canonical, err := RigRoleChannel(role, rigName)
	if err != nil || canonical != channel {
		return fmt.Errorf("invalid rig role channel %q", channel)
	}
	payload := make(map[string]string)
	for _, pair := range payloadPairs {
		key, value, found := strings.Cut(pair, "=")
		if found {
			payload[key] = value
		}
	}
	if payload["rig"] != rigName {
		return fmt.Errorf("channel %q requires payload rig=%s", channel, rigName)
	}
	if payload["source"] == "" {
		return fmt.Errorf("channel %q requires a non-empty source payload", channel)
	}
	return nil
}

// emitToDir writes an event file to the given directory.
func emitToDir(eventDir, channel, eventType string, payloadPairs []string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}

	payload := make(map[string]string)
	for _, pair := range payloadPairs {
		key, val, found := strings.Cut(pair, "=")
		if found {
			payload[key] = val
		}
	}

	now := time.Now()
	event := map[string]interface{}{
		"type":      eventType,
		"channel":   channel,
		"timestamp": now.Format(time.RFC3339),
		"payload":   payload,
	}

	data, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling event: %w", err)
	}

	seq := emitSeq.Add(1)
	eventFile := filepath.Join(eventDir, fmt.Sprintf("%d-%d-%d.event", now.UnixNano(), seq, os.Getpid()))
	if err := os.WriteFile(eventFile, data, 0644); err != nil {
		return "", fmt.Errorf("writing event file: %w", err)
	}

	return eventFile, nil
}
