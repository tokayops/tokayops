package rbac

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestAdminBypassesAllChecks(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// denis is admin in mock store
	tests := []struct {
		action Action
		scope  Scope
	}{
		{ActionTeamCreate, GlobalScope()},
		{ActionTeamDelete, GlobalScope()},
		{ActionUserCreate, GlobalScope()},
		{ActionUserDelete, GlobalScope()},
		{ActionScheduleEdit, TeamScope("devops")},
		{ActionAlertAck, TeamScope("devops")},
	}

	for _, tt := range tests {
		allowed, err := c.HasPermission("denis", tt.action, tt.scope)
		if err != nil {
			t.Errorf("HasPermission(%v, %v) error: %v", tt.action, tt.scope, err)
		}
		if !allowed {
			t.Errorf("Admin should have permission for %v", tt.action)
		}
	}
}

func TestPublicInternalActions(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is regular user (not admin)
	publicActions := []Action{
		ActionAlertView,
		ActionAlertNoteAdd,
		ActionTeamList,
		ActionTeamView,
		ActionScheduleView,
		ActionUserList,
		ActionUserView,
	}

	for _, action := range publicActions {
		allowed, err := c.HasPermission("alex", action, GlobalScope())
		if err != nil {
			t.Errorf("HasPermission(%v) error: %v", action, err)
		}
		if !allowed {
			t.Errorf("Any authenticated user should have permission for %v", action)
		}
	}
}

func TestTeamMemberCanAck(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is team_member in devops
	allowed, err := c.HasPermission("alex", ActionAlertAck, TeamScope("devops"))
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if !allowed {
		t.Error("Team member should be able to ack alerts in their team")
	}
}

func TestNonMemberCannotAck(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is not in triage team
	allowed, err := c.HasPermission("alex", ActionAlertAck, TeamScope("triage"))
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if allowed {
		t.Error("Non-member should not be able to ack alerts in other teams")
	}
}

func TestTeamAdminCanEditSchedule(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// denis is team_admin in devops
	allowed, err := c.HasPermission("denis", ActionScheduleEdit, TeamScope("devops"))
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if !allowed {
		t.Error("Team admin should be able to edit schedule")
	}
}

func TestTeamMemberCannotEditSchedule(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// Create a non-admin user for this test
	s.CreateUser(&model.User{
		ID:    "bob",
		Email: "bob@example.com",
		Name:  "Bob",
		Role:  model.UserRoleUser,
	})
	s.AddTeamMember("devops", "bob", model.TeamMemberRoleMember)

	allowed, err := c.HasPermission("bob", ActionScheduleEdit, TeamScope("devops"))
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if allowed {
		t.Error("Team member (not admin) should not be able to edit schedule")
	}
}

func TestUserCannotCreateTeam(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is regular user
	allowed, err := c.HasPermission("alex", ActionTeamCreate, GlobalScope())
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if allowed {
		t.Error("Regular user should not be able to create teams")
	}
}

func TestTeamMemberCanCreateOverride(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is team_member in devops
	allowed, err := c.HasPermission("alex", ActionOverrideCreate, TeamScope("devops"))
	if err != nil {
		t.Fatalf("HasPermission error: %v", err)
	}
	if !allowed {
		t.Error("Team member should be able to create overrides in their team")
	}
}

func TestIsTeamMember(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	isMember, err := c.IsTeamMember("alex", "devops")
	if err != nil {
		t.Fatalf("IsTeamMember error: %v", err)
	}
	if !isMember {
		t.Error("alex should be a member of devops")
	}

	isMember, err = c.IsTeamMember("alex", "triage")
	if err != nil {
		t.Fatalf("IsTeamMember error: %v", err)
	}
	if isMember {
		t.Error("alex should not be a member of triage")
	}
}

func TestTeamAdminCanManageTeamIntegration(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// denis is team_admin in devops (from seed data)
	actions := []Action{
		ActionIntegrationList,
		ActionIntegrationView,
		ActionIntegrationCreate,
		ActionIntegrationUpdate,
		ActionIntegrationDelete,
	}

	for _, action := range actions {
		allowed, err := c.HasPermission("denis", action, TeamScope("devops"))
		if err != nil {
			t.Errorf("HasPermission(%v) error: %v", action, err)
		}
		if !allowed {
			t.Errorf("Team admin should have permission for %v with TeamScope", action)
		}
	}
}

func TestTeamMemberCannotManageIntegration(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is team_member (not admin) in devops
	actions := []Action{
		ActionIntegrationList,
		ActionIntegrationView,
		ActionIntegrationCreate,
		ActionIntegrationUpdate,
		ActionIntegrationDelete,
	}

	for _, action := range actions {
		allowed, err := c.HasPermission("alex", action, TeamScope("devops"))
		if err != nil {
			t.Errorf("HasPermission(%v) error: %v", action, err)
		}
		if allowed {
			t.Errorf("Team member should NOT have permission for %v", action)
		}
	}
}

func TestNonAdminCannotManageGlobalIntegration(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	// alex is regular user — with GlobalScope, integration actions should be denied
	actions := []Action{
		ActionIntegrationList,
		ActionIntegrationView,
		ActionIntegrationCreate,
		ActionIntegrationUpdate,
		ActionIntegrationDelete,
	}

	for _, action := range actions {
		allowed, err := c.HasPermission("alex", action, GlobalScope())
		if err != nil {
			t.Errorf("HasPermission(%v) error: %v", action, err)
		}
		if allowed {
			t.Errorf("Non-admin should NOT have permission for %v with GlobalScope", action)
		}
	}
}

func TestIsAdmin(t *testing.T) {
	s := store.NewMockStore()
	c := NewChecker(s)

	isAdmin, err := c.IsAdmin("denis")
	if err != nil {
		t.Fatalf("IsAdmin error: %v", err)
	}
	if !isAdmin {
		t.Error("denis should be admin")
	}

	isAdmin, err = c.IsAdmin("alex")
	if err != nil {
		t.Fatalf("IsAdmin error: %v", err)
	}
	if isAdmin {
		t.Error("alex should not be admin")
	}
}
