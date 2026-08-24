package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// The producer: turning an alert group and its policy into an admission.
//
// This is where everything a message depends on is decided, once. After it, a
// delivery has an identity and a frozen state to render from, and the only
// answer it can give on a retry is the same answer - which is the whole reason
// the snapshot, the links, the team's state and the buttons are settled HERE
// rather than read again by whoever sends (S1-D28).
//
// It is also the only place that spends an alert's one chance to page: nothing
// picks up a group that already has an admission. So the difference between
// "the schedule says nobody" and "the schedule could not be read" is load
// bearing, and the two leave by different doors - the first is an outcome, the
// second is a deferral that keeps the group for the next tick.

// ErrOnCallResolutionUnavailable means the recipients could not be established,
// which is not the same as there being none. Nothing is admitted, the group
// stays new, and the next tick asks again.
var ErrOnCallResolutionUnavailable = errors.New("on-call recipients could not be resolved")

// planStore is what building a plan reads.
type planStore interface {
	GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error)
	GetUsersByIDs(ids []string) ([]*model.User, error)
	GetTeamByID(id string) (*model.Team, error)
}

// channelSettings is the configuration a MESSAGE depends on, as opposed to the
// configuration a call depends on.
//
// Only the first is frozen. Whether buttons are offered changes the bytes of a
// card, so it is decided here and travels with the commitment; the token to
// send it with is read at each attempt, because rotating one has to apply to
// work that has not gone out yet.
type channelSettings interface {
	GetSlackInteractive() bool
	GetTelegramInteractive() bool
}

type planner struct {
	store    planStore
	oncall   onCallProjection
	settings channelSettings
	cfg      *config.Config
}

// firehoseProvider: the firehose is Slack-only, deliberately, as it was.
const firehoseProvider = "slack"

// buildPlan decides what an alert group promises, and to whom.
//
// teamOnCall is who is on duty for the alert's team, read once by the caller
// and handed in: the snapshot stored on the group and the people paged have to
// be the same answer, and two reads a moment apart can straddle a handoff.
func (p *planner) buildPlan(ctx context.Context, ag *model.AlertGroup, policyID string,
	teamOnCall schedulerender.TeamOnCallResult) (outbound.EscalationAdmission, error) {

	policy := p.policyFor(policyID)
	if policy == nil {
		policyID = ""
	}

	state, err := p.freeze(ag)
	if err != nil {
		return outbound.EscalationAdmission{}, err
	}

	commitments, unattended, err := p.commitments(ctx, ag, policy, policyID, teamOnCall)
	if err != nil {
		return outbound.EscalationAdmission{}, err
	}

	admission, err := keys.EscalationBatch{
		Kind:               keys.KindEscalation,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Snapshot:           state,
		Commitments:        commitments,
	}.Admit()
	if err != nil {
		return outbound.EscalationAdmission{}, fmt.Errorf("build the admission for %s: %w", ag.ID, err)
	}

	return outbound.EscalationAdmission{
		Admission:              admission,
		PolicyID:               policyID,
		PolicySnapshot:         policySnapshot(policyID, policy, commitments),
		StepsWithoutRecipients: unattended,
		Actor:                  "engine",
	}, nil
}

// freeze takes the state every message of this escalation will be rendered
// from, at revision 0.
//
// Everything that would otherwise be read again at send time is settled here:
// the links whole rather than a base URL, whether the alert's team is set up in
// TokayOps, and the zone times are printed in. Two instances, or one instance
// an hour later, then render the same bytes.
func (p *planner) freeze(ag *model.AlertGroup) (keys.RenderSnapshot, error) {
	selfURL := ""
	if p.cfg != nil {
		selfURL = p.cfg.Global.SelfURL
	}

	state, err := providers.SnapshotOf(providers.GroupView{
		Group:         ag,
		SelfURL:       selfURL,
		TeamOnboarded: p.teamIsOnboarded(ag.TeamID),
		// The zone this instance was told to use, frozen now. It is not read
		// again: an instance in another zone rendering this snapshot has to
		// produce the same message.
		Zone: providers.ProcessZone(),
	})
	if err != nil {
		return keys.RenderSnapshot{}, fmt.Errorf("freeze the state of %s: %w", ag.ID, err)
	}
	return state, nil
}

// teamIsOnboarded asks once, here, whether the alert's team exists in TokayOps.
// A card says so where its buttons would be, and asking again at send time
// would let that answer change between two attempts of one delivery.
func (p *planner) teamIsOnboarded(teamID string) bool {
	if teamID == "" || p.store == nil {
		return true
	}
	team, err := p.store.GetTeamByID(teamID)
	if err != nil {
		// The same direction the channels degraded in: deciding "not onboarded"
		// on a database blip strips the buttons from teams that are set up
		// perfectly well, and does it exactly when alerts arrive in bulk.
		log.Printf("AlertEngine: team lookup for %q failed, assuming onboarded: %v", teamID, err)
		return true
	}
	return team != nil
}

