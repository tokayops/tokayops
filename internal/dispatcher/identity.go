package dispatcher

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// ErrIdentityNotLinked means the user has no linked external identity for the
// requested provider. It is a permanent dispatcher identity-resolution error
// (see worker.go isPermanentError) - retrying the step cannot help; the user
// must link their account.
//
// This sentinel lives in the dispatcher package, not the Slack provider,
// because the failure is about provider-agnostic identity resolution.
var ErrIdentityNotLinked = errors.New("user has no linked external identity for provider")

// resolveRecipient maps an TokayOps user to a provider-specific recipient ID via
// external_identities. It is fully generic - no per-provider switch - so adding
// Telegram did not require touching this function. The old
// `default -> ErrNoIdentityResolver` branch is gone
// because the (user_id, provider) lookup is the same shape for every
// provider; missing identity is the only failure mode.
//
// Returns the external recipient ID and a human-readable display name (for
// timeline detail). Errors are permanent (config / linking faults) - see
// isPermanentError.
func resolveRecipient(s store.StoreInterface, providerName, userID string) (extID, displayName string, err error) {
	ident, err := s.GetExternalIdentity(userID, providerName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("user %s has no %s identity: %w", userID, providerName, ErrIdentityNotLinked)
		}
		return "", "", fmt.Errorf("failed to load %s identity for user %s: %w", providerName, userID, err)
	}
	if ident == nil || ident.ExternalID == "" {
		return "", "", fmt.Errorf("user %s has no %s identity: %w", userID, providerName, ErrIdentityNotLinked)
	}
	name := ident.DisplayName
	if name == "" {
		if u, e := s.GetUserByID(userID); e == nil && u != nil {
			name = u.Name
		}
	}
	return identitySendTarget(ident), name, nil
}

// identitySendTarget is the single seam for picking which external_identities
// column is the actual send target. Today it is always ExternalID - for Slack
// `chat.postMessage` and for Telegram private-chat DMs (where chat_id ==
// user_id by convention). If a future provider needs the chat_id column
// instead, either branch here on provider name or move the choice onto
// Provider as a SendTargetFor(ei) method.
func identitySendTarget(ei *model.ExternalIdentity) string {
	return ei.ExternalID
}
