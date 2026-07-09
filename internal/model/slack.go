package model

// Slack block_actions action IDs for interactive buttons.
// Shared between dispatcher (button rendering) and api (handler).
const (
	SlackActionAckAlertGroup     = "ack_alert_group"
	SlackActionResolveAlertGroup = "resolve_alert_group"
)
