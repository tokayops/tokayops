package dispatcher

import (
	"errors"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	telegramprovider "github.com/tokayops/tokayops/internal/outbound/providers/telegram"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestResolveRecipient_Slack(t *testing.T) {
	s := store.NewMockStore()
	s.CreateUser(&model.User{ID: "u1", Name: "Alice"})
	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: "u1", Provider: "slack", ExternalID: "U1",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	id, name, err := resolveRecipient(s, "slack", "u1")
	if err != nil {
		t.Fatalf("resolveRecipient: %v", err)
	}
	if id != "U1" || name != "Alice" {
		t.Fatalf("got id=%q name=%q, want U1/Alice", id, name)
	}
}

func TestResolveRecipient_NoIdentity_Permanent(t *testing.T) {
	s := store.NewMockStore()
	s.CreateUser(&model.User{ID: "u1", Name: "Bob"})

	_, _, err := resolveRecipient(s, "slack", "u1")
	if !errors.Is(err, ErrIdentityNotLinked) {
		t.Fatalf("expected ErrIdentityNotLinked, got %v", err)
	}
	if !isPermanentError(err) {
		t.Fatal("ErrIdentityNotLinked must be a permanent error")
	}
}

// TestResolveRecipient_Generic_AnyProvider replaces the old
// TestResolveRecipient_UnknownProvider_Permanent. resolveRecipient is generic
// over external_identities now, so an unknown provider
// is the same shape of failure as "no identity for known provider" - the user
// is simply not linked to that provider, regardless of whether that provider
// is registered with the dispatcher.
func TestResolveRecipient_Generic_AnyProvider(t *testing.T) {
	s := store.NewMockStore()
	s.CreateUser(&model.User{ID: "u1", Name: "Bob"})
	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: "u1", Provider: "telegram", ExternalID: "TG_1",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	id, name, err := resolveRecipient(s, "telegram", "u1")
	if err != nil {
		t.Fatalf("resolveRecipient(telegram): %v", err)
	}
	if id != "TG_1" || name != "Bob" {
		t.Fatalf("got id=%q name=%q, want TG_1/Bob", id, name)
	}

	// A provider the user is not linked to → ErrIdentityNotLinked (permanent).
	_, _, err = resolveRecipient(s, "discord", "u1")
	if !errors.Is(err, ErrIdentityNotLinked) {
		t.Fatalf("expected ErrIdentityNotLinked, got %v", err)
	}
	if !isPermanentError(err) {
		t.Fatal("ErrIdentityNotLinked must be a permanent error")
	}
}

// TestAChannelWithNoTokenIsPermanent keeps the classification where the rule
// lives. A missing integration token is a configuration fault: retrying it
// occupies a worker and changes nothing until somebody sets the token.
func TestAChannelWithNoTokenIsPermanent(t *testing.T) {
	for name, err := range map[string]error{
		"slack":    slackprovider.ErrNoToken,
		"telegram": telegramprovider.ErrNoToken,
	} {
		if !isPermanentError(err) {
			t.Errorf("%s: a missing token is retried forever", name)
		}
	}
}
