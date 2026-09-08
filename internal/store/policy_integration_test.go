package store

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

func TestEscalationPolicy_MessagePersistence(t *testing.T) {
	if testStore == nil {
		t.Skip("Test DB not initialized")
	}

	// 1. Create a policy with steps containing messages
	policyID := uuid.New().String()
	step1ID := uuid.New().String()
	step2ID := uuid.New().String()

	p := &model.EscalationPolicy{
		ID:   policyID,
		Name: "Test Policy Messages",
		Steps: []*model.EscalationStep{
			{
				ID:             step1ID,
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "user-1",
				Message:        "Message for step 1",
				TimeoutSeconds: 60,
				MaxAttempts:    3,
			},
			{
				ID:             step2ID,
				StepIndex:      1,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "user-2",
				Message:        "Message for step 2",
				TimeoutSeconds: 60,
				MaxAttempts:    3,
			},
		},
	}

	if err := testStore.CreateEscalationPolicy(p); err != nil {
		t.Fatalf("Failed to create policy: %v", err)
	}

	// 2. Verify creation
	created, err := testStore.GetEscalationPolicyByID(policyID)
	if err != nil {
		t.Fatalf("Failed to get policy: %v", err)
	}
	if len(created.Steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(created.Steps))
	}
	if created.Steps[0].Message != "Message for step 1" {
		t.Errorf("Step 1 message mismatch: expected 'Message for step 1', got '%s'", created.Steps[0].Message)
	}
	if created.Steps[1].Message != "Message for step 2" {
		t.Errorf("Step 2 message mismatch: expected 'Message for step 2', got '%s'", created.Steps[1].Message)
	}

	// 3. Update the policy (change one message, keep another)
	created.Steps[0].Message = "Updated message for step 1"
	// step 2 message remains the same

	if err := testStore.UpdateEscalationPolicy(created); err != nil {
		t.Fatalf("Failed to update policy: %v", err)
	}

	// 4. Verify update
	updated, err := testStore.GetEscalationPolicyByID(policyID)
	if err != nil {
		t.Fatalf("Failed to get updated policy: %v", err)
	}
	if len(updated.Steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(updated.Steps))
	}

	// Find steps by index (they should be ordered)
	var s1, s2 *model.EscalationStep
	for _, s := range updated.Steps {
		if s.StepIndex == 0 {
			s1 = s
		} else if s.StepIndex == 1 {
			s2 = s
		}
	}

	if s1 == nil || s1.Message != "Updated message for step 1" {
		t.Errorf("Step 1 updated message mismatch: expected 'Updated message for step 1', got '%s'", s1.Message)
	}
	if s2 == nil || s2.Message != "Message for step 2" {
		t.Errorf("Step 2 message mismatch (should be preserved): expected 'Message for step 2', got '%s'", s2.Message)
	}
}

