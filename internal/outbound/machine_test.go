package outbound

import (
	"errors"
	"testing"
)

// The machine is checked twice over: a table of named rows that says what each
// rule does, and a walk of the whole input space that says nothing else does
// anything at all.
//
// The second half is the one that matters. A rule can be read; the absence of a
// rule cannot, and a state machine's real failures are the inputs nobody
// thought to write down - a cancellation arriving on a path that ignores it, an
// unknown outcome falling through to a default, a terminal state that turns out
// to have an exit.

var (
	allStatuses = []Status{
		StatusPending, StatusSending, StatusIdle, StatusManualReview,
		StatusSucceeded, StatusPermanentFailed, StatusExpired, StatusCanceled,
	}
	allOutcomes = []Outcome{
		OutcomeAccepted, OutcomeRetryableRejection, OutcomePermanentRejection,
		OutcomeAmbiguous, OutcomeCanceled,
	}
	allPolicies = []AmbiguityPolicy{
		PolicyRetry, PolicyReconcileThenRetry, PolicyManualReview, PolicyAssumeAccepted,
	}
	allForms       = []Form{FormOneShot, FormEditable}
	allPreparation = []PreparationOutcome{
		PreparationReady, PreparationPermanent, PreparationTransient,
	}
	allDecisions = []Decision{
		DecisionAssumeAccepted, DecisionCancel,
		DecisionRetryCurrentGeneration, DecisionRetryNewGeneration,
	}
	allAttemptKinds = []AttemptKind{AttemptCreate, AttemptMutation, AttemptReconcile}
	allCompletion   = []CompletionMode{CompletionOnAcceptance, CompletionOnProviderReceipt}
	bothWays        = []bool{false, true}
)

func sendingIntent() Intent {
	return Intent{
		ID:              "intent-1",
		AlertGroupID:    "ag-1",
		Provider:        "slack",
		Form:            FormOneShot,
		CompletionMode:  CompletionOnAcceptance,
		AmbiguityPolicy: PolicyRetry,
		Status:          StatusSending,
	}
}

