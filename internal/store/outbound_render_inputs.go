package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// What a revision freezes besides the group's row: the tail of its history and
// which providers have buttons on. Both are read from the database at the
// moment of the freeze - by the producer of revision 0 before it admits, and by
// every raise inside the transaction that made the alert move - and from
// nowhere else. A process cache would let two instances freeze two different
// answers for one revision.

// timelineTailTx reads the most recent keys.TimelineLength lines of a group's
// history, oldest first, and how many earlier lines there are.
func timelineTailTx(ctx context.Context, q sqlQueryer, alertGroupID string) ([]*model.TimelineEvent, int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, type, message, actor, created_at, count(*) OVER ()
		FROM timeline_events
		WHERE alert_group_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, alertGroupID, keys.TimelineLength)
	if err != nil {
		return nil, 0, fmt.Errorf("read the history of %s: %w", alertGroupID, err)
	}
	defer rows.Close()

	var (
		newestFirst []*model.TimelineEvent
		total       int64
	)
	for rows.Next() {
		var (
			e     model.TimelineEvent
			actor sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Type, &e.Message, &actor, &e.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		e.AlertGroupID = alertGroupID
		e.Actor = actor.String
		newestFirst = append(newestFirst, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	lines := make([]*model.TimelineEvent, len(newestFirst))
	for i, e := range newestFirst {
		lines[len(newestFirst)-1-i] = e
	}
	return lines, total - int64(len(lines)), nil
}

// interactiveProvidersTx says which providers put buttons on their cards, as
// the integrations table stands now: an enabled Slack integration with the
// switch on, an enabled Telegram integration with the switch on and somewhere
// to send people back to.
//
// A configuration that cannot be read stops the freeze and names the row. The
// answer decides the bytes of a message, so it is execution data here, and a
// row skipped as "off" would take the buttons away under a digest saying that
// is what was asked for.
func interactiveProvidersTx(ctx context.Context, q sqlQueryer, selfURL string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, type, config FROM integrations
		WHERE enabled AND type IN ($1, $2)`,
		model.IntegrationTypeSlack, model.IntegrationTypeTelegram)
	if err != nil {
		return nil, fmt.Errorf("read the button switches: %w", err)
	}
	defer rows.Close()

	on := map[string]bool{}
	for rows.Next() {
		var id, kind, encrypted string
		if err := rows.Scan(&id, &kind, &encrypted); err != nil {
			return nil, err
		}
		raw, err := decryptConfig(encrypted)
		if err != nil {
			return nil, fmt.Errorf("read the configuration of integration %s: %w", id, err)
		}
		switch model.IntegrationType(kind) {
		case model.IntegrationTypeSlack:
			var cfg model.SlackConfig
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("read the configuration of integration %s: %w", id, err)
			}
			if cfg.Interactive {
				on[keys.InteractiveSlack] = true
			}
		case model.IntegrationTypeTelegram:
			var cfg model.TelegramConfig
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("read the configuration of integration %s: %w", id, err)
			}
			// Telegram's buttons need somewhere to send people back to, and
			// that link comes from this instance's own URL.
			if cfg.IsInteractive() && selfURL != "" {
				on[keys.InteractiveTelegram] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	providers := make([]string, 0, len(on))
	for p := range on {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	return providers, nil
}

// RenderInputs is the producer's read of the same two things, before it admits.
//
// Outside the admission's transaction, and deliberately so: the plan is built
// before SubmitBatch, and the window between this read and the commit is the
// named best-effort risk of the button switch - closed by the switch's own
// door and by the reconciliation every start performs.
func (s *Store) RenderInputs(ctx context.Context, alertGroupID string) (providers.RenderInputs, error) {
	history, omitted, err := timelineTailTx(ctx, s.db, alertGroupID)
	if err != nil {
		return providers.RenderInputs{}, err
	}
	buttons, err := interactiveProvidersTx(ctx, s.db, s.render.selfURL)
	if err != nil {
		return providers.RenderInputs{}, err
	}
	return providers.RenderInputs{
		Timeline: history, TimelineOmitted: omitted, Interactive: buttons,
	}, nil
}
