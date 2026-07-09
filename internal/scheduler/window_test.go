package scheduler

import (
	"testing"
	"time"
)

func TestCurrentOnCallWindow(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	from, until := CurrentOnCallWindow(at)

	wantFrom := at.Add(-CurrentOnCallLookback)
	wantUntil := at.Add(CurrentOnCallLookahead)

	if !from.Equal(wantFrom) {
		t.Errorf("from = %v, want %v", from, wantFrom)
	}
	if !until.Equal(wantUntil) {
		t.Errorf("until = %v, want %v", until, wantUntil)
	}

	// Verify concrete durations to lock the API contract
	if CurrentOnCallLookback != 8*24*time.Hour {
		t.Errorf("CurrentOnCallLookback = %v, want 8 days", CurrentOnCallLookback)
	}
	if CurrentOnCallLookahead != 31*24*time.Hour {
		t.Errorf("CurrentOnCallLookahead = %v, want 31 days", CurrentOnCallLookahead)
	}
}
