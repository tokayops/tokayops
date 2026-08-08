package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
)

// The lifecycle contract, as one table.
//
// It is run twice - against PostgreSQL and against the mock - because the two
// disagreeing is how a hole gets shipped: production forbids the write, the
// mock allows it, and every API test keeps passing against a state that can no
// longer exist.
var userOwnedWrites = []struct {
	name string
	// write creates something owned by userID.
	write func(s StoreInterface, userID string) error
	// wantErr is what the write must answer once the user is erased. Some
	// paths report "nothing changed" instead, and say so here.
	tolerateSilence bool
}{
	{
		name: "api token",
		write: func(s StoreInterface, userID string) error {
			return s.CreateAPIToken(&model.APIToken{
				ID: "tok-" + userID, UserID: userID, Name: "ci", TokenHash: "hash-" + userID,
			})
		},
	},
	{
		name: "external identity",
		write: func(s StoreInterface, userID string) error {
			return s.BindExternalIdentity(&model.ExternalIdentity{
				UserID: userID, Provider: "slack", ExternalID: "U-" + userID,
			})
		},
	},
	{
		// This one is documented to answer "nothing changed" rather than to
		// fail, and an erased user is exactly that: nothing changed.
		name: "external identity if absent",
		write: func(s StoreInterface, userID string) error {
			changed, err := s.BindExternalIdentityIfAbsent(userID, "telegram", "T-"+userID, "")
			if err != nil {
				return err
			}
			if changed {
				return errors.New("an identity was bound to an erased user")
			}
			return nil
		},
		tolerateSilence: true,
	},
	{
		name: "link token",
		write: func(s StoreInterface, userID string) error {
			return s.IssueLinkToken(userID, "telegram", "", "123456", time.Now().Add(time.Hour))
		},
	},
	{
		name: "team membership",
		write: func(s StoreInterface, userID string) error {
			return s.AddTeamMember("devops", userID, model.TeamMemberRoleMember)
		},
	},
}

// The role lifecycle, as its own table.
//
// Role is not a user-owned object, so the table above never touched it - which
// is exactly why the mock kept letting an erased user be promoted and kept
// counting them as one of the administrators the system must not run out of.
var userRoleLifecycle = []struct {
	name string
	// check runs against a store where "gone" has been erased and "root" is
	// the only living administrator.
	check func(t *testing.T, s StoreInterface)
}{
	{
		name: "an erased user has no role to change",
		check: func(t *testing.T, s StoreInterface) {
			if err := s.SetUserRole("gone", model.UserRoleAdmin); !errors.Is(err, ErrUserNotFound) {
				t.Fatalf("SetUserRole on an erased user = %v, want ErrUserNotFound", err)
			}
		},
	},
	{
		name: "an erased admin is not one of the administrators",
		check: func(t *testing.T, s StoreInterface) {
			count, err := s.CountAdmins()
			if err != nil {
				t.Fatalf("CountAdmins: %v", err)
			}
			if count != 1 {
				t.Fatalf("CountAdmins = %d, want only the living administrator", count)
			}
		},
	},
	{
		// The one that matters: with the erased admin counted, the guard sees
		// two and lets the last living administrator go.
		name: "the last living admin cannot be demoted",
		check: func(t *testing.T, s StoreInterface) {
			if err := s.SetUserRole("root", model.UserRoleUser); !errors.Is(err, ErrLastAdmin) {
				t.Fatalf("demoting the last admin = %v, want ErrLastAdmin", err)
			}
			user, err := s.GetActiveUserByID("root")
			if err != nil {
				t.Fatalf("GetActiveUserByID: %v", err)
			}
			if user.Role != model.UserRoleAdmin {
				t.Fatalf("role = %q, want the demotion refused", user.Role)
			}
		},
	},
}

func TestErasedAdminIsNotAnAdmin(t *testing.T) {
	for _, tc := range userRoleLifecycle {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			seedTeam(t, s, "devops")
			seedTwoAdminsOneErased(t, s, s.ErasureRepository())
			tc.check(t, s)
		})
	}
}

