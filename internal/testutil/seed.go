package testutil

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func SeedTeam(t *testing.T, s *store.Store, id string) *model.Team {
	t.Helper()
	tm := &model.Team{
		ID:          id,
		Name:        "Team " + id,
		Description: "Test Team " + id,
	}
	if err := s.CreateTeam(tm); err != nil {
		t.Fatalf("Failed to seed team: %v", err)
	}
	return tm
}

func SeedUser(t *testing.T, s *store.Store, email string) *model.User {
	t.Helper()
	u := &model.User{
		ID:           uuid.New().String(),
		Email:        email,
		Name:         "User " + email,
		AuthProvider: "password",
	}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("Failed to seed user: %v", err)
	}
	// Bind a per-test slack identity so dispatch resolves the recipient.
	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID:     u.ID,
		Provider:   "slack",
		ExternalID: "U_" + email,
	}); err != nil {
		t.Fatalf("Failed to bind slack identity: %v", err)
	}
	return u
}

func SeedTeamMember(t *testing.T, s *store.Store, teamID, userID string, role model.TeamMemberRole) {
	t.Helper()
	if err := s.AddTeamMember(teamID, userID, role); err != nil {
		t.Fatalf("Failed to add team member: %v", err)
	}
}
