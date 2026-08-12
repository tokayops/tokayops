// Package rotation is the pure domain core for schedule revisions: handoff
// grid math, rotation position, and transition planning. It depends only on
// internal/model and must never import store or API code. It is the only
// rotation math in the product - internal/schedulerender projects revisions
// through it, and every consumer reaches it that way.
package rotation

import (
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// RotationPolicy is the immutable per-layer rotation policy stored inside a
// schedule revision snapshot. Timezone deliberately lives on the snapshot
// top-level, not here: both layers of one revision share a single timezone.
type RotationPolicy struct {
	Cadence       model.RotationType `json:"cadence"`      // daily | weekly
	HandoffTime   string             `json:"handoff_time"` // local, canonical "HH:MM"
	HandoffDay    *int               `json:"handoff_day"`  // weekly: 0..6, 0=Sunday; daily: nil
}

// ParseHandoffTime parses a canonical "HH:MM" local handoff time. Unlike the
// legacy scheduler parser, garbage is an error, never a silent default; a
// missing leading zero ("9:00") is rejected so that string equality of
// handoff times is semantic equality.
func ParseHandoffTime(s string) (hour, minute int, err error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("rotation: handoff time %q is not canonical HH:MM", s)
	}
	for _, i := range [4]int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, fmt.Errorf("rotation: handoff time %q is not canonical HH:MM", s)
		}
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	minute = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, 0, fmt.Errorf("rotation: handoff time %q is out of range", s)
	}
	return hour, minute, nil
}

// Validate checks the policy in its canonical form: daily policies carry a
// nil handoff day (normalization converts before validation).
func (p RotationPolicy) Validate() error {
	if _, _, err := ParseHandoffTime(p.HandoffTime); err != nil {
		return err
	}
	switch p.Cadence {
	case model.RotationDaily:
		if p.HandoffDay != nil {
			return fmt.Errorf("rotation: daily policy must have nil handoff_day, got %d", *p.HandoffDay)
		}
	case model.RotationWeekly:
		if p.HandoffDay == nil {
			return fmt.Errorf("rotation: weekly policy requires handoff_day")
		}
		if *p.HandoffDay < 0 || *p.HandoffDay > 6 {
			return fmt.Errorf("rotation: handoff_day %d out of range 0..6", *p.HandoffDay)
		}
	default:
		return fmt.Errorf("rotation: unknown cadence %q", p.Cadence)
	}
	return nil
}

func (p RotationPolicy) clone() RotationPolicy {
	c := p
	if p.HandoffDay != nil {
		d := *p.HandoffDay
		c.HandoffDay = &d
	}
	return c
}

// equalPolicy compares policies semantically on their canonical forms.
func equalPolicy(a, b RotationPolicy) bool {
	if a.Cadence != b.Cadence || a.HandoffTime != b.HandoffTime {
		return false
	}
	switch {
	case a.HandoffDay == nil && b.HandoffDay == nil:
		return true
	case a.HandoffDay != nil && b.HandoffDay != nil:
		return *a.HandoffDay == *b.HandoffDay
	default:
		return false
	}
}

func validateTimezone(tz string) (*time.Location, error) {
	if tz == "" {
		// time.LoadLocation("") silently returns UTC; an empty timezone in a
		// snapshot is a data error, not an alias for UTC.
		return nil, fmt.Errorf("rotation: empty timezone")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("rotation: invalid timezone %q: %w", tz, err)
	}
	return loc, nil
}
