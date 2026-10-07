package outbound

import "testing"

func seq(n int) *int { return &n }

// Each row of the verdict table, and the order the rows are asked in. The order
// is the part that is easy to get wrong: every row here would pass if the table
// were asked in a different order, except the ones named for it.
func TestTheVerdictIsTheFirstRowThatHolds(t *testing.T) {
	effect := func(attempt string, generation int, state EffectState) Effect {
		return Effect{AttemptID: attempt, Generation: generation, ExternalRef: "CA" + attempt, State: state}
	}
	for _, tc := range []struct {
		name  string
		facts GenerationFacts
		want  Verdict
	}{
		{
			name: "1: a call took place",
			facts: GenerationFacts{Generation: 1, Effects: []Effect{
				effect("a", 1, EffectNotPlaced), effect("b", 1, EffectHappened)}},
			want: VerdictHappened,
		},
		{
			// A call of an earlier generation that did take place, late,
			// discharges the obligation: a second call is not needed.
			name: "1: a call of an earlier generation took place",
			facts: GenerationFacts{Generation: 2, Effects: []Effect{
				effect("a", 1, EffectHappened), effect("b", 2, EffectInProgress)}},
			want: VerdictHappened,
		},
		{
			name: "1 before 2: a call took place while another attempt is open",
			facts: GenerationFacts{Generation: 1, AttemptOpen: true, Effects: []Effect{
				effect("a", 1, EffectHappened)}},
			want: VerdictHappened,
		},
		{
			name: "2: an attempt is still open",
			facts: GenerationFacts{Generation: 1, AttemptOpen: true, DeadlinePassed: true, Effects: []Effect{
				effect("a", 1, EffectNotPlaced)}},
			want: VerdictStillGoing,
		},
		{
			name: "3: every call of this generation did not take place",
			facts: GenerationFacts{Generation: 1, Effects: []Effect{
				effect("a", 1, EffectNotPlaced), effect("b", 1, EffectWithdrawn)}},
			want: VerdictNotPlaced,
		},
		{
			// A failure of an earlier generation is history; this generation
			// is still ringing.
			name: "3: only this generation counts",
			facts: GenerationFacts{Generation: 2, Effects: []Effect{
				effect("a", 1, EffectNotPlaced), effect("b", 2, EffectInProgress)}},
			want: VerdictStillGoing,
		},
		{
			// One attempt of the generation ended in doubt and nothing has
			// named what it made: it may be ringing.
			name: "3 needs no doubt left",
			facts: GenerationFacts{Generation: 1, Unaccounted: 1, Effects: []Effect{
				effect("a", 1, EffectNotPlaced)}},
			want: VerdictStillGoing,
		},
		{
			name: "3 before 4: not placed even after the wait",
			facts: GenerationFacts{Generation: 1, DeadlinePassed: true, Effects: []Effect{
				effect("a", 1, EffectNotPlaced)}},
			want: VerdictNotPlaced,
		},
		{
			// The last word heard was "in progress", the callback that would
			// have ended it was lost and the polls failed. Asked the other way
			// round, this commitment would wait forever.
			name: "4 before 5: the wait is over though the last word was in progress",
			facts: GenerationFacts{Generation: 1, DeadlinePassed: true, Effects: []Effect{
				effect("a", 1, EffectInProgress)}},
			want: VerdictUnknown,
		},
		{
			name:  "4: the wait is over and nothing was heard at all",
			facts: GenerationFacts{Generation: 1, DeadlinePassed: true, Unaccounted: 1},
			want:  VerdictUnknown,
		},
		{
			name: "5: still ringing",
			facts: GenerationFacts{Generation: 1, Effects: []Effect{
				effect("a", 1, EffectInProgress)}},
			want: VerdictStillGoing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerationVerdict(tc.facts)
			if err != nil {
				t.Fatalf("verdict: %v", err)
			}
			if got != tc.want {
				t.Fatalf("verdict = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAnEffectStateThisBuildDoesNotKnowIsAnError(t *testing.T) {
	_, err := GenerationVerdict(GenerationFacts{Generation: 1, Effects: []Effect{
		{AttemptID: "a", Generation: 1, State: "ringing"}}})
	if err == nil {
		t.Fatal("an unknown state was added up")
	}
}

// Events are separate requests and arrive in any order.
func TestAnEffectFoldsEventsInTheProvidersOrder(t *testing.T) {
	start := Effect{AttemptID: "a", Generation: 1, ExternalRef: "CA1", State: EffectInProgress, LastSequence: seq(1)}

	// An older "in progress" after a newer one changes nothing.
	if got, changed := FoldEffect(Effect{AttemptID: "a", State: EffectInProgress, LastSequence: seq(3)},
		EffectInProgress, seq(2)); changed || *got.LastSequence != 3 {
		t.Fatalf("an older event moved the object: %+v", got)
	}

	// A terminal word wins whatever its number: it can only be later.
	ended, changed := FoldEffect(Effect{AttemptID: "a", State: EffectInProgress, LastSequence: seq(5)},
		EffectHappened, seq(2))
	if !changed || ended.State != EffectHappened {
		t.Fatalf("the end of the call did not land: %+v", ended)
	}

	// And it absorbs: a late "ringing", or anything else, is history.
	for _, next := range EffectStates() {
		if got, changed := FoldEffect(ended, next, seq(9)); changed || got.State != EffectHappened {
			t.Fatalf("%s moved an object that had ended: %+v", next, got)
		}
	}

	// An event with no number - a poll's answer - still lands.
	if got, changed := FoldEffect(start, EffectNotPlaced, nil); !changed || got.State != EffectNotPlaced {
		t.Fatalf("an unnumbered terminal event did not land: %+v", got)
	}
	// The same event twice is one event.
	once, _ := FoldEffect(start, EffectInProgress, seq(2))
	if _, changed := FoldEffect(once, EffectInProgress, seq(2)); changed {
		t.Fatal("a repeated event changed the object again")
	}
}
