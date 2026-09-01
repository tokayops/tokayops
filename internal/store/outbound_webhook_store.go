package store

import (
	"context"
	"database/sql"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// admitWebhookTx admits one webhook batch - a fan-out of an event, or a replay
// to one subscriber - inside the caller's transaction, and it is the ONLY door
// the webhook family has.
//
// It takes the provenance and derives the admission itself. Handed a ready
// Admission, it would have to trust that the keys, the payloads and the
// fingerprint in it were still the ones the grammar produced together, and
// nothing in the type system says so: the fields are exported, and a caller
// that changed the recipient in the payload and in the columns alike would pass
// every pairwise check with a key still naming the old one. Deriving here,
// there is no moment at which the parts exist apart.
//
// The caller owns the transaction, so the caller counts the admission - after
// its commit and not before. Counting here, inside a transaction that may yet
// roll back, would report work that never came to exist.
//
// Unlike an escalation's or a handover's door this one locks no recipients: a
// subscriber is an integration, and whether it still exists is the fan-out's
// business, decided under the share lock it takes on the audience.
func admitWebhookTx(ctx context.Context, tx *sql.Tx, batch keys.WebhookBatch,
	actor string) (outbound.SubmitResult, error) {

	admission, err := batch.Admit()
	if err != nil {
		return outbound.SubmitResult{}, outboundContractf("%v", err)
	}
	family, err := keys.FamilyOf(admission.Kind)
	if err != nil {
		return outbound.SubmitResult{}, outboundContractf("%v", err)
	}

	// The claim first, for the same reason every other door reads it first:
	// a repeat of an admission has to find the row the first one wrote.
	if held, found, err := existingAdmission(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if found {
		return held, nil
	}

	batchID, admittedAt, won, err := claimBatchTx(ctx, tx, admission, string(family),
		"", nil, nil, 0, 0)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	if !won {
		return lostAdmission(ctx, tx, admission)
	}

	intentIDs, err := insertCommitmentsTx(ctx, tx, batchID, admission, string(family),
		admittedAt, actor)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	return outbound.SubmitResult{
		Outcome:   outbound.SubmitCreated,
		BatchID:   batchID,
		IntentIDs: intentIDs,
	}, nil
}
