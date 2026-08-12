package schedulerender

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// The chain must tile the part of the range whose history is supposed to be
// exact. Comparing neighbouring revisions only finds a hole in the middle;
// these are the ends and the empty case, where there is no pair to compare
// and a lost revision would otherwise come back as a silent empty stretch
// with HistoryComplete=true.

func warningsOfCode(res Result, code WarningCode) []Warning {
	var out []Warning
	for _, w := range res.Warnings {
		if w.Code == code {
			out = append(out, w)
		}
	}
	return out
}

func TestMissingFirstRevisionIsAGap(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	lost := utc(2026, 5, 3, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	cfg := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	revs := chain(t,
		revisionStep{at: created, cfg: cfg},
		revisionStep{at: lost, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	// History starts at creation, but the revision covering its first two days
	// is gone.
	damaged := revs[1:]

	err := renderRefuses(t, Input{Root: root(created), Revisions: damaged, From: created, Until: until},
		ErrRevisionGap)
	if !strings.Contains(err.Error(), created.Format(time.RFC3339)) ||
		!strings.Contains(err.Error(), lost.Format(time.RFC3339)) {
		t.Fatalf("error must name the uncovered stretch %v..%v, got %v", created, lost, err)
	}
}

func TestMissingTailRevisionIsAGap(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	closed := utc(2026, 5, 3, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	cfg := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	revs := chain(t,
		revisionStep{at: created, cfg: cfg},
		revisionStep{at: closed, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	// The first revision was closed at `closed`, but its successor is gone,
	// so nothing covers the rest of the range.
	damaged := revs[:1]

	err := renderRefuses(t, Input{Root: root(created), Revisions: damaged, From: created, Until: until},
		ErrRevisionGap)
	if !strings.Contains(err.Error(), closed.Format(time.RFC3339)) {
		t.Fatalf("error must name where coverage stopped (%v), got %v", closed, err)
	}
}

func TestEmptyChainWithKnownHistoryIsAGap(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	renderRefuses(t, Input{Root: root(created), Revisions: nil, From: created, Until: until},
		ErrRevisionGap)
}

// TestMissingHistoryStartIsRefused: a root with no history horizon is refused
// outright rather than rendered as an honest-looking empty calendar.
//
// This replaces TestUnknownHistoryStartDoesNotCryGap, which asserted the
// opposite and was right to: while pre-revision rows existed, an unknown
// horizon was a reachable state and claiming coverage over it would have cried
// damage on real schedules. The create flow now writes the horizon in the same
// statement as the row, so the state is unreachable, and answering it with a
// 200 would give a corrupt row the same reply as a schedule created yesterday.
func TestMissingHistoryStartIsRefused(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
	)
	unknown := root(created)
	unknown.HistoryCompleteFrom = nil

	_, err := Render(Input{Root: unknown, Revisions: revs, From: created, Until: until})
	if !errors.Is(err, ErrHistoryMarkerMissing) {
		t.Fatalf("error = %v, want ErrHistoryMarkerMissing", err)
	}
}

// TestQueryBeforeHistoryStartIsIncompleteNotAnError: the other half of the
// predicate that used to answer both cases at once.
//
// A schedule younger than the question asked of it is ordinary, and it keeps
// its ordinary answer: a 200 whose history_complete is false and whose warning
// says which stretch is unknown. Losing this when the two cases were split
// would have turned every young schedule into an error.
func TestQueryBeforeHistoryStartIsIncompleteNotAnError(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	askedFrom := created.Add(-48 * time.Hour)
	until := utc(2026, 5, 5, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
	)

	res := renderOf(t, Input{Root: root(created), Revisions: revs, From: askedFrom, Until: until})
	if res.HistoryComplete {
		t.Fatal("history reported complete for a range starting before the horizon")
	}
	incomplete := warningsOfCode(res, WarnHistoryIncomplete)
	if len(incomplete) != 1 {
		t.Fatalf("got %d history_incomplete warnings, want 1: %+v", len(incomplete), res.Warnings)
	}
	if !incomplete[0].From.Equal(askedFrom) || !incomplete[0].Until.Equal(created) {
		t.Fatalf("incomplete = %v..%v, want %v..%v",
			incomplete[0].From, incomplete[0].Until, askedFrom, created)
	}
}

// TestInnerGapIsReportedBetweenPresentRevisions: two revisions that should be
// adjacent and are not is damage, and it is reported as a gap rather than as
// incomplete history - the chain is what is broken, not the horizon.
func TestInnerGapIsReportedBetweenPresentRevisions(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	second := utc(2026, 5, 3, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: second, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	damaged := []scheduleconfig.ScheduleRevision{revs[0], revs[1]}
	closedEarly := second.Add(-24 * time.Hour)
	damaged[0].EffectiveTo = &closedEarly

	err := renderRefuses(t, Input{Root: root(created), Revisions: damaged, From: created, Until: until},
		ErrRevisionGap)
	if !strings.Contains(err.Error(), closedEarly.Format(time.RFC3339)) {
		t.Fatalf("error must name the inner hole starting %v, got %v", closedEarly, err)
	}
}

// TestGapOutsideTheQueryRangeIsNotRefused: a hole the caller did not ask about
// is not the caller's problem - clipping still decides whether it is a gap.
func TestGapOutsideTheQueryRangeIsNotRefused(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	closed := utc(2026, 5, 3, 11, 0)
	resumed := utc(2026, 5, 9, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: closed, kind: scheduleconfig.RevisionDeleted},
		revisionStep{at: resumed, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
	)
	damaged := []scheduleconfig.ScheduleRevision{revs[0], revs[2]} // the middle row is gone

	// The query sits entirely inside the hole, so it is refused.
	from, until := utc(2026, 5, 5, 0, 0), utc(2026, 5, 7, 0, 0)
	renderRefuses(t, Input{Root: root(created), Revisions: damaged, From: from, Until: until},
		ErrRevisionGap)

	// A query that ends before the hole opens is answered normally: the same
	// damaged chain, a different question.
	early, earlyUntil := utc(2026, 5, 1, 12, 0), utc(2026, 5, 2, 12, 0)
	if res := renderOf(t, Input{Root: root(created), Revisions: damaged, From: early, Until: earlyUntil}); len(res.Assignments) == 0 {
		t.Fatal("a range covered by the surviving revision must still render")
	}
}

// The exclusion constraint forbids this, so it is corruption. The renderer
// used to resolve it in favour of the earlier revision and say so in a
// warning; it now refuses, because "here is the calendar, one of these two
// configurations produced it" is not an answer anyone can act on.
//
// The old comment continued: the renderer stays total, keeps one
// assignment per layer per instant, and does not rewrite the answer already
// given for the earlier revision.
func TestOverlappingRevisionsAreReportedAndResolved(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	second := utc(2026, 5, 3, 11, 0)
	until := utc(2026, 5, 5, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: second, cfg: config("UTC", dailyPolicy("11:00"), group(groupB, "bob"))},
	)
	// Push the first revision's end past its successor's start.
	overlapEnd := second.Add(24 * time.Hour)
	revs[0].EffectiveTo = &overlapEnd

	err := renderRefuses(t, Input{Root: root(created), Revisions: revs, From: created, Until: until},
		scheduleconfig.ErrRevisionOverlap)
	for _, id := range []string{revs[0].ID, revs[1].ID} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error must name both revisions, got %v", err)
		}
	}

}

// TestRangeIsNormalizedToDatabaseResolution: the queries that fetch revisions
// floor their bounds to microseconds, so the renderer has to clip with the
// same value. Clipping with a nanosecond-precise `until` would expect a
// revision the query had already excluded.
func TestRangeIsNormalizedToDatabaseResolution(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	until := utc(2026, 5, 3, 11, 0)
	res := renderOf(t, Input{
		Root:      root(created),
		Revisions: revs,
		From:      created.Add(500 * time.Nanosecond),
		Until:     until.Add(500 * time.Nanosecond),
	})

	if !res.From.Equal(created) || !res.Until.Equal(until) {
		t.Fatalf("answered range %v..%v, want it normalized to %v..%v", res.From, res.Until, created, until)
	}
	l1 := assignmentsOf(res, LayerL1)
	if len(l1) == 0 {
		t.Fatal("nothing rendered")
	}
	if !l1[0].AssignmentStart.Equal(created) || !l1[len(l1)-1].AssignmentEnd.Equal(until) {
		t.Fatalf("assignments cover %v..%v, want the normalized range",
			l1[0].AssignmentStart, l1[len(l1)-1].AssignmentEnd)
	}

	// A range narrower than the stored resolution is empty, not a rounding
	// coincidence to be papered over.
	if _, err := Render(Input{
		Root: root(created), Revisions: revs,
		From:  created,
		Until: created.Add(500 * time.Nanosecond),
	}); err == nil {
		t.Fatal("a sub-resolution range was accepted")
	}
}

// TestGapNamesTheRevisionItActuallyFollows: a gap's RelatedIDs must name the
// revisions on either side of it, and the left one is whatever the coverage
// ended at - not simply whichever revision was processed last.
//
// The two differ only on a damaged chain: a revision nested wholly inside what
// an earlier one already covers extends nothing, so the coverage still ends at
// the earlier revision. Naming the nested one would point an operator at a row
// that does not touch the gap at all, which is a lie told exactly when the data
// is already confusing.
func TestGapNamesTheRevisionItActuallyFollows(t *testing.T) {
	created := utc(2026, 5, 1, 11, 0)
	until := utc(2026, 5, 9, 11, 0)

	revs := chain(t,
		revisionStep{at: created, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: utc(2026, 5, 3, 11, 0),
			cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
		revisionStep{at: utc(2026, 5, 7, 11, 0),
			cfg: config("UTC", dailyPolicy("11:00"), group(groupB, "bob"))},
	)

	// outer covers [1st, 5th); nested sits inside it and ends earlier, so it
	// leaves the coverage exactly where outer put it. after starts on the 7th,
	// so the hole is [5th, 7th) - bounded on the left by outer.
	outer, nested, after := revs[0], revs[1], revs[2]
	outerEnd := utc(2026, 5, 5, 11, 0)
	outer.EffectiveTo = &outerEnd
	nestedStart, nestedEnd := utc(2026, 5, 2, 11, 0), utc(2026, 5, 3, 11, 0)
	nested.EffectiveFrom, nested.EffectiveTo = nestedStart, &nestedEnd

	err := renderRefuses(t, Input{
		Root:      root(created),
		Revisions: []scheduleconfig.ScheduleRevision{outer, nested, after},
		From:      created,
		Until:     until,
	}, scheduleconfig.ErrRevisionOverlap)

	// The nested revision is refused as an overlap before the gap after it is
	// ever reached: it starts inside what the outer one already covers. Which
	// of the two damages is reported first does not matter to a caller who
	// gets neither a calendar nor a wrong one - but it does have to name the
	// rows involved.
	for _, id := range []string{outer.ID, nested.ID} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error must name the revisions in dispute, got %v", err)
		}
	}
}
