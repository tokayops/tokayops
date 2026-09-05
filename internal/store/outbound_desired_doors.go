package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Two more doors through which what a group's messages show can move: a note
// somebody wrote, which the thread under the card renders, and the button
// switch of a provider, which the card renders. Both go through the same raise
// as the acknowledgement, the resolution and the merge, under the same lock,
// and each form of message is raised only when what it shows has changed.

// NoteLimit is the longest note a person can write, in runes. The thread
// renders notes, and a thread is a message with a size.
const NoteLimit = 2000

// ErrNoteInvalid is a note that is empty or longer than NoteLimit.
var ErrNoteInvalid = errors.New("a note is between 1 and 2000 characters")

// liveEditableStatuses are the states a card can be raised in.
const liveEditableStatuses = `'pending', 'sending', 'idle', 'manual_review'`

// AddAlertGroupNoteAtomic writes a line of history and raises what the thread
// shows, in one transaction under the group's lock.
//
// The note used to be written beside the domain, with no lock and no raise:
// nothing rendered it. The thread does, so a note is a door like an
// acknowledgement, and the history line and the raise commit together - a
// note without a raise is a thread that stays a line behind until the next
// door, and a raise without the note is a revision about nothing.
//
// The actor is the authenticated person, never a name the request supplied:
// the thread prints the actor beside the line.
func (s *Store) AddAlertGroupNoteAtomic(ctx context.Context, alertGroupID, text string,
	who alertgroup.Actor) (*model.TimelineEvent, error) {

	if n := utf8.RuneCountInString(text); n == 0 || n > NoteLimit {
		return nil, ErrNoteInvalid
	}
	by, err := outbound.UserActor(who.ID)
	if err != nil {
		return nil, outboundContractf("note on %s: %v", alertGroupID, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// The group's row first, and taken by name: lockAlertGroupTx forgives a
	// group that is not there, and a note on one is a 404, not a no-op.
	var locked string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM alert_groups WHERE id = $1 FOR UPDATE`, alertGroupID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("lock alert group %s: %w", alertGroupID, err)
	}

	event := &model.TimelineEvent{
		ID: uuid.New().String(), AlertGroupID: alertGroupID,
		Type: model.TimelineEventNote, Message: text, Actor: who.Name,
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, '{}', now())
		RETURNING created_at`,
		event.ID, event.AlertGroupID, event.Type, event.Message, event.Actor,
	).Scan(&event.CreatedAt); err != nil {
		return nil, fmt.Errorf("write the note on %s: %w", alertGroupID, err)
	}

	desired, err := setDesiredStateTx(ctx, tx, s.render, outbound.DesiredStateRequest{
		AlertGroupID: alertGroupID, Reason: outbound.DesiredNote, Actor: by,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	countDesired(outbound.DesiredNote, desired.Outcome)
	return event, nil
}

// RaiseInteractivity brings every live card of a provider up to date with its
// button switch, one transaction per group.
//
// It runs in the request that moved the switch, after the switch's own commit.
// One transaction per group rather than one for all of them: a hundred open
// incidents are a hundred short transactions, and an instance that dies
// halfway leaves half the cards raised and half not - which the reconciliation
// at the next start finishes. Nothing here is a guarantee; the guarantee is
// that a PRESS of a button reads the switch from the database.
func (s *Store) RaiseInteractivity(ctx context.Context, provider string, by outbound.Actor) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT alert_group_id FROM outbound_intents
		WHERE provider = $1 AND form = $2 AND alert_group_id IS NOT NULL
		  AND status IN (`+liveEditableStatuses+`)`,
		provider, string(outbound.FormEditable))
	if err != nil {
		return 0, fmt.Errorf("find the live cards of %s: %w", provider, err)
	}
	var groups []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		groups = append(groups, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for i, group := range groups {
		if err := s.raiseForInteractivity(ctx, group, by); err != nil {
			return i, err
		}
	}
	return len(groups), nil
}

// ReconcileInteractivity is what every start does: for each group with a live
// card, compare the buttons its snapshot was drawn with against the switches
// as they stand, and raise the ones that differ.
//
// It is the other half of RaiseInteractivity. That door is best-effort - an
// instance can die halfway, and an admission planned before the switch moved
// can commit after the door's scan - and this is what closes both: the
// snapshot says what was drawn, the switch says what should be, and a
// difference is a raise.
func (s *Store) ReconcileInteractivity(ctx context.Context) (int, error) {
	current, err := interactiveProvidersTx(ctx, s.db, s.render.selfURL)
	if err != nil {
		return 0, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT g.alert_group_id, g.snapshot->'interactive_providers'
		FROM outbound_group_snapshots g
		WHERE NOT g.final AND g.snapshot_schema_version = $1
		  AND EXISTS (SELECT 1 FROM outbound_intents i
		              WHERE i.alert_group_id = g.alert_group_id AND i.form = $2
		                AND i.status IN (`+liveEditableStatuses+`))`,
		keys.RenderSnapshotSchemaV2, string(outbound.FormEditable))
	if err != nil {
		return 0, fmt.Errorf("read the buttons the live cards were drawn with: %w", err)
	}
	var behind []string
	for rows.Next() {
		var (
			group string
			raw   []byte
		)
		if err := rows.Scan(&group, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		var drawn []string
		if err := json.Unmarshal(raw, &drawn); err != nil {
			rows.Close()
			return 0, outboundContractf(
				"the state of %s does not say which buttons its cards were drawn with: %v", group, err)
		}
		if !sameProviders(drawn, current) {
			behind = append(behind, group)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for i, group := range behind {
		if err := s.raiseForInteractivity(ctx, group, outbound.ActorSystem); err != nil {
			return i, err
		}
	}
	return len(behind), nil
}

// raiseForInteractivity is one group's raise: the lock, the raise, the commit.
func (s *Store) raiseForInteractivity(ctx context.Context, alertGroupID string, by outbound.Actor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAlertGroupTx(ctx, tx, alertGroupID); err != nil {
		return err
	}
	desired, err := setDesiredStateTx(ctx, tx, s.render, outbound.DesiredStateRequest{
		AlertGroupID: alertGroupID, Reason: outbound.DesiredInteractivity, Actor: by,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	countDesired(outbound.DesiredInteractivity, desired.Outcome)
	return nil
}

// sameProviders compares two canonical lists: both sorted, no repeats.
func sameProviders(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buttonsMoved says whether a change to an integration may have moved its
// button switch, and for which provider. A configuration that cannot be read
// counts as moved: the raise recomputes from the database and answers
// "unchanged" cheaply when nothing did.
func buttonsMoved(before, after *model.Integration) (string, bool) {
	if before.Enabled != after.Enabled {
		switch before.Type {
		case model.IntegrationTypeSlack:
			return keys.InteractiveSlack, true
		case model.IntegrationTypeTelegram:
			return keys.InteractiveTelegram, true
		}
		return "", false
	}
	switch before.Type {
	case model.IntegrationTypeSlack:
		var was, now model.SlackConfig
		if json.Unmarshal(before.Config, &was) != nil || json.Unmarshal(after.Config, &now) != nil {
			return keys.InteractiveSlack, true
		}
		return keys.InteractiveSlack, was.Interactive != now.Interactive
	case model.IntegrationTypeTelegram:
		var was, now model.TelegramConfig
		if json.Unmarshal(before.Config, &was) != nil || json.Unmarshal(after.Config, &now) != nil {
			return keys.InteractiveTelegram, true
		}
		return keys.InteractiveTelegram, was.IsInteractive() != now.IsInteractive()
	}
	return "", false
}

// raiseAfterSwitch is UpdateIntegration's call, after its commit. The outcome
// is logged rather than returned: the switch is committed either way, and an
// error here is a gap the next start closes.
func (s *Store) raiseAfterSwitch(ctx context.Context, before, after *model.Integration, actor string) {
	provider, moved := buttonsMoved(before, after)
	if !moved {
		return
	}
	by := outbound.ActorSystem
	if actor != "" {
		if person, err := outbound.UserActor(actor); err == nil {
			by = person
		}
	}
	raised, err := s.RaiseInteractivity(ctx, provider, by)
	if err != nil {
		log.Printf("outbound: the button switch of %s moved; %d alert group(s) brought up to date "+
			"before an error, the next start finishes: %v", provider, raised, err)
		return
	}
	log.Printf("outbound: the button switch of %s moved: %d alert group(s) brought up to date",
		provider, raised)
}
