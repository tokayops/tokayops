package model

// EscalationStepData contains specific data for escalation steps
type EscalationStepData struct {
	TargetID     string `json:"target_id"`
	TargetType   string `json:"target_type"`
	ProviderName string `json:"provider_name"` // which provider sends this step (set by builder)
	Message      string `json:"message"`
	AlertGroupID string `json:"alert_group_id"`
	PolicyID     string `json:"policy_id"`
	DelaySeconds int    `json:"delay_seconds"`
	IsFirehose   bool   `json:"is_firehose"`
}

// OTPStepData has been removed. The live OTP flow (POST
// /me/slack/request-code -> DM sent synchronously in the handler) never went
// through a job step; the job-based path was an experiment that the link-token
// service replaced. Restoring an async OTP path is a new feature, not a
// regression to fix here.

// HandoffStepData contains specific data for handoff notification steps.
//
// The executor reads ProviderName rather than hardcoding "slack", and TargetID
// is the provider-specific external ID, resolved by handoff_notifier via
// identitySendTarget.
type HandoffStepData struct {
	ProviderName string `json:"provider_name"` // which provider sends this DM
	TargetID     string `json:"target_id"`     // provider-specific external ID
	Message      string `json:"message"`       // Formatted DM text
	ScheduleID   string `json:"schedule_id"`   // For audit
	TeamID       string `json:"team_id"`       // For audit
}
