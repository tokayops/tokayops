package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestConcurrentAck(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "race-ack-1", "dedup-race-ack-1", model.AlertGroupStatusTriggered)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	codes := make([]int, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/race-ack-1/ack", nil)
			addAuth(req, "denis")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}

	wg.Wait()

	// All requests should return 200 (winner gets changed=true, losers get idempotent 200)
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: expected 200, got %d", i, code)
		}
	}

	// Exactly 1 ack timeline event
	events, err := s.GetTimelineEvents("race-ack-1")
	if err != nil {
		t.Fatalf("Failed to get timeline events: %v", err)
	}
	ackCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventAcknowledged {
			ackCount++
		}
	}
	if ackCount != 1 {
		t.Errorf("Expected exactly 1 ack timeline event, got %d", ackCount)
	}

	// Final status should be acknowledged
	ag, err := s.GetAlertGroupByID("race-ack-1")
	if err != nil {
		t.Fatalf("Failed to get alert group: %v", err)
	}
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status 'acknowledged', got '%s'", ag.Status)
	}
}

func TestConcurrentResolve(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "race-res-1", "dedup-race-res-1", model.AlertGroupStatusTriggered)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	codes := make([]int, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/race-res-1/resolve", nil)
			addAuth(req, "denis")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}

	wg.Wait()

	// All requests should return 200
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: expected 200, got %d", i, code)
		}
	}

	// Exactly 1 resolve timeline event
	events, err := s.GetTimelineEvents("race-res-1")
	if err != nil {
		t.Fatalf("Failed to get timeline events: %v", err)
	}
	resolveCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventResolved {
			resolveCount++
		}
	}
	if resolveCount != 1 {
		t.Errorf("Expected exactly 1 resolve timeline event, got %d", resolveCount)
	}

	// Final status should be resolved
	ag, err := s.GetAlertGroupByID("race-res-1")
	if err != nil {
		t.Fatalf("Failed to get alert group: %v", err)
	}
	if ag.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
	}
}
