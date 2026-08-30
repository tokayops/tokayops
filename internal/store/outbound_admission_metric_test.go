package store

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Two partitions offer work to the same door, and the series has to keep them
// apart.
//
// Collected together, the two most important answers this counter gives are
// unreadable: an admission that promised nobody means an alert nobody will see
// on one side and a shift change nobody will hear about on the other, and a
// rate over both is a number no alert can be written against. Every case here
// therefore reads BOTH series across the same admission, because a label that
// is right for one family and constant for the other looks correct from inside
// one of them.

func admissionCount(t *testing.T, family, outcome string) float64 {
	t.Helper()
	counter, err := metrics.OutboundAdmissionsTotal.GetMetricWithLabelValues(family, outcome)
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// admissionSeries is both families' view of one outcome, read at one moment.
type admissionSeries struct{ notification, handoff float64 }

func readAdmissions(t *testing.T, outcome string) admissionSeries {
	t.Helper()
	return admissionSeries{
		notification: admissionCount(t, string(keys.FamilyNotification), outcome),
		handoff:      admissionCount(t, string(keys.FamilyHandoff), outcome),
	}
}

func (before admissionSeries) since(t *testing.T, outcome string) admissionSeries {
	t.Helper()
	now := readAdmissions(t, outcome)
	return admissionSeries{
		notification: now.notification - before.notification,
		handoff:      now.handoff - before.handoff,
	}
}

func TestAnAdmissionIsCountedUnderTheFamilyItRunsIn(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	t.Run("a page and an announcement", func(t *testing.T) {
		before := readAdmissions(t, "created")

		agID := outboundGroup(t, s)
		if _, err := s.SubmitBatch(ctx,
			outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))); err != nil {
			t.Fatalf("admit the escalation: %v", err)
		}
		seedUsers(t, s, "u-alice")
		if _, err := s.SubmitBatch(ctx, handoffAnnouncedFor(t, "sched-1",
			time.Now().Add(time.Hour), announceTo("slack", "u-alice"))); err != nil {
			t.Fatalf("admit the announcement: %v", err)
		}

		if got := before.since(t, "created"); got.notification != 1 || got.handoff != 1 {
			t.Fatalf("one of each moved the series by %+v, want one apiece", got)
		}
	})

	t.Run("a shift change nobody could be told about", func(t *testing.T) {
		before := readAdmissions(t, "no_targets")

		// An announcement with no recipients: accepted, and it promised
		// nothing. The escalation series must not move at all.
		empty := handoffAnnouncedFor(t, "sched-2", time.Now().Add(time.Hour))
		if _, err := s.SubmitBatch(ctx, empty); err != nil {
			t.Fatalf("admit the empty announcement: %v", err)
		}

		got := before.since(t, "no_targets")
		if got.handoff != 1 {
			t.Fatalf("the handover series moved by %v, want 1", got.handoff)
		}
		if got.notification != 0 {
			t.Fatalf("an announcement moved the paging series by %v", got.notification)
		}
	})

	t.Run("an alert nobody could be paged about", func(t *testing.T) {
		before := readAdmissions(t, "no_targets")

		agID := outboundGroup(t, s)
		if _, err := s.SubmitBatch(ctx, outboundAdmission(t, agID, "unpromised")); err != nil {
			t.Fatalf("admit the escalation: %v", err)
		}

		got := before.since(t, "no_targets")
		if got.notification != 1 {
			t.Fatalf("the paging series moved by %v, want 1", got.notification)
		}
		if got.handoff != 0 {
			t.Fatalf("an escalation moved the handover series by %v", got.handoff)
		}
	})
}
