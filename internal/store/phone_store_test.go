package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
)

const (
	phoneA = "+14155550101"
	phoneB = "+14155550102"
)

// twilioIntegration stores an enabled Twilio integration and returns its id.
func twilioIntegration(t *testing.T, s *Store, from string, coverage ...string) string {
	t.Helper()
	cfg, _ := json.Marshal(model.TwilioConfig{
		AccountSID: "AC" + fmt.Sprintf("%032x", 1), AuthToken: "secret", FromNumber: from,
		Coverage: coverage, CPS: 1, MaxConcurrent: 1,
	})
	in := &model.Integration{
		ID: uuid.New().String(), Type: model.IntegrationTypeTwilio, Direction: model.IntegrationDirectionOutbound,
		Name: "twilio " + from, Enabled: true, Config: cfg, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateIntegration(in); err != nil {
		t.Fatalf("create twilio integration: %v", err)
	}
	return in.ID
}

func mustSetPhone(t *testing.T, s *Store, userID, value string) *model.PhoneContact {
	t.Helper()
	c, err := s.SetPhoneContact(context.Background(), userID, value)
	if err != nil {
		t.Fatalf("set phone %s: %v", value, err)
	}
	return c
}

func mustVerifyPhone(t *testing.T, s *Store, userID, number string) {
	t.Helper()
	ctx := context.Background()
	if err := s.IssuePhoneCode(ctx, userID, number, "424242", time.Minute); err != nil {
		t.Fatalf("issue code: %v", err)
	}
	if err := s.ConfirmPhoneCode(ctx, userID, "424242"); err != nil {
		t.Fatalf("confirm code: %v", err)
	}
}

// callTo is a request for a call to number on its own account, so that only
// the person's lock stands between requests about the same person.
func callTo(userID, purpose, number, from string) PhoneCallRequest {
	return PhoneCallRequest{
		UserID: userID, Purpose: purpose, ToNumber: number, FromNumber: from,
		AccountScope: "AC-" + uuid.New().String(), CPS: 1000, MaxConcurrent: 1000, Hold: time.Minute,
	}
}

// A different number starts unverified and unpinned: the proof and the pin were
// about the old one. The same number written again keeps both.
func TestAnotherNumberForgetsTheProofAndThePin(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	pin := twilioIntegration(t, s, "+15005550006", "+1")

	mustSetPhone(t, s, "alice", phoneA)
	mustVerifyPhone(t, s, "alice", phoneA)
	if err := s.PinPhoneIntegration(ctx, "alice", phoneA, pin); err != nil {
		t.Fatalf("pin: %v", err)
	}

	same := mustSetPhone(t, s, "alice", phoneA)
	if !same.Verified() || same.PinnedIntegrationID != pin {
		t.Fatalf("the same number lost what it had: %+v", same)
	}

	other := mustSetPhone(t, s, "alice", phoneB)
	if other.Verified() {
		t.Fatal("a new number carried the old one's proof")
	}
	if other.PinnedIntegrationID != "" {
		t.Fatal("a new number carried the old one's pin")
	}
}

// A pin checked against one number never lands on another.
func TestAPinForAnEarlierNumberIsRefused(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "alice")
	pin := twilioIntegration(t, s, "+15005550006", "+1")
	mustSetPhone(t, s, "alice", phoneB)

	err := s.PinPhoneIntegration(context.Background(), "alice", phoneA, pin)
	if !errors.Is(err, ErrPhoneContactChanged) {
		t.Fatalf("pin for a number the person no longer has = %v, want ErrPhoneContactChanged", err)
	}
}

// The code is bound to the number it was spoken to.
func TestACodeSpokenToAnEarlierNumberVerifiesNothing(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)
	if err := s.IssuePhoneCode(ctx, "alice", phoneA, "111111", time.Minute); err != nil {
		t.Fatalf("issue: %v", err)
	}
	mustSetPhone(t, s, "alice", phoneB)

	if err := s.ConfirmPhoneCode(ctx, "alice", "111111"); !errors.Is(err, ErrLinkTokenInvalid) {
		t.Fatalf("code of the earlier number = %v, want ErrLinkTokenInvalid", err)
	}
	if c, _ := s.GetPhoneContact(ctx, "alice"); c.Verified() {
		t.Fatal("the new number was verified with the old number's code")
	}
}

func TestThreeWrongCodesEndTheCode(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)
	if err := s.IssuePhoneCode(ctx, "alice", phoneA, "123456", time.Minute); err != nil {
		t.Fatalf("issue: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ConfirmPhoneCode(ctx, "alice", "000000"); !errors.Is(err, ErrLinkTokenInvalid) {
			t.Fatalf("wrong code %d = %v, want ErrLinkTokenInvalid", i+1, err)
		}
	}
	if err := s.ConfirmPhoneCode(ctx, "alice", "000000"); !errors.Is(err, ErrLinkTokenExpired) {
		t.Fatalf("third wrong code = %v, want ErrLinkTokenExpired", err)
	}
	if err := s.ConfirmPhoneCode(ctx, "alice", "123456"); err == nil {
		t.Fatal("the right code worked after three wrong ones")
	}
}