// TestDecideNamedRows pins what each rule of the specification does. The row
// names are part of the assertion: a transition that lands in the right state
// by the wrong rule is a rule nobody is testing.
func TestDecideNamedRows(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		to   Status
		row  string
	}{
		{
			name: "an attempt begins",
			in: Input{
				Intent:      Intent{Status: StatusPending},
				Trigger:     TriggerPreparation,
				Preparation: PreparationReady,
			},
			to: StatusSending, row: "T4",
		},
		{
			name: "preparation refused for good",
			in: Input{
				Intent:      Intent{Status: StatusPending},
				Trigger:     TriggerPreparation,
				Preparation: PreparationPermanent,
			},
			to: StatusPermanentFailed, row: "T4a",
		},
		{
			name: "preparation refused for now",
			in: Input{
				Intent:      Intent{Status: StatusPending},
				Trigger:     TriggerPreparation,
				Preparation: PreparationTransient,
			},
			to: StatusPending, row: "T4b",
		},
		{
			name: "accepted, one-shot",
			in: Input{
				Intent:  sendingIntent(),
				Trigger: TriggerFinishAttempt,
				Outcome: OutcomeAccepted,
			},
			to: StatusSucceeded, row: "T8/S1",
		},
		{
			name: "retryable",
			in: Input{
				Intent:  sendingIntent(),
				Trigger: TriggerFinishAttempt,
				Outcome: OutcomeRetryableRejection,
			},
			to: StatusPending, row: "T9",
		},
		{
			name: "permanently refused",
			in: Input{
				Intent:  sendingIntent(),
				Trigger: TriggerFinishAttempt,
				Outcome: OutcomePermanentRejection,
			},
			to: StatusPermanentFailed, row: "T10",
		},
		{
			name: "ambiguous, retry",
			in: Input{
				Intent:  sendingIntent(),
				Trigger: TriggerFinishAttempt,
				Outcome: OutcomeAmbiguous,
			},
			to: StatusPending, row: "T11",
		},
		{
			name: "ambiguous, reconcile first",
			in: func() Input {
				i := sendingIntent()
				i.AmbiguityPolicy = PolicyReconcileThenRetry
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAmbiguous}
			}(),
			to: StatusPending, row: "T12",
		},
		{
			name: "ambiguous, ask a person",
			in: func() Input {
				i := sendingIntent()
				i.AmbiguityPolicy = PolicyManualReview
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAmbiguous}
			}(),
			to: StatusManualReview, row: "T13",
		},
		{
			name: "ambiguous, assumed delivered",
			in: func() Input {
				i := sendingIntent()
				i.AmbiguityPolicy = PolicyAssumeAccepted
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAmbiguous}
			}(),
			to: StatusSucceeded, row: "T14/S1",
		},
		{
			name: "withdrawn while failing",
			in: func() Input {
				i := sendingIntent()
				i.CancellationRequested = true
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeRetryableRejection}
			}(),
			to: StatusCanceled, row: "T15",
		},
		{
			name: "withdrawn, but it had already gone out",
			in: func() Input {
				i := sendingIntent()
				i.CancellationRequested = true
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAccepted}
			}(),
			to: StatusSucceeded, row: "T16/S1",
		},
		{
			name: "an editable card applied the latest revision",
			in: func() Input {
				i := sendingIntent()
				i.Form = FormEditable
				i.DesiredRevision = 3
				return Input{
					Intent: i, Trigger: TriggerFinishAttempt,
					Outcome: OutcomeAccepted, AttemptRevision: 3,
				}
			}(),
			to: StatusIdle, row: "T8/S3",
		},
		{
			name: "an editable card fell behind while it was sending",
			in: func() Input {
				i := sendingIntent()
				i.Form = FormEditable
				i.DesiredRevision = 4
				return Input{
					Intent: i, Trigger: TriggerFinishAttempt,
					Outcome: OutcomeAccepted, AttemptRevision: 3,
				}
			}(),
			to: StatusPending, row: "T8/S4",
		},
		{
			name: "an editable card applied its last revision",
			in: func() Input {
				i := sendingIntent()
				i.Form = FormEditable
				i.DesiredRevision = 3
				return Input{
					Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAccepted,
					AttemptRevision: 3, AttemptIsFinal: true,
				}
			}(),
			to: StatusSucceeded, row: "T8/S2",
		},
		{
			name: "a lease died mid-flight",
			in:   Input{Intent: sendingIntent(), Trigger: TriggerRecoverStale},
			to:   StatusPending, row: "T7",
		},
		{
			name: "a lease died mid-flight, past the deadline",
			in:   Input{Intent: sendingIntent(), Trigger: TriggerRecoverStale, Expired: true},
			to:   StatusExpired, row: "T7",
		},
		{
			name: "an operator accepts the risk",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusManualReview
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionAssumeAccepted}
			}(),
			to: StatusSucceeded, row: "T25/S1",
		},
		{
			name: "an operator gives up on it",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusManualReview
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionCancel}
			}(),
			to: StatusCanceled, row: "T27",
		},
		{
			name: "an operator decides not to deliver a confirmed failure",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusPermanentFailed
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionCancel}
			}(),
			to: StatusCanceled, row: "T30",
		},
		{
			name: "an operator fixed the configuration",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusPermanentFailed
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionRetryCurrentGeneration}
			}(),
			to: StatusPending, row: "T31",
		},
		{
			name: "an operator starts a new effect",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusManualReview
				return Input{
					Intent: i, Trigger: TriggerOperator,
					Decision: DecisionRetryNewGeneration, LastAttemptKind: AttemptCreate,
				}
			}(),
			to: StatusPending, row: "T29",
		},
		{
			name: "an operator revives an expired commitment",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusExpired
				return Input{
					Intent: i, Trigger: TriggerOperator,
					Decision: DecisionRetryCurrentGeneration, NewExpiryProvided: true,
				}
			}(),
			to: StatusPending, row: "T33",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.in)
			if err != nil {
				t.Fatalf("the machine refused a rule it has: %v", err)
			}
			if got.To != tc.to {
				t.Errorf("landed in %s, want %s", got.To, tc.to)
			}
			if got.Row != tc.row {
				t.Errorf("fired rule %s, want %s", got.Row, tc.row)
			}
		})
	}
}

