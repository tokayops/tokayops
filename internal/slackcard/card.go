// Package slackcard defines the transport struct a Slack alert card is built
// into. It is intentionally a leaf package - it imports only the slack-go SDK,
// and the channel that renders into it imports nothing of the domain.
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
