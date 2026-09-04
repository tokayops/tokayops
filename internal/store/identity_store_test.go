package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// TestStore_IdentityLink_HappyPath issues a Slack OTP via IssueLinkToken and
// confirms it via ConfirmIdentityLink - what replaced SaveSlackOTP/ConfirmSlackOTP.
// `testutil.SeedUser` itself binds a `slack` identity, so we use a fresh provider name
// to exercise the link flow end-to-end without conflicting with the seed binding.
func TestStore_IdentityLink_HappyPath(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "link-happy@example.com")
	const provider = "slacktest"
	const externalID = "U_LINK_HAPPY"

	if err := s.IssueLinkToken(user.ID, provider, externalID, "654321", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}

	ident, err := s.ConfirmIdentityLink(user.ID, provider, "654321")
	if err != nil {
		t.Fatalf("ConfirmIdentityLink: %v", err)
	}
	if ident.ExternalID != externalID {
		t.Errorf("got external id %q, want %q", ident.ExternalID, externalID)
	}

	// The link token is single-use.
	if _, err := s.ConfirmIdentityLink(user.ID, provider, "654321"); !errors.Is(err, store.ErrLinkTokenInvalid) {
		t.Errorf("second confirm should be invalid, got %v", err)
	}

	// The identity is queryable both directions.
	got, err := s.GetExternalIdentity(user.ID, provider)
	if err != nil || got.ExternalID != externalID {
		t.Errorf("GetExternalIdentity: %v / %+v", err, got)
	}
	gotUser, err := s.GetUserByExternalID(provider, externalID)
	if err != nil || gotUser.ID != user.ID {
		t.Errorf("GetUserByExternalID: %v / %+v", err, gotUser)
	}
}

// TestStore_IdentityLink_AttemptsLockout verifies the 3-wrong-code lockout.
func TestStore_IdentityLink_AttemptsLockout(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "link-attempts@example.com")
	const provider = "lockout"

	if err := s.IssueLinkToken(user.ID, provider, "U_LO", "999999", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}
	// Two wrong attempts: invalid each time.
	for i := 0; i < 2; i++ {
		if _, err := s.ConfirmIdentityLink(user.ID, provider, "000000"); !errors.Is(err, store.ErrLinkTokenInvalid) {
			t.Errorf("attempt %d: expected ErrLinkTokenInvalid, got %v", i+1, err)
		}
	}
	// Third wrong attempt locks out (deletes row, returns expired).
	if _, err := s.ConfirmIdentityLink(user.ID, provider, "000000"); !errors.Is(err, store.ErrLinkTokenExpired) {
		t.Errorf("third attempt should lock out: got %v", err)
	}
	// Correct code now fails because the token was deleted on lockout.
	if _, err := s.ConfirmIdentityLink(user.ID, provider, "999999"); !errors.Is(err, store.ErrLinkTokenInvalid) {
		t.Errorf("after lockout, even the correct code is invalid: got %v", err)
	}
}

// TestStore_IdentityLink_Expired enforces TTL.
func TestStore_IdentityLink_Expired(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "link-expired@example.com")
	const provider = "expired"

	if err := s.IssueLinkToken(user.ID, provider, "U_EXP", "111111", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}
	if _, err := s.ConfirmIdentityLink(user.ID, provider, "111111"); !errors.Is(err, store.ErrLinkTokenExpired) {
		t.Errorf("expected ErrLinkTokenExpired, got %v", err)
	}
}

// TestStore_BindExternalIdentity_ConflictAcrossUsers verifies the global
// uniqueness of (provider, external_id).
func TestStore_BindExternalIdentity_ConflictAcrossUsers(t *testing.T) {
	s := testutil.SetupDB(t)
	u1 := testutil.SeedUser(t, s, "conflict-u1@example.com")
	u2 := testutil.SeedUser(t, s, "conflict-u2@example.com")

	const provider = "conflict"
	const externalID = "U_CONFLICT"

	if err := s.IssueLinkToken(u1.ID, provider, externalID, "222222", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken u1: %v", err)
	}
	if _, err := s.ConfirmIdentityLink(u1.ID, provider, "222222"); err != nil {
		t.Fatalf("ConfirmIdentityLink u1: %v", err)
	}

	// u2 tries to claim the same external id.
	if err := s.IssueLinkToken(u2.ID, provider, externalID, "333333", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken u2: %v", err)
	}
	if _, err := s.ConfirmIdentityLink(u2.ID, provider, "333333"); !errors.Is(err, store.ErrExternalIdentityAlreadyLinked) {
		t.Errorf("expected ErrExternalIdentityAlreadyLinked, got %v", err)
	}
}

