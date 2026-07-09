package builders

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
)

func TestFirehoseBuilder_Creation(t *testing.T) {
	cfg := &config.Config{
		Global: config.GlobalConfig{
			FirehoseCriticalChannel: "C_CRIT",
			FirehoseWarningChannel:  "C_WARN",
		},
	}
	b := NewFirehoseJobBuilder(cfg)

	// Case 1: Critical
	agCrit := &model.AlertGroup{ID: "ag1", Severity: "critical", DedupKey: "dk1"}
	job, _, steps, err := b.Build(agCrit)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if job == nil {
		t.Fatal("Job should not be nil")
	}
	if *job.DedupKey != "firehose_dk1" {
		t.Errorf("Expected dedup key firehose_dk1, got %s", *job.DedupKey)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}
	if steps[0].StepType != "firehose" {
		t.Errorf("Expected step type firehose, got %s", steps[0].StepType)
	}
	var data model.EscalationStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if data.TargetID != "C_CRIT" {
		t.Errorf("Expected target C_CRIT, got %s", data.TargetID)
	}

	// Case 2: Warning
	agWarn := &model.AlertGroup{ID: "ag2", Severity: "warning", DedupKey: "dk2"}
	job, _, steps, err = b.Build(agWarn)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if job == nil {
		t.Fatal("Job should not be nil")
	}
	if *job.DedupKey != "firehose_dk2" {
		t.Errorf("Expected dedup key firehose_dk2, got %s", *job.DedupKey)
	}
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if data.TargetID != "C_WARN" {
		t.Errorf("Expected target C_WARN, got %s", data.TargetID)
	}
}

func TestFirehoseBuilder_Disabled(t *testing.T) {
	// Only Critical configured
	cfg := &config.Config{
		Global: config.GlobalConfig{
			FirehoseCriticalChannel: "C_CRIT",
			// Warning empty
		},
	}
	b := NewFirehoseJobBuilder(cfg)

	// Warning AG -> Should be nil
	agWarn := &model.AlertGroup{ID: "ag3", Severity: "warning"}
	job, _, _, err := b.Build(agWarn)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if job != nil {
		t.Error("Job should be nil when channel not configured")
	}
}
