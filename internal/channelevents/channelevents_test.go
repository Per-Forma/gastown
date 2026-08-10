package channelevents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitToTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	path, err := EmitToTown(townRoot, "refinery", "MERGE_READY", []string{
		"source=witness",
		"rig=dashboard",
	})
	if err != nil {
		t.Fatalf("EmitToTown failed: %v", err)
	}

	if !strings.HasSuffix(path, ".event") {
		t.Errorf("expected .event suffix, got %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading event file: %v", err)
	}

	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("unmarshaling event: %v", err)
	}

	if event["type"] != "MERGE_READY" {
		t.Errorf("type = %v, want MERGE_READY", event["type"])
	}
	if event["channel"] != "refinery" {
		t.Errorf("channel = %v, want refinery", event["channel"])
	}

	payload, ok := event["payload"].(map[string]interface{})
	if !ok {
		t.Fatal("payload is not a map")
	}
	if payload["source"] != "witness" {
		t.Errorf("payload.source = %v, want witness", payload["source"])
	}
	if payload["rig"] != "dashboard" {
		t.Errorf("payload.rig = %v, want dashboard", payload["rig"])
	}
}

func TestEmitToTown_InvalidChannel(t *testing.T) {
	t.Parallel()
	_, err := EmitToTown(t.TempDir(), "../escape", "TEST", nil)
	if err == nil {
		t.Error("expected error for invalid channel name")
	}
}

func TestEmitToTown_UniqueFilenames(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	seen := make(map[string]bool)

	for i := 0; i < 10; i++ {
		path, err := EmitToTown(townRoot, "test", "EVENT", nil)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if seen[path] {
			t.Errorf("duplicate filename: %s", path)
		}
		seen[path] = true
	}
}

func TestValidChannelName(t *testing.T) {
	t.Parallel()
	valid := []string{"refinery", "witness", "my-channel", "test_chan", "abc123"}
	for _, name := range valid {
		if !ValidChannelName.MatchString(name) {
			t.Errorf("%q should be valid", name)
		}
	}

	invalid := []string{"../escape", "has space", "has/slash", "", "has.dot"}
	for _, name := range invalid {
		if ValidChannelName.MatchString(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestEmitToTown_CreatesDirectory(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	channelDir := filepath.Join(townRoot, "events", "newchannel")

	if _, err := os.Stat(channelDir); !os.IsNotExist(err) {
		t.Fatal("channel dir should not exist yet")
	}

	_, err := EmitToTown(townRoot, "newchannel", "TEST", nil)
	if err != nil {
		t.Fatalf("EmitToTown failed: %v", err)
	}

	if _, err := os.Stat(channelDir); err != nil {
		t.Errorf("channel dir should exist after emit: %v", err)
	}
}

func TestEmitRigRoleToTown_IsolatesRigsAndRequiresMetadata(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	canaryPath, err := EmitRigRoleToTown(townRoot, RoleRefinery, "canary", "MERGE_READY", "witness", []string{
		"rig=wrong",
		"source=wrong",
		"mr=cy-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	shortenerPath, err := EmitRigRoleToTown(townRoot, RoleRefinery, "shortener", "MQ_SUBMIT", "sling", nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(canaryPath) == filepath.Dir(shortenerPath) {
		t.Fatal("different rigs must not share a channel directory")
	}
	if got, want := filepath.Base(filepath.Dir(canaryPath)), "refinery-canary"; got != want {
		t.Fatalf("canary channel = %q, want %q", got, want)
	}

	data, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Channel string            `json:"channel"`
		Payload map[string]string `json:"payload"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Channel != "refinery-canary" || event.Payload["rig"] != "canary" || event.Payload["source"] != "witness" {
		t.Fatalf("unexpected rig event: %+v", event)
	}
	if _, err := EmitRigRoleToTown(townRoot, RoleWitness, "canary", "TEST", "", nil); err == nil {
		t.Fatal("missing source must fail")
	}
}

func TestValidateRigRolePayload(t *testing.T) {
	t.Parallel()
	valid := []string{"rig=canary", "source=witness"}
	if err := ValidateRigRolePayload("refinery-canary", valid); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	for _, tc := range [][]string{
		{"source=witness"},
		{"rig=shortener", "source=witness"},
		{"rig=canary"},
	} {
		if err := ValidateRigRolePayload("refinery-canary", tc); err == nil {
			t.Fatalf("invalid payload accepted: %v", tc)
		}
	}
	if err := ValidateRigRolePayload("legacy-channel", nil); err != nil {
		t.Fatalf("unrelated channels must remain valid: %v", err)
	}
}