// TestDecideRefusesWhatItDoesNotHave pins the refusals that are decisions
// rather than gaps: each of these is something a caller might reasonably ask
// for and the domain deliberately will not do.
func TestDecideRefusesWhatItDoesNotHave(t *testing.T) {
	manualReview := func() Intent {
		i := sendingIntent()
		i.Status = StatusManualReview
		return i
	}

	cases := []struct {
		name string
		in   Input
	}{
		{
			name: "assuming delivery of a confirmed refusal",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusPermanentFailed
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionAssumeAccepted}
			}(),
		},
		{
			name: "assuming delivery of something that expired unsent",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusExpired
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionAssumeAccepted}
			}(),
		},
		{
			name: "assuming delivery of an editable card with nothing to edit",
			in: func() Input {
				i := manualReview()
				i.Form = FormEditable
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionAssumeAccepted}
			}(),
		},
		{
			name: "cancelling something that already expired",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusExpired
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionCancel}
			}(),
		},
		{
			name: "reviving an expired commitment with no new deadline",
			in: func() Input {
				i := sendingIntent()
				i.Status = StatusExpired
				return Input{Intent: i, Trigger: TriggerOperator, Decision: DecisionRetryCurrentGeneration}
			}(),
		},
		{
			name: "a new effect where the old one may still exist",
			in: Input{
				Intent: manualReview(), Trigger: TriggerOperator,
				Decision: DecisionRetryNewGeneration, LastAttemptKind: AttemptMutation,
			},
		},
		{
			name: "a new effect after doubt, with nobody accepting the duplicate",
			in: Input{
				Intent: manualReview(), Trigger: TriggerOperator,
				Decision: DecisionRetryNewGeneration, LastAttemptKind: AttemptCreate,
				AmbiguousInGeneration: true,
			},
		},
		{
			name: "assuming delivery automatically for an editable card",
			in: func() Input {
				i := sendingIntent()
				i.Form = FormEditable
				i.AmbiguityPolicy = PolicyAssumeAccepted
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAmbiguous}
			}(),
		},
		{
			name: "a channel that waits for the provider's own confirmation",
			in: func() Input {
				i := sendingIntent()
				i.CompletionMode = CompletionOnProviderReceipt
				return Input{Intent: i, Trigger: TriggerFinishAttempt, Outcome: OutcomeAccepted}
			}(),
		},
		{
			name: "an attempt closed as cancelled that nobody withdrew",
			in: Input{
				Intent: sendingIntent(), Trigger: TriggerFinishAttempt, Outcome: OutcomeCanceled,
			},
		},
		{
			name: "finishing an attempt that was never started",
			in: Input{
				Intent: Intent{Status: StatusPending}, Trigger: TriggerFinishAttempt,
				Outcome: OutcomeAccepted,
			},
		},
		{
			name: "an operator decision on something still in flight",
			in: Input{
				Intent: sendingIntent(), Trigger: TriggerOperator, Decision: DecisionCancel,
			},
		},
		{
			name: "a trigger nobody defined",
			in:   Input{Intent: sendingIntent(), Trigger: Trigger("apply_provider_event")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decide(tc.in)
			if err == nil {
				t.Fatal("the machine performed a transition it does not have")
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("expected an invalid transition, got: %v", err)
			}
		})
	}
}

