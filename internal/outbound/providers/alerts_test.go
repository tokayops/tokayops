package providers

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// TestFiringAlertsComeFirst. The reading order puts what is still firing
// before what has resolved and keeps the snapshot's order inside each: a
// resolved alert that started first is read after every firing one.
func TestFiringAlertsComeFirst(t *testing.T) {
	at := func(minutes int) time.Time { return time.Unix(1700000000+int64(minutes)*60, 0).UTC() }
	alerts := []keys.AlertSnapshot{
		{Fingerprint: "a", Status: keys.AlertResolved, StartsAt: at(0), AlertName: "resolved-first"},
		{Fingerprint: "b", Status: keys.AlertFiring, StartsAt: at(1), AlertName: "firing-second"},
		{Fingerprint: "c", Status: keys.AlertResolved, StartsAt: at(2), AlertName: "resolved-third"},
		{Fingerprint: "d", Status: keys.AlertFiring, StartsAt: at(3), AlertName: "firing-fourth"},
	}
	var got []string
	for _, a := range FiringFirst(alerts) {
		got = append(got, a.AlertName)
	}
	want := []string{"firing-second", "firing-fourth", "resolved-first", "resolved-third"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("the reading order is %v, want %v", got, want)
		}
	}
	if alerts[0].AlertName != "resolved-first" {
		t.Fatal("the order of the snapshot itself was changed")
	}
}