// commitments turns the plan into promises: the firehose first, then the policy
// steps in order.
func (p *planner) commitments(ctx context.Context, ag *model.AlertGroup,
	policy *model.EscalationPolicy, policyID string,
	teamOnCall schedulerender.TeamOnCallResult) ([]keys.EscalationCommitment, []string, error) {

	var (
		out        []keys.EscalationCommitment
		unattended []string
	)

	if channel := p.firehoseChannel(ag.Severity); channel != "" {
		out = append(out, keys.EscalationCommitment{
			Slot:            keys.Slot{Kind: keys.SlotFirehose},
			Provider:        firehoseProvider,
			Target:          keys.Target{Kind: keys.TargetChannel, Ref: channel},
			Editable:        true,
			Interactive:     p.interactiveOn(firehoseProvider),
			Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
			CompletionMode:  keys.CompletionOnAcceptance,
			AmbiguityPolicy: keys.PolicyRetry,
		})
	}

	if policy == nil {
		return out, unattended, nil
	}

	resolver := &scheduleResolver{store: p.store, oncall: p.oncall, team: teamOnCall}

	// The offset is cumulative from ADMISSION, not from the previous step
	// finishing (S1-D4). A step that is still retrying no longer holds the rest
	// of the escalation back: the promise of step one outlives its own delay,
	// and step two goes out when the policy said it would.
	offset := time.Duration(0)
	for index, step := range policy.Steps {
		if step == nil {
			continue
		}
		offset += time.Duration(step.DelaySeconds) * time.Second

		// A provider nothing here delivers through is refused by the admission
		// gate, and that refusal takes the WHOLE batch with it: one misspelled
		// step would cost the alert its firehose, on every tick, forever. So it
		// is asked in advance and the step is recorded rather than promised.
		if !outbound.DeliversThrough(step.Provider) {
			unattended = append(unattended, fmt.Sprintf("%d (%s: no channel for provider %q)",
				index+1, describeStep(step), step.Provider))
			continue
		}

		recipients, err := p.recipients(ctx, resolver, step)
		if err != nil {
			return nil, nil, err
		}
		if len(recipients) == 0 {
			// A step that resolved to nobody produces no commitment. An intent
			// that is certain to fail is an alert about a failure the system
			// knew about in advance; the alert's history is where this belongs.
			unattended = append(unattended, fmt.Sprintf("%d (%s)", index+1, describeStep(step)))
			continue
		}

		for _, target := range recipients {
			out = append(out, keys.EscalationCommitment{
				Slot:            keys.Slot{Kind: keys.SlotPolicy, Index: index},
				Provider:        step.Provider,
				Target:          target,
				Editable:        step.TargetKind == "channel",
				MessageOverride: optionalText(step.Message),
				Interactive:     p.interactiveOn(step.Provider),
				Timing: keys.TimingSpec{
					Kind: keys.TimingRelativeToAdmission, Offset: offset,
				},
				CompletionMode:  keys.CompletionOnAcceptance,
				AmbiguityPolicy: keys.PolicyRetry,
			})
		}
	}
	return out, unattended, nil
}

// recipients is who one policy step names, as this system names them.
//
// A schedule is resolved to the people on it; anything else names its recipient
// directly. What comes back is never a provider address - turning a person into
// a Slack account is preparation's job, redone on every attempt, because an
// identity relinked in between must not move a message that may already exist.
func (p *planner) recipients(ctx context.Context, resolver *scheduleResolver,
	step *model.EscalationStep) ([]keys.Target, error) {

	if step.TargetType == "schedule" {
		users, err := resolver.usersFor(ctx, step.TargetID)
		if err != nil {
			return nil, err
		}
		targets := make([]keys.Target, 0, len(users))
		for _, user := range users {
			targets = append(targets, keys.Target{Kind: keys.TargetUser, Ref: user.ID})
		}
		return targets, nil
	}

	if step.TargetID == "" {
		return nil, nil
	}
	kind := keys.TargetUser
	if step.TargetType == "channel" {
		kind = keys.TargetChannel
	}
	return []keys.Target{{Kind: kind, Ref: step.TargetID}}, nil
}

func (p *planner) firehoseChannel(severity string) string {
	if p.cfg == nil {
		return ""
	}
	if severity == "critical" {
		return p.cfg.Global.FirehoseCriticalChannel
	}
	return p.cfg.Global.FirehoseWarningChannel
}

// interactiveOn says whether this provider's messages may carry buttons.
//
// Frozen per commitment because it changes the bytes: a card whose buttons come
// and go between two attempts is two different messages under one key. The cost
// is named in the plan - interactivity switched on after an alert was admitted
// does not appear on cards already promised.
func (p *planner) interactiveOn(provider string) bool {
	if p.settings == nil {
		return false
	}
	switch provider {
	case "slack":
		return p.settings.GetSlackInteractive()
	case "telegram":
		// Telegram's buttons need somewhere to send people back to, and that
		// link comes from this instance's own URL.
		return p.settings.GetTelegramInteractive() && p.cfg != nil && p.cfg.Global.SelfURL != ""
	default:
		return false
	}
}