func TestMockStoreMirrorsTheRoleLifecycle(t *testing.T) {
	for _, tc := range userRoleLifecycle {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockStore()
			defer m.Close()
			for _, id := range []string{"root", "gone"} {
				if err := m.CreateUser(&model.User{ID: id, Name: id,
					Email: id + "@example.test", Role: model.UserRoleAdmin}); err != nil {
					t.Fatalf("CreateUser %s: %v", id, err)
				}
			}
			// The mock seeds denis as an admin; this table is about root and
			// gone, so denis steps aside.
			if err := m.SetUserRole("denis", model.UserRoleUser); err != nil {
				t.Fatalf("SetUserRole denis: %v", err)
			}
			m.EraseUser("gone")
			tc.check(t, m)
		})
	}
}

// seedTwoAdminsOneErased leaves root as the only living administrator and gone
// as an erased one, which is the state the demotion guard used to get wrong.
func seedTwoAdminsOneErased(t *testing.T, s *Store, repo erasure.Repository) {
	t.Helper()
	for _, id := range []string{"root", "gone"} {
		if err := s.CreateUser(&model.User{ID: id, Name: id,
			Email: id + "@example.test", Role: model.UserRoleAdmin}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.SetUserRole(id, model.UserRoleAdmin); err != nil {
			t.Fatalf("SetUserRole %s: %v", id, err)
		}
	}
	if err := erasure.NewService(repo).Erase(context.Background(), "gone"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
}

// A link token that silently issues for nobody is worse than one that fails:
// the caller goes on to send an OTP or a deep link that can never work. Every
// creating path therefore says so, and this is the deterministic check - the
// concurrent one only proves no row survived, which a silent no-op also
// satisfies.
func TestErasedUserCannotOwnAnythingNew(t *testing.T) {
	for _, w := range userOwnedWrites {
		t.Run(w.name, func(t *testing.T) {
			s := setupTestDB(t)
			seedTeam(t, s, "devops", "alice")
			if err := s.CreateUser(&model.User{ID: "carol", Name: "carol",
				Email: "carol@example.test"}); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			// alice is the bootstrap admin, so carol is not the last one.
			if err := s.SetUserRole("alice", model.UserRoleAdmin); err != nil {
				t.Fatalf("SetUserRole: %v", err)
			}
			if err := erasure.NewService(s.ErasureRepository()).
				Erase(context.Background(), "carol"); err != nil {
				t.Fatalf("Erase: %v", err)
			}

			if got := w.write(s, "carol"); !errors.Is(got, ErrUserNotFound) {
				if w.tolerateSilence && got == nil {
					return
				}
				t.Fatalf("error = %v, want ErrUserNotFound", got)
			}
		})
	}
}

// The same table against the mock. If this passes and the one above fails, or
// the other way round, the doubles have drifted and the API tests are proving
// something about a system that does not exist.
func TestMockStoreMirrorsTheUserLifecycleContract(t *testing.T) {
	for _, w := range userOwnedWrites {
		t.Run(w.name, func(t *testing.T) {
			m := NewMockStore()
			defer m.Close()
			if err := m.CreateUser(&model.User{ID: "carol", Name: "carol",
				Email: "carol@example.test"}); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			m.EraseUser("carol")

			if got := w.write(m, "carol"); !errors.Is(got, ErrUserNotFound) {
				if w.tolerateSilence && got == nil {
					return
				}
				t.Fatalf("error = %v, want ErrUserNotFound", got)
			}
		})
	}
}

// Reads have the same rule from the other side: a credential or an inbound
// identity must not resolve to somebody who has been erased.
func TestMockStoreDoesNotResolveErasedUsers(t *testing.T) {
	m := NewMockStore()
	defer m.Close()
	if err := m.CreateUser(&model.User{ID: "carol", Name: "carol",
		Email: "carol@example.test"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := m.CreateAPIToken(&model.APIToken{
		ID: "tok-1", UserID: "carol", Name: "ci", TokenHash: "hash-1",
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if err := m.BindExternalIdentity(&model.ExternalIdentity{
		UserID: "carol", Provider: "slack", ExternalID: "U-carol",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	if _, err := m.GetAPITokenByHash("hash-1"); err != nil {
		t.Fatalf("the token should resolve before the erasure: %v", err)
	}
	m.EraseUser("carol")

	if _, err := m.GetAPITokenByHash("hash-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a token of an erased user resolved: %v", err)
	}
	if _, err := m.GetUserByExternalID("slack", "U-carol"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an erased user was found by external id: %v", err)
	}
}