// TestDecideOverTheWholeInputSpace walks every combination the machine can be
// asked about and checks the properties that have to hold for all of them.
//
// This is where "exhaustive" stops being a claim. Each case either produces one
// named transition or refuses; nothing falls through, nothing depends on the
// order two guards happen to be written in, and no terminal state is left by
// anything but a person.
func TestDecideOverTheWholeInputSpace(t *testing.T) {
	checked := 0

	check := func(in Input) {
		checked++

		got, err := Decide(in)
		if err != nil {
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("a refusal that is not a contract violation: %v", err)
			}
			return
		}

		// Determinism: the same question twice is the same answer. A machine
		// that reads anything but its input would fail here.
		again, againErr := Decide(in)
		if againErr != nil || again != got {
			t.Fatalf("asked twice, answered differently:\nfirst: %+v\nthen:  %+v (%v)",
				got, again, againErr)
		}

		if got.Row == "" {
			t.Fatalf("a transition with no rule behind it: %+v", in)
		}
		if !knownStatus(got.To) {
			t.Fatalf("landed outside the domain, in %q", got.To)
		}

		e := got.Effects

		// A commitment nobody is allowed to leave was left.
		if in.Intent.Status.Terminal() && in.Trigger != TriggerOperator {
			t.Fatalf("%s was left by %s", in.Intent.Status, in.Trigger)
		}

		// Success is one shape, whatever proved it.
		if e.Proof != "" {
			if !e.ApplyRevision || !e.StoreReceipt || !e.TriggerGroup || !e.ResetFailureStreak {
				t.Fatalf("a success that did not settle: %+v", e)
			}
			switch got.To {
			case StatusSucceeded, StatusIdle, StatusPending:
			default:
				t.Fatalf("a success landed in %s", got.To)
			}
			if e.Proof == ProofAssumed && !e.RecordDuplicateRisk {
				t.Fatal("an assumed success recorded no risk")
			}
		} else if e.ApplyRevision || e.StoreReceipt || e.TriggerGroup {
			t.Fatalf("a non-success applied a revision or moved the group: %+v", e)
		}

		// Leaving "sending" always releases what was held there. A lease or an
		// attempt id left behind is a row nothing will ever pick up again.
		if in.Intent.Status == StatusSending && got.To != StatusSending {
			if !e.ClearLease || !e.ClearCurrentAttempt {
				t.Fatalf("left sending without releasing the lease or the attempt: %+v", e)
			}
		}

		// The withdrawal flag is consumed exactly where it was read.
		if e.ConsumeCancellation && !in.Intent.CancellationRequested {
			t.Fatal("consumed a withdrawal nobody asked for")
		}
		if in.Intent.CancellationRequested && in.Intent.Status == StatusSending &&
			in.Trigger != TriggerOperator && !e.ConsumeCancellation {
			t.Fatalf("left a withdrawal unconsumed: %+v", got)
		}

		// Two schedules are one schedule too many.
		if e.ScheduleRetry && e.ScheduleNow {
			t.Fatalf("scheduled the next attempt twice: %+v", e)
		}
		if (e.ScheduleRetry || e.ScheduleNow) && got.To != StatusPending {
			t.Fatalf("scheduled an attempt for a commitment in %s", got.To)
		}
		if e.BumpFailureStreak && e.ResetFailureStreak {
			t.Fatalf("both raised and cleared the failure streak: %+v", e)
		}

		// Doubt is never resolved by silence: an ambiguous attempt always ends
		// somewhere a person or a retry can reach.
		if in.Trigger == TriggerFinishAttempt && in.Outcome == OutcomeAmbiguous &&
			!in.Intent.CancellationRequested {
			switch got.To {
			case StatusPending, StatusManualReview, StatusSucceeded, StatusIdle:
			default:
				t.Fatalf("an ambiguous attempt was buried in %s", got.To)
			}
		}
	}

	for _, status := range allStatuses {
		for _, prep := range allPreparation {
			check(Input{
				Intent:      Intent{Status: status},
				Trigger:     TriggerPreparation,
				Preparation: prep,
			})
		}
	}

	for _, status := range allStatuses {
		for _, outcome := range allOutcomes {
			for _, policy := range allPolicies {
				for _, form := range allForms {
					for _, completion := range allCompletion {
						for _, final := range bothWays {
							for _, canceled := range bothWays {
								for _, behind := range bothWays {
									intent := sendingIntent()
									intent.Status = status
									intent.AmbiguityPolicy = policy
									intent.Form = form
									intent.CompletionMode = completion
									intent.CancellationRequested = canceled
									intent.DesiredRevision = 3
									revision := int64(3)
									if behind {
										revision = 2
									}
									check(Input{
										Intent:          intent,
										Trigger:         TriggerFinishAttempt,
										Outcome:         outcome,
										AttemptRevision: revision,
										AttemptIsFinal:  final,
									})
								}
							}
						}
					}
				}
			}
		}
	}

	for _, status := range allStatuses {
		for _, policy := range allPolicies {
			for _, form := range allForms {
				for _, canceled := range bothWays {
					for _, expired := range bothWays {
						intent := sendingIntent()
						intent.Status = status
						intent.AmbiguityPolicy = policy
						intent.Form = form
						intent.CancellationRequested = canceled
						check(Input{
							Intent:  intent,
							Trigger: TriggerRecoverStale,
							Expired: expired,
						})
					}
				}
			}
		}
	}

	for _, status := range allStatuses {
		for _, decision := range allDecisions {
			for _, form := range allForms {
				for _, receipt := range bothWays {
					for _, kind := range allAttemptKinds {
						for _, ambiguous := range bothWays {
							for _, risk := range bothWays {
								for _, loss := range bothWays {
									for _, expiry := range bothWays {
										intent := sendingIntent()
										intent.Status = status
										intent.Form = form
										intent.HasReceipt = receipt
										check(Input{
											Intent:                intent,
											Trigger:               TriggerOperator,
											Decision:              decision,
											LastAttemptKind:       kind,
											AmbiguousInGeneration: ambiguous,
											AcceptedDuplicateRisk: risk,
											ResourceLossConfirmed: loss,
											NewExpiryProvided:     expiry,
										})
									}
								}
							}
						}
					}
				}
			}
		}
	}

	if checked < 1000 {
		t.Fatalf("the walk covered only %d inputs; it is meant to be exhaustive", checked)
	}
	t.Logf("walked %d inputs", checked)
}

func knownStatus(s Status) bool {
	for _, known := range allStatuses {
		if known == s {
			return true
		}
	}
	return false
}