func TestEscalationPolicy_ContinueOnFailurePersistence(t *testing.T) {
	if testStore == nil {
		t.Skip("Test DB not initialized")
	}

	// 1. Create a policy with ContinueOnFailure set explicitly
	policyID := uuid.New().String()
	step1ID := uuid.New().String()
	step2ID := uuid.New().String()

	p := &model.EscalationPolicy{
		ID:   policyID,
		Name: "Test Policy ContinueOnFailure",
		Steps: []*model.EscalationStep{
			{
				ID:                step1ID,
				StepIndex:         0,
				Provider:          "slack",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "user-1",
				ContinueOnFailure: true,
				TimeoutSeconds:    60,
				MaxAttempts:       3,
			},
			{
				ID:                step2ID,
				StepIndex:         1,
				Provider:          "slack",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "user-2",
				ContinueOnFailure: false, // Explicitly false
				TimeoutSeconds:    60,
				MaxAttempts:       3,
			},
		},
	}

	if err := testStore.CreateEscalationPolicy(p); err != nil {
		t.Fatalf("Failed to create policy: %v", err)
	}

	// 2. Verify via GetEscalationPolicyByID
	created, err := testStore.GetEscalationPolicyByID(policyID)
	if err != nil {
		t.Fatalf("Failed to get policy by ID: %v", err)
	}
	if len(created.Steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(created.Steps))
	}
	if !created.Steps[0].ContinueOnFailure {
		t.Errorf("GetByID: Step 0 ContinueOnFailure mismatch: expected true, got false")
	}
	if created.Steps[1].ContinueOnFailure {
		t.Errorf("GetByID: Step 1 ContinueOnFailure mismatch: expected false, got true")
	}

	// 3. Verify via GetAllEscalationPolicies (list API)
	allPolicies, err := testStore.GetAllEscalationPolicies()
	if err != nil {
		t.Fatalf("Failed to get all policies: %v", err)
	}
	var foundPolicy *model.EscalationPolicy
	for _, pol := range allPolicies {
		if pol.ID == policyID {
			foundPolicy = pol
			break
		}
	}
	if foundPolicy == nil {
		t.Fatalf("Policy not found in GetAllEscalationPolicies")
	}
	if len(foundPolicy.Steps) != 2 {
		t.Fatalf("GetAll: Expected 2 steps, got %d", len(foundPolicy.Steps))
	}
	if !foundPolicy.Steps[0].ContinueOnFailure {
		t.Errorf("GetAll: Step 0 ContinueOnFailure mismatch: expected true, got false")
	}
	if foundPolicy.Steps[1].ContinueOnFailure {
		t.Errorf("GetAll: Step 1 ContinueOnFailure mismatch: expected false, got true")
	}

	// 4. Update: flip the values
	created.Steps[0].ContinueOnFailure = false
	created.Steps[1].ContinueOnFailure = true

	if err := testStore.UpdateEscalationPolicy(created); err != nil {
		t.Fatalf("Failed to update policy: %v", err)
	}

	// 5. Verify update
	updated, err := testStore.GetEscalationPolicyByID(policyID)
	if err != nil {
		t.Fatalf("Failed to get updated policy: %v", err)
	}
	if updated.Steps[0].ContinueOnFailure {
		t.Errorf("After update: Step 0 ContinueOnFailure mismatch: expected false, got true")
	}
	if !updated.Steps[1].ContinueOnFailure {
		t.Errorf("After update: Step 1 ContinueOnFailure mismatch: expected true, got false")
	}
}

// TestEscalationPolicy_AStepWithoutTimeoutAndRetriesIsSaved. The form no
// longer sends a timeout or a retry limit and the handler carries neither, so
// the step arrives with zeros; the table refuses a zero, and the columns have
// to keep their defaults instead.
func TestEscalationPolicy_AStepWithoutTimeoutAndRetriesIsSaved(t *testing.T) {
	if testStore == nil {
		t.Skip("Test DB not initialized")
	}
	step := func(index int) *model.EscalationStep {
		return &model.EscalationStep{
			ID: uuid.New().String(), StepIndex: index, Provider: "slack",
			TargetKind: "dm", TargetType: "user", TargetID: "user-1", ContinueOnFailure: true,
		}
	}
	p := &model.EscalationPolicy{ID: uuid.New().String(), Name: "Without the ignored fields", Steps: []*model.EscalationStep{step(0)}}
	if err := testStore.CreateEscalationPolicy(p); err != nil {
		t.Fatalf("create a policy whose step has no timeout and no retry limit: %v", err)
	}
	p.Steps = append(p.Steps, step(1))
	if err := testStore.UpdateEscalationPolicy(p); err != nil {
		t.Fatalf("update it with another such step: %v", err)
	}
	saved, err := testStore.GetEscalationPolicyByID(p.ID)
	if err != nil {
		t.Fatalf("read it back: %v", err)
	}
	if len(saved.Steps) != 2 {
		t.Fatalf("%d steps saved, want 2", len(saved.Steps))
	}
	for _, s := range saved.Steps {
		if s.TimeoutSeconds != 30 || s.MaxAttempts != 5 {
			t.Fatalf("step %d carries timeout %d and retries %d, want the table's defaults 30 and 5",
				s.StepIndex, s.TimeoutSeconds, s.MaxAttempts)
		}
	}
}