// TestStore_BindIfAbsent_GuardSemantics mirrors the old UpdateUserSlackID guard:
// existing identity for same user → (false, nil); external_id taken by another → typed conflict.
func TestStore_BindIfAbsent_GuardSemantics(t *testing.T) {
	s := testutil.SetupDB(t)
	u1 := testutil.SeedUser(t, s, "guard-u1@example.com")
	u2 := testutil.SeedUser(t, s, "guard-u2@example.com")

	const provider = "guard"

	// First bind succeeds.
	changed, err := s.BindExternalIdentityIfAbsent(u1.ID, provider, "U_GUARD_A", "")
	if err != nil || !changed {
		t.Fatalf("first bind: changed=%v err=%v", changed, err)
	}
	// Same user, second call - no-op (don't overwrite).
	changed, err = s.BindExternalIdentityIfAbsent(u1.ID, provider, "U_GUARD_B", "")
	if err != nil || changed {
		t.Errorf("re-bind for same user should be no-op: changed=%v err=%v", changed, err)
	}
	if got, _ := s.GetExternalIdentity(u1.ID, provider); got.ExternalID != "U_GUARD_A" {
		t.Errorf("identity must not be overwritten: %+v", got)
	}
	// Other user claiming an external id already linked → typed conflict.
	changed, err = s.BindExternalIdentityIfAbsent(u2.ID, provider, "U_GUARD_A", "")
	if changed || !errors.Is(err, store.ErrExternalIdentityAlreadyLinked) {
		t.Errorf("cross-user conflict expected: changed=%v err=%v", changed, err)
	}
}

// TestStore_BindIfAbsent_DoesNotOverwrite_RealDB verifies the atomic
// `INSERT ... ON CONFLICT (user_id, provider) DO NOTHING` semantics - the previous
// check-then-bind implementation overwrote an existing identity via DO UPDATE.
func TestStore_BindIfAbsent_DoesNotOverwrite_RealDB(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "no-overwrite@example.com")
	const provider = "slacktest-noov"

	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: user.ID, Provider: provider, ExternalID: "U_ORIGINAL",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	changed, err := s.BindExternalIdentityIfAbsent(user.ID, provider, "U_OVERWRITE_ATTEMPT", "")
	if err != nil {
		t.Fatalf("BindExternalIdentityIfAbsent: %v", err)
	}
	if changed {
		t.Fatal("BindExternalIdentityIfAbsent must NOT overwrite an existing identity for the same user")
	}

	got, err := s.GetExternalIdentity(user.ID, provider)
	if err != nil {
		t.Fatalf("GetExternalIdentity: %v", err)
	}
	if got.ExternalID != "U_ORIGINAL" {
		t.Fatalf("external_id was overwritten: got %q, want %q", got.ExternalID, "U_ORIGINAL")
	}
}

// TestStore_UnbindExternalIdentity removes the (user, provider) row.
func TestStore_UnbindExternalIdentity(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "unbind@example.com")
	const provider = "unbind"

	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: user.ID, Provider: provider, ExternalID: "U_UNBIND",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}
	if got, _ := s.GetExternalIdentity(user.ID, provider); got == nil {
		t.Fatalf("setup: identity not bound")
	}
	if err := s.UnbindExternalIdentity(user.ID, provider); err != nil {
		t.Fatalf("UnbindExternalIdentity: %v", err)
	}
	if _, err := s.GetExternalIdentity(user.ID, provider); err == nil {
		t.Errorf("expected identity gone after unbind")
	}
}

// TestStore_ConsumeLinkToken_DeepLink issues a deep-link token (empty external_id)
// and consumes it bot-side by (provider, token), writing chat_id/display_name - the
// path where the bot has no logged-in user.
func TestStore_ConsumeLinkToken_DeepLink(t *testing.T) {
	s := testutil.SetupDB(t)
	user := testutil.SeedUser(t, s, "consume-happy@example.com")
	const provider = "tgtest"
	const token = "deeplink-secret-abc"

	// Issue with empty external_id - from.id is unknown until /start.
	if err := s.IssueLinkToken(user.ID, provider, "", token, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}

	ident, err := s.ConsumeLinkToken(provider, token, "TG_123", "TG_CHAT_123", "Alice")
	if err != nil {
		t.Fatalf("ConsumeLinkToken: %v", err)
	}
	if ident.ExternalID != "TG_123" || ident.ChatID != "TG_CHAT_123" || ident.DisplayName != "Alice" {
		t.Errorf("identity = %+v, want external/chat/name TG_123/TG_CHAT_123/Alice", ident)
	}

	// Single-use.
	if _, err := s.ConsumeLinkToken(provider, token, "TG_123", "TG_CHAT_123", "Alice"); !errors.Is(err, store.ErrLinkTokenInvalid) {
		t.Errorf("second consume should be invalid, got %v", err)
	}

	// Reverse lookup (used by the webhook to authenticate callbacks) + persistence.
	gotUser, err := s.GetUserByExternalID(provider, "TG_123")
	if err != nil || gotUser.ID != user.ID {
		t.Errorf("GetUserByExternalID: %v / %+v", err, gotUser)
	}
	got, _ := s.GetExternalIdentity(user.ID, provider)
	if got == nil || got.ChatID != "TG_CHAT_123" || got.DisplayName != "Alice" {
		t.Errorf("persisted identity = %+v, want chat_id/display_name set", got)
	}
}

