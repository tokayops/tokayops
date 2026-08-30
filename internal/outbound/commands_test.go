package outbound

import "testing"

// TestAnAdmissionThatPromisedNothingIsCountedAsSuch. Both are successes as far
// as the store is concerned - the escalation was admitted, the group is out of
// the loop - and only one of them means somebody was paged. The alert on this
// counter is about exactly that difference, so the label has to draw it.
func TestAnAdmissionThatPromisedNothingIsCountedAsSuch(t *testing.T) {
	cases := []struct {
		outcome     SubmitOutcome
		commitments int
		want        string
	}{
		{SubmitCreated, 3, "created"},
		{SubmitCreated, 0, "no_targets"},
		{SubmitExisting, 3, "existing"},
		{SubmitConflict, 3, "conflict"},
		{SubmitSourceChanged, 3, "source_changed"},
		{SubmitGroupNotAdmitted, 3, "group_not_admitted"},

		// A repeat of an admission that promised nothing is still a repeat:
		// nobody was paged the first time either, and the counter would say so
		// twice for one alert.
		{SubmitExisting, 0, "existing"},
	}

	for _, tt := range cases {
		if got := AdmissionLabel(tt.outcome, tt.commitments); got != tt.want {
			t.Errorf("%s with %d commitments counted as %q, want %q",
				tt.outcome, tt.commitments, got, tt.want)
		}
	}
}
