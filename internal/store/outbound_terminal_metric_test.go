package store

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Every door into a terminal state, counted exactly once.
//
// This is a table rather than a test per door because the failure it exists to
// catch is an omission, and an omission is invisible in a test written per
// case: the two doors that were missed first - a preparation refused for good,
// and an operator deciding - were missed precisely because nobody was writing a
// test about them. Listed together, a new door with no row here is a gap
// somebody has to look at.
//
// The counter alerts on any increment, so both halves matter: a transition into
// a terminal state must give exactly one, and everything else must give none.

func terminalCount(t *testing.T, family, status string) float64 {
	t.Helper()
	counter, err := metrics.OutboundIntentsTerminalTotal.GetMetricWithLabelValues(family, status)
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestEveryDoorIntoATerminalStateIsCounted(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	doors := []struct {
		name string
		want map[string]float64 // status -> expected increments
		open func(t *testing.T)
	}{
		{
			name: "the provider refused for good",
			want: map[string]float64{string(outbound.StatusPermanentFailed): 1},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				result, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
					AttemptID:  beginOne(t, s, intentID, token).AttemptID,
					LeaseToken: token,
					Conclusion: concluded(outbound.OutcomePermanentRejection, "channel_not_found"),
				})
				if err != nil || result.To != outbound.StatusPermanentFailed {
					t.Fatalf("finalize: %v %v", result.To, err)
				}
			},
		},
		{
			name: "a one-shot message went out",
			want: map[string]float64{string(outbound.StatusSucceeded): 1},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				result, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
					AttemptID:  beginOne(t, s, intentID, token).AttemptID,
					LeaseToken: token,
					Conclusion: acceptedConclusion(t, "U0001"),
				})
				if err != nil || result.To != outbound.StatusSucceeded {
					t.Fatalf("finalize: %v %v", result.To, err)
				}
			},
		},
		{
			// The other half of the same rule, and the reason the case above
			// names a one-shot: an editable card that was accepted is not
			// finished, it is up to date. Counted as an ending it would report
			// a delivery as over while the card is still being kept current.
			name: "an editable card was accepted, which is not an ending",
			want: map[string]float64{},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, channelCommitment("C0001", 0))
				token := claimOne(t, s, intentID)
				result, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
					AttemptID:  beginOne(t, s, intentID, token).AttemptID,
					LeaseToken: token,
					Conclusion: acceptedConclusion(t, "C0001"),
				})
				if err != nil || result.To != outbound.StatusIdle {
					t.Fatalf("finalize: %v %v", result.To, err)
				}
			},
		},
		{
			name: "preparation was refused for good, before any call",
			want: map[string]float64{string(outbound.StatusPermanentFailed): 1},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				result, err := s.BeginAttempt(ctx, outbound.BeginAttemptRequest{
					IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
					Preparation: outbound.PreparationPermanent, ErrorClass: "identity_not_linked",
				})
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				if result.Outcome != outbound.BeginPreparedPermanent {
					t.Fatalf("a permanent refusal answered %q", result.Outcome)
				}
			},
		},
		{
			name: "the deadline passed unsent",
			want: map[string]float64{string(outbound.StatusExpired): 1},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				if _, err := s.db.Exec(
					`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
					intentID); err != nil {
					t.Fatalf("bring the deadline forward: %v", err)
				}
				expired, err := s.ExpireDueIntents(ctx, outbound.FamilyNotification, 10)
				if err != nil || len(expired) != 1 {
					t.Fatalf("expire: %d %v", len(expired), err)
				}
			},
		},
		{
			name: "the alert was acknowledged",
			want: map[string]float64{string(outbound.StatusCanceled): 2},
			open: func(t *testing.T) {
				agID := outboundGroup(t, s)
				admitOne(t, s, agID, channelCommitment("C0001", 0), dmCommitment("U0001"))
				if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
					t.Fatalf("acknowledge: %v", err)
				}
			},
		},
		{
			name: "an operator withdrew a stuck commitment",
			want: map[string]float64{string(outbound.StatusCanceled): 1},
			open: func(t *testing.T) {
				intentID := stuckInReview(t, s, outboundGroup(t, s))
				result, err := s.ResolveAmbiguity(ctx, outbound.ResolveAmbiguityRequest{
					IntentID: intentID, Decision: outbound.DecisionCancel,
					Actor: "nina", Reason: "the alert is handled",
				})
				if err != nil || result.Outcome != outbound.ResolveResolved {
					t.Fatalf("resolve the ambiguity: %v %v", result.Outcome, err)
				}
			},
		},
		{
			name: "recovery assumed a delivery that had gone quiet",
			want: map[string]float64{string(outbound.StatusSucceeded): 1},
			open: func(t *testing.T) {
				commitment := dmCommitment("U0001")
				commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
				intentID := oneOwed(t, s, commitment)

				token := claimOne(t, s, intentID)
				beginOne(t, s, intentID, token)
				expireLease(t, s, intentID)

				recovered, err := s.RecoverStaleAttempts(ctx, outbound.FamilyNotification, 10)
				if err != nil || len(recovered) != 1 ||
					recovered[0].To != outbound.StatusSucceeded {
					t.Fatalf("recover: %+v %v", recovered, err)
				}
			},
		},
		{
			// The other two things recovery can decide. Both leave the
			// commitment alive - one to be tried again, one to wait for a
			// person - and counting either as an ending would report a page
			// as over while it is still owed.
			name: "recovery left the commitment to be tried again",
			want: map[string]float64{},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				beginOne(t, s, intentID, token)
				expireLease(t, s, intentID)

				recovered, err := s.RecoverStaleAttempts(ctx, outbound.FamilyNotification, 10)
				if err != nil || len(recovered) != 1 ||
					recovered[0].To != outbound.StatusPending {
					t.Fatalf("recover: %+v %v", recovered, err)
				}
			},
		},
		{
			name: "recovery left the commitment for a person to decide",
			want: map[string]float64{},
			open: func(t *testing.T) {
				commitment := dmCommitment("U0001")
				commitment.AmbiguityPolicy = keys.PolicyManualReview
				intentID := oneOwed(t, s, commitment)

				token := claimOne(t, s, intentID)
				beginOne(t, s, intentID, token)
				expireLease(t, s, intentID)

				recovered, err := s.RecoverStaleAttempts(ctx, outbound.FamilyNotification, 10)
				if err != nil || len(recovered) != 1 ||
					recovered[0].To != outbound.StatusManualReview {
					t.Fatalf("recover: %+v %v", recovered, err)
				}
			},
		},
		{
			// Three separate calls to the same withdrawal, one per door, and
			// each of them can be dropped on its own. Acknowledging is above;
			// these are the two ways an alert ends.
			name: "a person resolved the alert",
			want: map[string]float64{string(outbound.StatusCanceled): 2},
			open: func(t *testing.T) {
				agID := outboundGroup(t, s)
				admitOne(t, s, agID, channelCommitment("C0001", 0), dmCommitment("U0001"))
				if changed, err := s.ResolveAlertGroupAtomic(agID, "nina", nil, nil); err != nil || !changed {
					t.Fatalf("resolve: %v %v", changed, err)
				}
			},
		},
		{
			name: "every alert in the group resolved itself",
			want: map[string]float64{string(outbound.StatusCanceled): 2},
			open: func(t *testing.T) {
				agID := outboundGroup(t, s)
				admitOne(t, s, agID, channelCommitment("C0001", 0), dmCommitment("U0001"))
				if changed, err := s.ResolveAlertGroupWithAlertsAtomic(agID, nil, nil, nil); err != nil || !changed {
					t.Fatalf("auto-resolve: %v %v", changed, err)
				}
			},
		},
		{
			name: "a retryable rejection, which is not an ending",
			want: map[string]float64{},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				result, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
					AttemptID:  beginOne(t, s, intentID, token).AttemptID,
					LeaseToken: token,
					Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
				})
				if err != nil || result.To != outbound.StatusPending {
					t.Fatalf("finalize: %v %v", result.To, err)
				}
			},
		},
		{
			name: "a repeat of a result already recorded",
			want: map[string]float64{string(outbound.StatusSucceeded): 1},
			open: func(t *testing.T) {
				intentID := oneOwed(t, s, dmCommitment("U0001"))
				token := claimOne(t, s, intentID)
				attemptID := beginOne(t, s, intentID, token).AttemptID
				request := outbound.FinalizeRequest{
					AttemptID: attemptID, LeaseToken: token,
					Conclusion: acceptedConclusion(t, "U0001"),
				}
				if _, err := s.FinalizeDeliveryAttempt(ctx, request); err != nil {
					t.Fatalf("finalize: %v", err)
				}
				// The reply was lost and the worker asks again. One ending,
				// one increment.
				again, err := s.FinalizeDeliveryAttempt(ctx, request)
				if err != nil || again.Outcome != outbound.FinalizeIdempotentRepeat {
					t.Fatalf("the repeat answered %q: %v", again.Outcome, err)
				}
			},
		},
	}

	statuses := []string{
		string(outbound.StatusSucceeded), string(outbound.StatusPermanentFailed),
		string(outbound.StatusExpired), string(outbound.StatusCanceled),
	}

	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			before := map[string]float64{}
			for _, status := range statuses {
				before[status] = terminalCount(t, outbound.FamilyNotification, status)
			}

			door.open(t)

			for _, status := range statuses {
				got := terminalCount(t, outbound.FamilyNotification, status) - before[status]
				if want := door.want[status]; got != want {
					t.Errorf("%s counted %v, want %v", status, got, want)
				}
			}
		})
	}
}

// oneOwed admits exactly one commitment, on a group of its own.
//
// A group each, because a door that ends a commitment usually ends the group's
// escalation with it, and because admitOne returns ids in KEY order rather than
// in the order the commitments were passed - a case reaching for the wrong one
// of two would read as a counting bug and be a fixture bug.
func oneOwed(t *testing.T, s *Store, commitment keys.EscalationCommitment) string {
	t.Helper()
	return admitOne(t, s, outboundGroup(t, s), commitment)[0]
}

// acceptedConclusion is a delivery the provider confirmed, with coordinates.
func acceptedConclusion(t *testing.T, ref string) outbound.Conclusion {
	t.Helper()
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAccepted, Status: "ok",
		Receipt: receiptOf(ref+"/1700000000.000100",
			`{"channel":"`+ref+`","ts":"1700000000.000100"}`),
	})
}
