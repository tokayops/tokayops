// Package slackcard defines the transport struct used to render or replace
// a Slack alert card. It is intentionally a leaf package — it imports only
// the slack-go SDK and is imported by both internal/api (which owns the
// SlackCardRenderer interface) and internal/dispatcher (which implements it).
package slackcard

import "github.com/slack-go/slack"

// Card bundles everything needed to render or replace a Slack alert card.
//   - Text: top-level "text" field — notification preview / accessibility fallback.
//   - Blocks: top-level blocks (title) — what Slack message-link unfurls render.
//   - Attachment: colored attachment with body + buttons + footer (the "sidebar" UI).
type Card struct {
	Text       string
	Blocks     []slack.Block
	Attachment slack.Attachment
}
