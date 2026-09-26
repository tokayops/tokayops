package model

import (
	"regexp"
	"time"
)

// e164 is the shape of a phone number this system stores and dials: a plus,
// a country code that does not start with zero, and at most fifteen digits in
// all. It checks the form only. Whether the number exists is for the call that
// verifies it to find out - the provider refuses a number it cannot dial.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// ValidE164 reports whether s is a phone number in E.164 form.
func ValidE164(s string) bool { return e164.MatchString(s) }

// PhoneContact is a person's phone number and what is known about it.
type PhoneContact struct {
	Value      string     `json:"value"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// PinnedIntegrationID is the provider the person prefers to be called
	// through. A preference and not a ban: when it no longer covers the number
	// or is switched off, the others are used in their order.
	PinnedIntegrationID string    `json:"pinned_integration_id,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Verified reports whether the person has proved the number is theirs.
func (c *PhoneContact) Verified() bool { return c != nil && c.VerifiedAt != nil }
