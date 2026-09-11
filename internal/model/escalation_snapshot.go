package model

// What an alert group records about the escalation it was put through.
//
// It is written once, when the escalation is admitted, and read afterwards by
// anybody asking what this alert did - including after the policy it was built
// from has been edited or deleted. That is the whole reason it is a snapshot
// rather than a policy id: a page that went out under yesterday's plan is not
// explained by today's.
//
// These lived among the job types, because a job was once what carried them.
// Nothing carries a job any more; the snapshot outlived it because it answers a
// question the job never did.

// EscalationPolicySnapshot is the plan an alert group was escalated by, as it
// stood at admission.
type EscalationPolicySnapshot struct {
	PolicyID string                    `json:"policy_id,omitempty"` // Effective policy ID (empty if firehose-only fallback)
	Name     string                    `json:"name"`
	Steps    []*EscalationStepSnapshot `json:"steps"`
}

// EscalationStepSnapshot is one step of that plan. The pair (Provider,
// TargetKind) mirrors EscalationStep.
type EscalationStepSnapshot struct {
	Provider   string `json:"provider"`
	TargetKind string `json:"target_kind"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	IsFirehose bool   `json:"is_firehose,omitempty"` // true for firehose step
	StageIndex int    `json:"stage_index"`           // which stage this step belongs to
}