// TestStore_ConsumeLinkToken_Errors covers expired, wrong-token, and cross-user conflict.
func TestStore_ConsumeLinkToken_Errors(t *testing.T) {
	s := testutil.SetupDB(t)

	t.Run("expired", func(t *testing.T) {
		user := testutil.SeedUser(t, s, "consume-expired@example.com")
		const provider = "tgexp"
		if err := s.IssueLinkToken(user.ID, provider, "", "expired-tok", time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("IssueLinkToken: %v", err)
		}
		if _, err := s.ConsumeLinkToken(provider, "expired-tok", "TG_X", "C_X", "X"); !errors.Is(err, store.ErrLinkTokenExpired) {
			t.Errorf("expected ErrLinkTokenExpired, got %v", err)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		user := testutil.SeedUser(t, s, "consume-wrong@example.com")
		const provider = "tgwrong"
		if err := s.IssueLinkToken(user.ID, provider, "", "right-tok", time.Now().Add(5*time.Minute)); err != nil {
			t.Fatalf("IssueLinkToken: %v", err)
		}
		if _, err := s.ConsumeLinkToken(provider, "wrong-tok", "TG_Y", "C_Y", "Y"); !errors.Is(err, store.ErrLinkTokenInvalid) {
			t.Errorf("expected ErrLinkTokenInvalid, got %v", err)
		}
	})

	t.Run("cross-user conflict", func(t *testing.T) {
		userA := testutil.SeedUser(t, s, "consume-a@example.com")
		userB := testutil.SeedUser(t, s, "consume-b@example.com")
		const provider = "tgconflict"
		if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: userA.ID, Provider: provider, ExternalID: "TG_SHARED"}); err != nil {
			t.Fatalf("BindExternalIdentity: %v", err)
		}
		if err := s.IssueLinkToken(userB.ID, provider, "", "tokB", time.Now().Add(5*time.Minute)); err != nil {
			t.Fatalf("IssueLinkToken: %v", err)
		}
		if _, err := s.ConsumeLinkToken(provider, "tokB", "TG_SHARED", "C_SHARED", "B"); !errors.Is(err, store.ErrExternalIdentityAlreadyLinked) {
			t.Errorf("expected ErrExternalIdentityAlreadyLinked, got %v", err)
		}
	})
}

// TestMockStore_ConsumeLinkToken is the no-DB mirror so webhook-handler unit tests
// (which use MockStore) exercise the same contract.
func TestMockStore_ConsumeLinkToken(t *testing.T) {
	s := store.NewMockStore()
	u := &model.User{ID: "u1", Email: "m@example.com", Name: "M"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const provider = "telegram"
	if err := s.IssueLinkToken(u.ID, provider, "", "tok-1", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}
	ident, err := s.ConsumeLinkToken(provider, "tok-1", "TG_1", "C_1", "Mock")
	if err != nil {
		t.Fatalf("ConsumeLinkToken: %v", err)
	}
	if ident.ExternalID != "TG_1" || ident.ChatID != "C_1" || ident.DisplayName != "Mock" {
		t.Errorf("ident = %+v", ident)
	}
	if gotUser, err := s.GetUserByExternalID(provider, "TG_1"); err != nil || gotUser.ID != u.ID {
		t.Errorf("GetUserByExternalID: %v / %+v", err, gotUser)
	}
	if _, err := s.ConsumeLinkToken(provider, "tok-1", "TG_1", "C_1", "Mock"); !errors.Is(err, store.ErrLinkTokenInvalid) {
		t.Errorf("second consume should be invalid, got %v", err)
	}
}

// TestTheIdentityLookupHonoursItsDeadline. Resolving a person's address is the
// slow half of preparing an attempt, and it runs under a deadline
// (outbound.NotificationPrepareDeadline) so a database that hangs costs one
// delivery slot for five seconds rather than for the length of a lease.
//
// Handed a context the query never passes on, that deadline is a comment: the
// call blocks for as long as the database wants, and the worker's slot goes
// with it. This asks the cheapest question that tells the two apart - a context
// that is already cancelled has to come back as cancelled, not as "no such
// identity".
func TestTheIdentityLookupHonoursItsDeadline(t *testing.T) {
	s := testutil.SetupDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.GetExternalIdentityContext(ctx, "some-user", "slack")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled lookup answered %v, so the preparation deadline "+
			"never reaches the database", err)
	}
}
