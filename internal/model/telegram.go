package model

// Telegram inline-button callback_data prefixes. The provider renders
// "<prefix><alert_group_id>" into the button; the webhook handler parses it back.
// Shared between dispatcher (button rendering) and api (handler) — mirror of
// model/slack.go's SlackAction* constants.
const (
	TelegramCallbackAckPrefix     = "ack:"
	TelegramCallbackResolvePrefix = "res:"
)
