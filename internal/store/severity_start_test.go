package store

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// TestAStartFoldsTheSeverityOfAWaitingGroupIntoInfo. A group an earlier
// version created with a word the ingester now folds into info, and that is
// still waiting for admission, is folded at start so that it is admitted as
// the ingester would have it; a group past admission keeps its word.
func TestAStartFoldsTheSeverityOfAWaitingGroupIntoInfo(t *testing.T) {
	s := setupTestDB(t)
	now := time.Now()
	for _, g := range []struct {
		id     string
		status model.AlertGroupStatus
	}{{"waiting", model.AlertGroupStatusNew}, {"resolved", model.AlertGroupStatusResolved}} {
		if err := s.CreateAlertGroup(&model.AlertGroup{ID: g.id, AlertKey: "k-" + g.id, Status: g.status, Severity: "error", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.buildSchema(); err != nil {
		t.Fatalf("the start: %v", err)
	}
	for id, want := range map[string]string{"waiting": "info", "resolved": "error"} {
		ag, err := s.GetAlertGroupByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if ag.Severity != want {
			t.Fatalf("%s carries severity %q after the start, want %q", id, ag.Severity, want)
		}
	}
}

// TestTheStartNamesTheTeamsRoutingByAFourthSeverity. The API refuses such a
// route now; one saved by an earlier version is reported, not refused.
func TestTheStartNamesTheTeamsRoutingByAFourthSeverity(t *testing.T) {
	s := setupTestDB(t)
	now := time.Now()
	for _, tm := range []*model.Team{
		{ID: "by-error", Name: "By error", SeverityRoutes: map[string]string{"critical": "p1", "error": "p2"}, CreatedAt: now},
		{ID: "by-three", Name: "By three", SeverityRoutes: map[string]string{"critical": "p1", "info": "p3"}, CreatedAt: now},
		{ID: "by-none", Name: "By none", CreatedAt: now},
	} {
		if err := s.CreateTeam(tm); err != nil {
			t.Fatal(err)
		}
	}
	teams, err := s.TeamsRoutingUnknownSeverities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || teams[0] != "by-error" {
		t.Fatalf("the start names %v, want [by-error]", teams)
	}
}