func (p *planner) policyFor(policyID string) *model.EscalationPolicy {
	if policyID == "" || p.store == nil {
		return nil
	}
	policy, err := p.store.GetEscalationPolicyByID(policyID)
	if err != nil {
		// The same degradation as before: an alert still reaches its firehose
		// channel when the policy behind it cannot be read.
		log.Printf("AlertEngine: policy %s not found (%v), admitting the firehose only", policyID, err)
		return nil
	}
	return policy
}

// scheduleResolver answers "who is on duty for this step" consistently within
// one plan.
//
// The team's answer is read once by the caller and handed in; a schedule read
// by id is remembered here for the same reason one level down - two policy
// steps naming the same schedule are one question, and neither a handoff nor a
// transient failure landing between them may split the answer.
type scheduleResolver struct {
	store  planStore
	oncall onCallProjection
	team   schedulerender.TeamOnCallResult

	teamUsers *resolution
	byID      map[string]resolution
}

type resolution struct {
	users []*model.User
	err   error
}

func (r *scheduleResolver) usersFor(ctx context.Context, fallbackScheduleID string) ([]*model.User, error) {
	// An unreadable team is not a team without a schedule. Falling through to
	// the stored id here would answer a question that could not be asked, out
	// of a schedule the team may no longer own.
	if err := r.team.Err(); err != nil {
		return nil, fmt.Errorf("%w: the team's on-call state is unreadable: %w",
			ErrOnCallResolutionUnavailable, err)
	}

	// A team WITH a schedule answers for itself, even if that schedule is
	// deleted or between shifts: "nobody" is its answer, and falling through to
	// the stored id would answer with somebody else's schedule.
	if team := r.team.OnCall(); team.ScheduleID != "" {
		if r.teamUsers == nil {
			users, err := r.hydrate(team.OnCall)
			r.teamUsers = &resolution{users: users, err: err}
		}
		return r.teamUsers.users, r.teamUsers.err
	}

	if fallbackScheduleID == "" {
		return nil, nil
	}
	if seen, ok := r.byID[fallbackScheduleID]; ok {
		return seen.users, seen.err
	}

	var answer resolution
	onCall, err := r.oncall.CurrentOnCallNow(ctx, fallbackScheduleID)
	switch {
	case err != nil:
		reason, damaged := schedulerender.FailureReasonOf(err)
		if damaged {
			// Damaged stored data answers the same way on every retry, so
			// deferring would defer forever. The step resolves to nobody, the
			// history says so, and the rest of the policy still runs.
			log.Printf("AlertEngine: schedule %s is damaged (%s), the step resolves to nobody: %v",
				fallbackScheduleID, reason, err)
		} else {
			answer.err = fmt.Errorf("%w: schedule %s: %w",
				ErrOnCallResolutionUnavailable, fallbackScheduleID, err)
		}
	default:
		answer.users, answer.err = r.hydrate(onCall)
	}

	if r.byID == nil {
		r.byID = map[string]resolution{}
	}
	r.byID[fallbackScheduleID] = answer
	return answer.users, answer.err
}

// hydrate turns the projected user ids into people, in the order the projection
// gave them so the plan is the same plan on every tick.
//
// People the store does not return are dropped rather than promised to: an
// erased person has no identity left to notify, and a commitment aimed at them
// could only fail.
func (r *scheduleResolver) hydrate(onCall schedulerender.OnCall) ([]*model.User, error) {
	if onCall.L1 == nil || len(onCall.L1.UserIDs) == 0 {
		return nil, nil
	}
	fetched, err := r.store.GetUsersByIDs(onCall.L1.UserIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOnCallResolutionUnavailable, err)
	}
	byID := make(map[string]*model.User, len(fetched))
	for _, u := range fetched {
		byID[u.ID] = u
	}
	out := make([]*model.User, 0, len(onCall.L1.UserIDs))
	for _, id := range onCall.L1.UserIDs {
		if u, ok := byID[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// policySnapshot is what the group records about the policy it was escalated
// by: what was decided, in the shape the API already reads.
func policySnapshot(policyID string, policy *model.EscalationPolicy,
	commitments []keys.EscalationCommitment) json.RawMessage {

	snapshot := model.EscalationPolicySnapshot{PolicyID: policyID}
	if policy != nil {
		snapshot.Name = policy.Name
	}
	for _, commitment := range commitments {
		snapshot.Steps = append(snapshot.Steps, &model.EscalationStepSnapshot{
			Provider:   commitment.Provider,
			TargetKind: string(commitment.Target.Kind),
			TargetType: string(commitment.Target.Kind),
			TargetID:   commitment.Target.Ref,
			IsFirehose: commitment.Slot.Kind == keys.SlotFirehose,
			StageIndex: commitment.Slot.Index,
		})
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		// The fields are strings and ints; a failure here is a broken build.
		log.Printf("AlertEngine: the policy snapshot could not be recorded: %v", err)
		return nil
	}
	return raw
}

func describeStep(step *model.EscalationStep) string {
	if step.TargetType == "schedule" {
		return "schedule " + step.TargetID
	}
	if step.TargetID == "" {
		return step.TargetType + " with no target"
	}
	return step.TargetType + " " + step.TargetID
}

func optionalText(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}
