package alertgroup

import (
	"database/sql"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
)

type TransitionOutcome string

const (
	OutcomeApplied     TransitionOutcome = "applied"
	OutcomeAlreadyDone TransitionOutcome = "already_done"
	OutcomeNotFound    TransitionOutcome = "not_found"
)

type Actor struct {
	// ID is the user, for the journal of the commitments the transition
	// withdraws. Name and Email are the audit labels the alert's own timeline
	// keeps.
	ID    string
	Name  string
	Email string
}

type TransitionResult struct {
	Outcome    TransitionOutcome
	AlertGroup *model.AlertGroup // nil only on not_found
}

type Service struct {
	store transitions
}

// transitions is the store as this service needs it: read the group, and apply
// one of the two single-winner transitions.
//
// The atomic calls are what this service exists for. Each of them carries the
// status change, the timeline entry, the outbox event and the cancellation of
// the escalation in one commit, so "acknowledged" and "nobody is being paged
// any more" are one fact rather than two writes that can be interrupted.
type transitions interface {
	GetAlertGroupByID(id string) (*model.AlertGroup, error)
	AckAlertGroupAtomic(id string, actor Actor, meta map[string]string, outboxEvent *model.OutboxEvent) (changed bool, err error)
	ResolveAlertGroupAtomic(id string, actor Actor, meta map[string]string, outboxEvent *model.OutboxEvent) (changed bool, err error)
}

func NewService(s transitions) *Service {
	return &Service{store: s}
}

func (s *Service) Ack(alertGroupID string, actor Actor, meta map[string]string) (*TransitionResult, error) {
	ag, err := s.store.GetAlertGroupByID(alertGroupID)
	if err != nil {
		if err == sql.ErrNoRows {
			return &TransitionResult{Outcome: OutcomeNotFound}, nil
		}
		return nil, err
	}

	if ag.Status != model.AlertGroupStatusProcessing && ag.Status != model.AlertGroupStatusTriggered {
		return &TransitionResult{Outcome: OutcomeAlreadyDone, AlertGroup: ag}, nil
	}

	now := time.Now()
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventAcknowledged, ag, ag.TeamNameSnapshot, actor.Name, actor.Email, now,
	)
	if err != nil {
		return nil, err
	}
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventAcknowledged,
		AlertGroupID: alertGroupID,
		TeamID:       ag.TeamID,
		Actor:        actor.Name,
		Payload:      eventPayload,
	}

	changed, err := s.store.AckAlertGroupAtomic(alertGroupID, actor, meta, outboxEvent)
	if err != nil {
		return nil, err
	}
	if !changed {
		current, err := s.store.GetAlertGroupByID(alertGroupID)
		if err != nil {
			return nil, err
		}
		return &TransitionResult{Outcome: OutcomeAlreadyDone, AlertGroup: current}, nil
	}

	metrics.ObserveAck(ag)

	ag.Status = model.AlertGroupStatusAcknowledged
	ag.AcknowledgedBy = actor.Name
	return &TransitionResult{Outcome: OutcomeApplied, AlertGroup: ag}, nil
}

func (s *Service) Resolve(alertGroupID string, actor Actor, meta map[string]string) (*TransitionResult, error) {
	ag, err := s.store.GetAlertGroupByID(alertGroupID)
	if err != nil {
		if err == sql.ErrNoRows {
			return &TransitionResult{Outcome: OutcomeNotFound}, nil
		}
		return nil, err
	}

	if ag.Status != model.AlertGroupStatusProcessing &&
		ag.Status != model.AlertGroupStatusTriggered &&
		ag.Status != model.AlertGroupStatusAcknowledged {
		return &TransitionResult{Outcome: OutcomeAlreadyDone, AlertGroup: ag}, nil
	}

	now := time.Now()
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventResolved, ag, ag.TeamNameSnapshot, actor.Name, actor.Email, now,
	)
	if err != nil {
		return nil, err
	}
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventResolved,
		AlertGroupID: alertGroupID,
		TeamID:       ag.TeamID,
		Actor:        actor.Name,
		Payload:      eventPayload,
	}

	changed, err := s.store.ResolveAlertGroupAtomic(alertGroupID, actor, meta, outboxEvent)
	if err != nil {
		return nil, err
	}
	if !changed {
		current, err := s.store.GetAlertGroupByID(alertGroupID)
		if err != nil {
			return nil, err
		}
		return &TransitionResult{Outcome: OutcomeAlreadyDone, AlertGroup: current}, nil
	}

	ag.ResolvedAt = &now
	metrics.ObserveResolution(ag)

	ag.Status = model.AlertGroupStatusResolved
	ag.ResolvedBy = actor.Name
	return &TransitionResult{Outcome: OutcomeApplied, AlertGroup: ag}, nil
}