// The expiry is the database's clock, not the process's.
func TestAnExpiredCodeVerifiesNothing(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)
	if err := s.IssuePhoneCode(ctx, "alice", phoneA, "123456", time.Minute); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE link_tokens SET expires_at = now() - interval '1 second'
		WHERE user_id = 'alice' AND provider = 'phone'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPhoneCode(ctx, "alice", "123456"); !errors.Is(err, ErrLinkTokenExpired) {
		t.Fatalf("expired code = %v, want ErrLinkTokenExpired", err)
	}
}

// Two tabs or a double click must not both get under the allowance. The
// requests go through different accounts, so nothing but the person's own lock
// stands between them.
func TestTheAllowanceHoldsUnderConcurrentRequests(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)

	const requests = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	start := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// A test call per sender, each its own account: none waits on another's
			// call in flight or on another's capacity.
			req := callTo("alice", "dnd_check", phoneA, fmt.Sprintf("+1500555%04d", i))
			if _, err := s.ReservePhoneCall(context.Background(), req); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			} else if refused := (*PhoneCallRefused)(nil); !errors.As(err, &refused) {
				t.Errorf("request %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if granted != phoneCallsPerDay {
		t.Fatalf("%d calls granted, the allowance is %d", granted, phoneCallsPerDay)
	}
}

// One code call at a time: a second one would replace the code the person is
// listening to.
func TestASecondCodeCallWaitsForTheFirst(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)

	first, err := s.ReservePhoneCall(ctx, callTo("alice", "verify", phoneA, "+15005550006"))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err = s.ReservePhoneCall(ctx, callTo("alice", "verify", phoneA, "+15005550006"))
	var refused *PhoneCallRefused
	if !errors.As(err, &refused) || refused.Reason != PhoneRefusedInFlight {
		t.Fatalf("second call while the first rings = %v, want in_flight", err)
	}

	// A call known not to exist gives its place back.
	if err := s.ReleasePhoneReservation(ctx, first.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.ReservePhoneCall(ctx, callTo("alice", "verify", phoneA, "+15005550006")); err != nil {
		t.Fatalf("call after the first was given back: %v", err)
	}
}

// No more calls alive on one account than it allows, whoever asks. Different
// people, so the person's lock serialises nothing here: only the account's row
// does.
func TestAnAccountNeverHasMoreCallsThanItAllows(t *testing.T) {
	s := setupTestDB(t)
	const people, limit = 12, 3
	account := "AC-" + uuid.New().String()
	ids := make([]string, people)
	for i := range ids {
		ids[i] = fmt.Sprintf("person-%d", i)
	}
	seedUsers(t, s, ids...)
	for _, id := range ids {
		mustSetPhone(t, s, id, phoneA)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			req := callTo(id, "verify", phoneA, "+15005550006")
			req.AccountScope, req.MaxConcurrent = account, limit
			if _, err := s.ReservePhoneCall(context.Background(), req); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(id)
	}
	close(start)
	wg.Wait()

	var live int
	if err := s.db.QueryRow(`SELECT count(*) FROM call_reservations
		WHERE account_scope = $1 AND released_at IS NULL AND hold_until > clock_timestamp()`, account).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if granted > limit || live > limit {
		t.Fatalf("%d calls granted and %d alive on an account that allows %d", granted, live, limit)
	}
	if granted == 0 {
		t.Fatal("no call was granted at all")
	}
}

func TestAnAccountKeepsItsRate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice", "bob")
	mustSetPhone(t, s, "alice", phoneA)
	mustSetPhone(t, s, "bob", phoneB)
	account := "AC-" + uuid.New().String()

	first := callTo("alice", "verify", phoneA, "+15005550006")
	first.AccountScope, first.CPS = account, 1
	if _, err := s.ReservePhoneCall(ctx, first); err != nil {
		t.Fatalf("first call: %v", err)
	}
	second := callTo("bob", "verify", phoneB, "+15005550006")
	second.AccountScope, second.CPS = account, 1
	_, err := s.ReservePhoneCall(ctx, second)
	var refused *PhoneCallRefused
	if !errors.As(err, &refused) || refused.Reason != PhoneRefusedBusy {
		t.Fatalf("a second call in the same second = %v, want busy", err)
	}
	if refused.RetryAfter <= 0 || refused.RetryAfter > time.Second {
		t.Fatalf("retry after %s, want within the second", refused.RetryAfter)
	}
}

