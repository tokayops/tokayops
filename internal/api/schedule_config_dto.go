package api

import (
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// The editor DTOs carry USER INTENT only.
//
// Generated fields - the phase anchor, the start position, and the L2 group
// identities derived from the user list - are deliberately absent from both
// directions. Accepting them would let a client pin a rotation's phase to
// whatever it last saw, which is exactly the class of bug the revision model
// exists to remove; returning them would invite a client to send them back.

// ScheduleGroupDTO is one L1 rotation group. The ID is owned by the editor: it
// is the identity of a row on screen, and it is what lets the planner tell
// "this group gained a member" from "the groups were replaced".
type ScheduleGroupDTO struct {
	ID      string   `json:"id"`
	UserIDs []string `json:"user_ids"`
}

// ScheduleL1DTO is the primary layer.
type ScheduleL1DTO struct {
	Enabled      bool               `json:"enabled"`
	RotationType string             `json:"rotation_type"` // daily | weekly
	HandoffTime  string             `json:"handoff_time"`  // local "HH:MM"
	HandoffDay   *int               `json:"handoff_day"`   // weekly only, 0=Sunday
	Groups       []ScheduleGroupDTO `json:"groups"`
}

// ScheduleL2DTO is the escalation layer. It carries an ordered user list rather
// than groups: an L2 position is always one person, and the server derives the
// singleton groups so both layers share one rotation math.
type ScheduleL2DTO struct {
	Enabled                  bool     `json:"enabled"`
	EscalationTimeoutMinutes int      `json:"escalation_timeout_minutes"`
	RotationType             string   `json:"rotation_type"`
	HandoffTime              string   `json:"handoff_time"`
	HandoffDay               *int     `json:"handoff_day"`
	UserIDs                  []string `json:"user_ids"`
}

// ScheduleConfigDTO is the configuration of a schedule as the editor sees it.
type ScheduleConfigDTO struct {
	Timezone         string        `json:"timezone"`
	SlackUsergroupID string        `json:"slack_usergroup_id,omitempty"`
	L1               ScheduleL1DTO `json:"l1"`
	L2               ScheduleL2DTO `json:"l2"`
}

// PutScheduleConfigRequest is the body of PUT /schedule/config: the
// configuration itself plus what the caller believes about it.
//
// The configuration is embedded rather than copied field by field, so the
// request and the GET response cannot drift apart in shape. JSON promotes the
// embedded fields to the top level, which is the wire format either way.
type PutScheduleConfigRequest struct {
	// ExpectedVersion is the config_version the editor loaded. Zero means
	// "there is no schedule yet"; anything else must match or the save is
	// rejected with the current version.
	ExpectedVersion int64 `json:"expected_version"`

	// Reason is free text recorded with the revision.
	Reason *string `json:"reason,omitempty"`

	ScheduleConfigDTO
}

func (d ScheduleConfigDTO) toConfiguration() rotation.ScheduleConfiguration {
	groups := make([]rotation.RotationGroup, len(d.L1.Groups))
	for i, g := range d.L1.Groups {
		groups[i] = rotation.RotationGroup{
			ID:      g.ID,
			Members: append([]string(nil), g.UserIDs...),
		}
	}
	return rotation.ScheduleConfiguration{
		Timezone:         d.Timezone,
		SlackUsergroupID: d.SlackUsergroupID,
		L1: rotation.LayerConfiguration{
			Enabled: d.L1.Enabled,
			Policy: rotation.RotationPolicy{
				Cadence:       model.RotationType(d.L1.RotationType),
				HandoffTime:   d.L1.HandoffTime,
				HandoffDay:    d.L1.HandoffDay,
			},
			Groups: groups,
		},
		L2: rotation.LayerConfiguration{
			Enabled: d.L2.Enabled,
			Policy: rotation.RotationPolicy{
				Cadence:       model.RotationType(d.L2.RotationType),
				HandoffTime:   d.L2.HandoffTime,
				HandoffDay:    d.L2.HandoffDay,
			},
			// The server owns L2 group identity: it is the user ID itself.
			Groups: rotation.L2GroupsFromUserIDs(d.L2.UserIDs),
		},
		L2EscalationTimeoutMins: d.L2.EscalationTimeoutMinutes,
	}
}

// configDTOFromSnapshot is the inverse, used by every read endpoint. It goes
// through ConfigurationFromSnapshot so the generated fields are dropped in one
// place rather than skipped field by field here.
func configDTOFromSnapshot(snap rotation.ScheduleRevisionSnapshot) ScheduleConfigDTO {
	cfg := rotation.ConfigurationFromSnapshot(snap)

	groups := make([]ScheduleGroupDTO, len(cfg.L1.Groups))
	for i, g := range cfg.L1.Groups {
		groups[i] = ScheduleGroupDTO{ID: g.ID, UserIDs: append([]string(nil), g.Members...)}
	}
	userIDs := make([]string, len(cfg.L2.Groups))
	for i, g := range cfg.L2.Groups {
		userIDs[i] = g.ID
	}

	return ScheduleConfigDTO{
		Timezone:         cfg.Timezone,
		SlackUsergroupID: cfg.SlackUsergroupID,
		L1: ScheduleL1DTO{
			Enabled:      cfg.L1.Enabled,
			RotationType: string(cfg.L1.Policy.Cadence),
			HandoffTime:  cfg.L1.Policy.HandoffTime,
			HandoffDay:   cfg.L1.Policy.HandoffDay,
			Groups:       groups,
		},
		L2: ScheduleL2DTO{
			Enabled:                  cfg.L2.Enabled,
			EscalationTimeoutMinutes: cfg.L2EscalationTimeoutMins,
			RotationType:             string(cfg.L2.Policy.Cadence),
			HandoffTime:              cfg.L2.Policy.HandoffTime,
			HandoffDay:               cfg.L2.Policy.HandoffDay,
			UserIDs:                  userIDs,
		},
	}
}

// ScheduleConfigResponse is GET /schedule/config.
type ScheduleConfigResponse struct {
	ScheduleID    string            `json:"schedule_id"`
	Version       int64             `json:"version"`
	RevisionID    string            `json:"revision_id"`
	EffectiveFrom time.Time         `json:"effective_from"`
	DeletedAt     *time.Time        `json:"deleted_at,omitempty"`
	Config        ScheduleConfigDTO `json:"config"`
}

// PutScheduleConfigResponse says what the save did, and nothing about the world
// after it.
//
// Every field is present even when nothing was written: a no-op still has a
// version and a revision in force, and an editor that had to special-case the
// no-op response would end up with two ways to read the same answer.
//
// It used to carry on_call_after, rendered by a second read AFTER the commit -
// so a failure of that read answered 500 for a command that had already been
// applied. The response lied about the outcome, which is worse than being
// terse. Who is on duty now is a separate question, asked by a separate
// request that is free to fail on its own.
type PutScheduleConfigResponse struct {
	Version    int64  `json:"version"`
	RevisionID string `json:"revision_id"`
	Noop       bool   `json:"noop"`
	Created    bool   `json:"created"`
	Recreated  bool   `json:"recreated"`
}

// LayerOnCallDTO is who is on duty on one layer.
//
// Both pairs of boundaries are reported. The grid pair is where the handoff
// math puts the shift; the assignment pair is when this particular composition
// actually applied. After a mid-shift edit they differ, and one field cannot
// honestly mean both.
type LayerOnCallDTO struct {
	GroupID string   `json:"group_id"`
	UserIDs []string `json:"user_ids"`

	GridSlotStart   time.Time `json:"grid_slot_start"`
	GridSlotEnd     time.Time `json:"grid_slot_end"`
	AssignmentStart time.Time `json:"assignment_start"`
	AssignmentEnd   time.Time `json:"assignment_end"`

	RevisionID string `json:"revision_id"`
	Source     string `json:"source"` // rotation | override
	OverrideID string `json:"override_id,omitempty"`
}

// OnCallDTO is the current-assignment projection. A null layer means nobody is
// on duty there.
//
// Warnings belong here rather than beside the projection: an override overlap
// is no less real for being seen through the current view than through the
// history, and a caller that got the projection without them would have to
// re-render a range to find out that the answer is contested. Carrying them
// inside the value means every path that returns a projection - the on-call
// endpoint, the preview's before/after, the save's result - reports the same
// thing without three chances to forget.
type OnCallDTO struct {
	At       time.Time            `json:"at"`
	L1       *LayerOnCallDTO      `json:"l1"`
	L2       *LayerOnCallDTO      `json:"l2"`
	Warnings []ScheduleWarningDTO `json:"warnings"`
}

func onCallDTO(o schedulerender.OnCall) OnCallDTO {
	return OnCallDTO{
		At:       o.At,
		L1:       layerOnCallDTO(o.L1),
		L2:       layerOnCallDTO(o.L2),
		Warnings: warningDTOs(o.Warnings),
	}
}

func layerOnCallDTO(l *schedulerender.LayerOnCall) *LayerOnCallDTO {
	if l == nil {
		return nil
	}
	return &LayerOnCallDTO{
		GroupID:         l.GroupID,
		UserIDs:         l.UserIDs,
		GridSlotStart:   l.GridSlotStart,
		GridSlotEnd:     l.GridSlotEnd,
		AssignmentStart: l.AssignmentStart,
		AssignmentEnd:   l.AssignmentEnd,
		RevisionID:      l.ScheduleRevisionID,
		Source:          l.Source,
		OverrideID:      l.OverrideID,
	}
}

// ShiftDTO is one natural shift: adjacent grid slots with the same duty merged.
type ShiftDTO struct {
	Layer   string   `json:"layer"`
	Source  string   `json:"source"`
	GroupID string   `json:"group_id"`
	UserIDs []string `json:"user_ids"`

	Start time.Time `json:"start"`
	End   time.Time `json:"end"`

	// No grid boundaries, slot count or contributing revisions. A merged shift
	// spans several slots, so those described nothing a caller could act on,
	// and nothing read them. Where one slot's boundaries matter - the DM about
	// a handoff, the editor showing that an assignment started mid-shift -
	// they come from LayerOnCallDTO, which is about exactly one slot.
	OverrideID         string `json:"override_id,omitempty"`
	OverrideRevisionID string `json:"override_revision_id,omitempty"`
}

func shiftDTOs(shifts []schedulerender.Shift) []ShiftDTO {
	out := make([]ShiftDTO, len(shifts))
	for i, s := range shifts {
		out[i] = ShiftDTO{
			Layer:              s.Layer,
			Source:             s.Source,
			GroupID:            s.GroupID,
			UserIDs:            s.UserIDs,
			Start:              s.Start,
			End:                s.End,
			OverrideID:         s.OverrideID,
			OverrideRevisionID: s.OverrideRevisionID,
		}
	}
	return out
}

// ScheduleOnCallResponse is who is on duty right now.
//
// It also answers "is there a schedule here at all", which is a different
// question from "is anyone on duty" and cannot be derived from the projection:
// a team with no schedule, a deleted one and a live one between shifts all put
// nobody on call. ScheduleID is empty when there is no schedule in this model,
// and DeletedAt is set when there is one but it has been deactivated.
//
// Carrying both here is what lets a widget render without also fetching the
// configuration - a request whose ordinary answer would be 404, once per team.
type ScheduleOnCallResponse struct {
	ScheduleID string     `json:"schedule_id,omitempty"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
	OnCall     OnCallDTO  `json:"on_call"`
}

// ScheduleWarningDTO is a structured condition the caller has to branch on.
// It is not a message: a client that had to parse prose to tell "history is
// incomplete" from "the chain has a hole" would break on the first rewording.
type ScheduleWarningDTO struct {
	Code       string    `json:"code"`
	Layer      string    `json:"layer,omitempty"`
	From       time.Time `json:"from"`
	Until      time.Time `json:"until"`
	RelatedIDs []string  `json:"related_ids,omitempty"`
}

func warningDTOs(warnings []schedulerender.Warning) []ScheduleWarningDTO {
	out := make([]ScheduleWarningDTO, len(warnings))
	for i, w := range warnings {
		out[i] = ScheduleWarningDTO{
			Code:       string(w.Code),
			Layer:      w.Layer,
			From:       w.From,
			Until:      w.Until,
			RelatedIDs: w.RelatedIDs,
		}
	}
	return out
}

// SchedulePreviewResponse is what a save would do, without doing it.
type SchedulePreviewResponse struct {
	EvaluatedAt   time.Time            `json:"evaluated_at"`
	BaseVersion   int64                `json:"base_version"`
	OnCallBefore  OnCallDTO            `json:"on_call_before"`
	OnCallAfter   OnCallDTO            `json:"on_call_after"`
	OnCallChanged bool                 `json:"on_call_changed"`
	Entries       []ShiftDTO           `json:"entries"`
	Warnings      []ScheduleWarningDTO `json:"warnings"`
}

// ScheduleRenderResponse is the rendered calendar range.
type ScheduleRenderResponse struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`

	// HistoryComplete is false when part of the range precedes the point from
	// which this schedule's history is exact. Inferred history is never
	// returned as if it had been recorded.
	HistoryComplete     bool       `json:"history_complete"`
	HistoryCompleteFrom *time.Time `json:"history_complete_from,omitempty"`

	DeletedAt *time.Time           `json:"deleted_at,omitempty"`
	Entries   []ShiftDTO           `json:"entries"`
	Warnings  []ScheduleWarningDTO `json:"warnings"`
}

// ScheduleRevisionDTO is one entry of the audit trail. Config is set only on
// the single-revision endpoint: a list of snapshots would be large and nobody
// reads more than one at a time.
type ScheduleRevisionDTO struct {
	RevisionID    string                  `json:"revision_id"`
	Version       int64                   `json:"version"`
	Kind          string                  `json:"kind"` // active | deleted
	EffectiveFrom time.Time               `json:"effective_from"`
	EffectiveTo   *time.Time              `json:"effective_to,omitempty"`
	RecordedAt    time.Time               `json:"recorded_at"`
	CreatedBy     *string                 `json:"created_by,omitempty"`
	ChangeReason  *string                 `json:"change_reason,omitempty"`
	ChangeSummary *rotation.ChangeSummary `json:"change_summary,omitempty"`
	Config        *ScheduleConfigDTO      `json:"config,omitempty"`
}

func scheduleRevisionDTO(rev scheduleconfig.ScheduleRevision, withConfig bool) ScheduleRevisionDTO {
	out := ScheduleRevisionDTO{
		RevisionID:    rev.ID,
		Version:       rev.Version,
		Kind:          rev.Kind,
		EffectiveFrom: rev.EffectiveFrom,
		EffectiveTo:   rev.EffectiveTo,
		RecordedAt:    rev.RecordedAt,
		CreatedBy:     rev.CreatedBy,
		ChangeReason:  rev.ChangeReason,
		ChangeSummary: rev.ChangeSummary,
	}
	if withConfig {
		cfg := configDTOFromSnapshot(rev.Snapshot)
		out.Config = &cfg
	}
	return out
}

// ScheduleRevisionListResponse is one page of the audit trail, newest first.
type ScheduleRevisionListResponse struct {
	Revisions []ScheduleRevisionDTO `json:"revisions"`

	// NextBeforeVersion is the cursor for the following page, absent when this
	// page is the end. A version cursor rather than an offset: versions are
	// dense and strictly increasing, so a page cannot shift under a reader.
	NextBeforeVersion *int64 `json:"next_before_version,omitempty"`
}

// ScheduleOverrideDTO is the head revision of one logical override - what the
// editor needs to show it and to send back a well-formed edit.
type ScheduleOverrideDTO struct {
	OverrideID string    `json:"override_id"`
	Revision   int64     `json:"revision"`
	UserID     string    `json:"user_id"`
	ValidFrom  time.Time `json:"valid_from"`
	ValidTo    time.Time `json:"valid_to"`
	Reason     *string   `json:"reason,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	RecordedBy *string   `json:"recorded_by,omitempty"`
}

func scheduleOverrideDTO(rev scheduleconfig.OverrideRevision) ScheduleOverrideDTO {
	return ScheduleOverrideDTO{
		OverrideID: rev.OverrideID,
		Revision:   rev.Revision,
		UserID:     rev.UserID,
		ValidFrom:  rev.ValidFrom,
		ValidTo:    rev.ValidTo,
		Reason:     rev.Reason,
		RecordedAt: rev.RecordedAt,
		RecordedBy: rev.RecordedBy,
	}
}

// ScheduleOverrideListResponse is every override that currently exists.
type ScheduleOverrideListResponse struct {
	Overrides []ScheduleOverrideDTO `json:"overrides"`
}

// ScheduleOverrideRequest creates or updates an override.
//
// ExpectedRevision is used by PUT only; DELETE takes it as a query parameter,
// because a body on a DELETE is not carried reliably by every client and proxy
// in the path.
type ScheduleOverrideRequest struct {
	UserID           string    `json:"user_id"`
	ValidFrom        time.Time `json:"valid_from"`
	ValidTo          time.Time `json:"valid_to"`
	Reason           *string   `json:"reason,omitempty"`
	ExpectedRevision int64     `json:"expected_revision"`
}
