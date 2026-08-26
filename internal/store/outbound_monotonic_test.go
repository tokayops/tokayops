package store

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The one property the whole revision model exists to hold: a card never shows
// an older state than it already showed, whatever order things happen in.

// cardState is what the property is asserted over, read from the row.
type cardState struct {
	Status   outbound.Status
	Desired  int64
	Applied  int64
	HasApply bool
}

func readCard(t *testing.T, s *Store, intentID string) cardState {
	t.Helper()
	var (
		status  string
		desired int64
		applied *int64
	)
	if err := s.db.QueryRow(`
		SELECT status, desired_revision, applied_revision
		FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&status, &desired, &applied); err != nil {
		t.Fatalf("read the card: %v", err)
	}
	out := cardState{Status: outbound.Status(status), Desired: desired}
	if applied != nil {
		out.Applied, out.HasApply = *applied, true
	}
	return out
}

// tryClaim is claimOne without the assertion: in a generated sequence a card is
// often settled, and "there was nothing to claim" is an ordinary step.
func tryClaim(t *testing.T, s *Store, intentID string) (string, bool) {
	t.Helper()
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID == intentID {
			return l.LeaseToken, true
		}
	}
	return "", false
}

// raiseFor moves what the group's messages have to show, with an alert set that
// really is different each time - otherwise the revision is "unchanged" and the
// step does nothing.
func raiseFor(t *testing.T, s *Store, agID string, step int) {
	t.Helper()
	recordAlerts(t, s, agID, []model.Alert{{
		Fingerprint: fmt.Sprintf("fp-%d", step), Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700000000+int64(step)*60, 0),
		Labels:   map[string]string{"alertname": fmt.Sprintf("Alert%d", step)},
	}})
	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	})
	if err != nil {
		t.Fatalf("raise the desired state: %v", err)
	}
	if result.Outcome != outbound.DesiredApplied {
		t.Fatalf("step %d raised nothing: %s", step, result.Outcome)
	}
}

// TestTheCardOnlyEverMovesForward generates revisions and attempt outcomes in
// every interleaving a seed produces, and asserts the same three things after
// every single step.
//
// The interesting one is the third. An attempt renders the revision it was
// opened at and applies THAT, not whatever the desired state has become since:
// a card that recorded the newest revision while showing an older one would be
// permanently, invisibly wrong, and no further revision would fix it because
// the numbers already agree.
//
// The seeds are fixed. A property that fails on a different sequence every run
// cannot be investigated.
func TestTheCardOnlyEverMovesForward(t *testing.T) {
	for _, seed := range []int64{1, 7, 13, 42, 99} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")

			agID := desiredGroup(t, s, "Disk filling up")
			intentID := changeableCard(t, s, agID)

			rng := rand.New(rand.NewSource(seed))
			was := readCard(t, s, intentID)
			step := 0

			check := func(what string) {
				t.Helper()
				now := readCard(t, s, intentID)
				if now.Applied < was.Applied {
					t.Fatalf("%s: the card went back from revision %d to %d",
						what, was.Applied, now.Applied)
				}
				if now.HasApply && now.Applied > now.Desired {
					t.Fatalf("%s: the card claims revision %d and only %d was ever asked for",
						what, now.Applied, now.Desired)
				}
				if now.Status == outbound.StatusIdle && now.Applied != now.Desired {
					t.Fatalf("%s: the card settled at revision %d with %d outstanding",
						what, now.Applied, now.Desired)
				}
				was = now
			}

			var raised, attempted, overtook int
			for i := 0; i < 24; i++ {
				switch rng.Intn(3) {
				case 0:
					raised++
					step++
					raiseFor(t, s, agID, step)
					check("after a revision")

				case 1:
					token, ok := tryClaim(t, s, intentID)
					if !ok {
						continue
					}
					attempted++
					begun := beginOne(t, s, intentID, token)
					finishOne(t, s, intentID, token, begun, rng)
					check("after an attempt")

				case 2:
					// A revision arriving while the call is out. The attempt
					// still applies what it rendered.
					token, ok := tryClaim(t, s, intentID)
					if !ok {
						continue
					}
					overtook++
					begun := beginOne(t, s, intentID, token)
					step++
					raiseFor(t, s, agID, step)
					finishOne(t, s, intentID, token, begun, rng)
					check("after a revision overtook an attempt")
				}
			}

			// A generated sequence that never reached the interesting states
			// would pass while asserting nothing, so what it reached is
			// asserted too.
			if raised+overtook < 5 || attempted+overtook < 3 || overtook < 1 {
				t.Fatalf("this sequence did too little to prove anything: "+
					"%d revisions, %d attempts, %d of them overtaken",
					raised+overtook, attempted+overtook, overtook)
			}

			// And it catches up. Nothing raises a revision now, so a card that
			// is still behind has to reach the last one - a queue that stops
			// short is the failure this property is really about.
			for i := 0; i < 40; i++ {
				now := readCard(t, s, intentID)
				if now.Status == outbound.StatusIdle && now.Applied == now.Desired {
					return
				}
				token, ok := tryClaim(t, s, intentID)
				if !ok {
					due(t, s, intentID)
					continue
				}
				begun := beginOne(t, s, intentID, token)
				applyOne(t, s, intentID, token, begun)
				check("while catching up")
			}
			final := readCard(t, s, intentID)
			t.Fatalf("the card stopped at revision %d of %d, %s",
				final.Applied, final.Desired, final.Status)
		})
	}
}

// finishOne ends an attempt the way a provider might: taken, or refused in a
// way that will be tried again.
func finishOne(t *testing.T, s *Store, intentID, token string,
	begun outbound.BeginAttemptResult, rng *rand.Rand) {

	t.Helper()
	if rng.Intn(2) == 0 {
		applyOne(t, s, intentID, token, begun)
		return
	}
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatalf("refuse the attempt: %v", err)
	}
	// The backoff is real time, and this test is not waiting it out.
	due(t, s, intentID)
}

// applyOne ends an attempt as taken, and checks the half of the property that
// only the attempt itself can answer: what it applied is what it rendered.
func applyOne(t *testing.T, s *Store, intentID, token string,
	begun outbound.BeginAttemptResult) {

	t.Helper()
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: mutationAccepted(t, begun.ReceiptRef, outbound.Receipt{}),
	}); err != nil {
		t.Fatalf("apply the revision: %v", err)
	}
	var recorded int64
	if err := s.db.QueryRow(
		`SELECT applied_revision FROM outbound_attempts WHERE id = $1`,
		begun.AttemptID).Scan(&recorded); err != nil {
		t.Fatalf("read what the attempt applied: %v", err)
	}
	if recorded != begun.AppliedRevision {
		t.Fatalf("the attempt rendered revision %d and recorded %d",
			begun.AppliedRevision, recorded)
	}

	// And the card is at the revision this attempt showed - not at whatever
	// the alert has moved to since. Taking the newer number here is the
	// failure this whole test is about: the card would be permanently showing
	// an older state with nothing outstanding to fix it, because desired and
	// applied would already agree.
	if now := readCard(t, s, intentID); now.Applied != begun.AppliedRevision {
		t.Fatalf("the card shows revision %d and records %d",
			begun.AppliedRevision, now.Applied)
	}
}