// A request that waited on the account's lock reckons from when it got it. The
// neighbour holds the row, moves the next free moment a second ahead and lets
// go after a second and a half: by then the moment has passed, and a request
// that read the clock when its transaction began would think it had not.
func TestTheRateIsReckonedFromAfterTheWait(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)
	account := "AC-" + uuid.New().String()
	if _, err := s.db.Exec(`INSERT INTO call_capacity (account_scope) VALUES ($1)`, account); err != nil {
		t.Fatal(err)
	}

	neighbour, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := neighbour.Exec(`SELECT 1 FROM call_capacity WHERE account_scope = $1 FOR UPDATE`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := neighbour.Exec(`UPDATE call_capacity SET next_free_at = clock_timestamp() + interval '1 second'
		WHERE account_scope = $1`, account); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		req := callTo("alice", "verify", phoneA, "+15005550006")
		req.AccountScope, req.CPS = account, 1
		_, err := s.ReservePhoneCall(ctx, req)
		done <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	if err := neighbour.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("a request whose wait outlasted the rate was refused: %v", err)
	}
}

// A mark is for the number it was made on: a new number asks again.
func TestADNDCheckIsForTheNumberItWasMadeFor(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	const sender = "+15005550006"
	mustSetPhone(t, s, "alice", phoneA)

	if err := s.ConfirmDNDCheck(ctx, "alice", sender); !errors.Is(err, ErrPhoneNotVerified) {
		t.Fatalf("mark on an unverified number = %v, want ErrPhoneNotVerified", err)
	}
	mustVerifyPhone(t, s, "alice", phoneA)
	if err := s.ConfirmDNDCheck(ctx, "alice", sender); err != nil {
		t.Fatalf("mark on a verified number: %v", err)
	}
	checks, err := s.PhoneDNDChecks(ctx, "alice")
	if err != nil || len(checks) != 1 {
		t.Fatalf("checks for the number = %v, %v", checks, err)
	}

	mustSetPhone(t, s, "alice", phoneB)
	if checks, _ := s.PhoneDNDChecks(ctx, "alice"); len(checks) != 0 {
		t.Fatalf("a new number shows the old number's checks: %v", checks)
	}
}

// A test call needs a verified number, and a request about a number the person
// no longer has is refused before anything is spent.
func TestACallIsOnlyForThePersonsCurrentNumber(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice")
	mustSetPhone(t, s, "alice", phoneA)

	req := callTo("alice", "dnd_check", phoneA, "+15005550006")
	req.RequireVerified = true
	if _, err := s.ReservePhoneCall(ctx, req); !errors.Is(err, ErrPhoneNotVerified) {
		t.Fatalf("test call to an unverified number = %v, want ErrPhoneNotVerified", err)
	}
	if _, err := s.ReservePhoneCall(ctx, callTo("alice", "verify", phoneB, "+15005550006")); !errors.Is(err, ErrPhoneContactChanged) {
		t.Fatalf("call to a number the person does not have = %v, want ErrPhoneContactChanged", err)
	}
}

// Erasure leaves no number behind: not the contact, not the marks, not the log.
func TestErasureRemovesThePhone(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedUsers(t, s, "alice", "bob")
	for _, id := range []string{"alice", "bob"} {
		mustSetPhone(t, s, id, phoneA)
		mustVerifyPhone(t, s, id, phoneA)
		req := callTo(id, "dnd_check", phoneA, "+15005550006")
		if _, err := s.ReservePhoneCall(ctx, req); err != nil {
			t.Fatalf("test call %s: %v", id, err)
		}
		if err := s.ConfirmDNDCheck(ctx, id, "+15005550006"); err != nil {
			t.Fatalf("mark %s: %v", id, err)
		}
	}

	if err := s.ErasureRepository().WithinTx(ctx, func(tx erasure.Tx) error {
		return tx.DeleteUserPhoneData(ctx, "alice")
	}); err != nil {
		t.Fatalf("erase alice's phone: %v", err)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, check := range []struct {
		what, query string
		alice, bob  int
	}{
		{"contacts", `SELECT count(*) FROM user_contacts WHERE user_id = $1`, 0, 1},
		{"dnd marks", `SELECT count(*) FROM phone_dnd_checks WHERE user_id = $1`, 0, 1},
		{"logged numbers", `SELECT count(*) FROM phone_calls_log WHERE user_id = $1 AND to_number IS NOT NULL`, 0, 1},
		{"reservations naming the person", `SELECT count(*) FROM call_reservations WHERE user_id = $1`, 0, 1},
	} {
		if got := count(check.query, "alice"); got != check.alice {
			t.Errorf("alice's %s after erasure = %d, want %d", check.what, got, check.alice)
		}
		if got := count(check.query, "bob"); got != check.bob {
			t.Errorf("bob's %s after erasing alice = %d, want %d", check.what, got, check.bob)
		}
	}
}
