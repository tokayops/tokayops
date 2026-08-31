package engine

import (
	"context"
	"database/sql"
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
// rather than read again by whoever sends.
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

	// firehose is the channel THIS alert's severity routes to, settled when
	// the plan starts rather than asked again while it is being built.
	firehose string
}

// firehoseProvider: the firehose is Slack-only, deliberately, as it was.
const firehoseProvider = "slack"

// buildPlan decides what an alert group promises, and to whom.
//
// teamOnCall is who is on duty for the alert's team, read once by the caller
// and handed in: the snapshot stored on the group and the people paged have to
// be the same answer, and two reads a moment apart can straddle a handoff.
func (p *planner) buildPlan(ctx context.Context, ag *model.AlertGroup,
	teamOnCall schedulerender.TeamOnCallResult) (outbound.Batch, error) {

	team, err := p.teamFor(ag.TeamID)
	if err != nil {
		return outbound.Batch{}, err
	}

	policyID := team.route(ag.Severity)
	policy, err := p.policyFor(policyID)
	if err != nil {
		return outbound.Batch{}, err
	}
	if policy == nil {
		policyID = ""
	}

	state, err := p.freeze(ag, team)
	if err != nil {
		return outbound.Batch{}, err
	}

	plan := *p
	plan.firehose = p.firehoseChannel(ag.Severity)

	// Who the people named by this plan actually are, resolved once and shared.
	// The commitments and the snapshot on the group are the same answer about
	// the same moment, and two reads a moment apart - or one that came back
	// short - would page Alice while the group said she was not on call.
	people := &roster{store: p.store}

	planned, unpromised, err := plan.commitments(ctx, people, policy, teamOnCall)
	if err != nil {
		return outbound.Batch{}, err
	}

	commitments := make([]keys.EscalationCommitment, 0, len(planned))
	for _, step := range planned {
		commitments = append(commitments, step.commitment)
	}

	admission, err := keys.EscalationBatch{
		Kind:               keys.KindEscalation,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Snapshot:           state,
		Commitments:        commitments,
	}.Admit()
	if err != nil {
		return outbound.Batch{}, fmt.Errorf("build the admission for %s: %w", ag.ID, err)
	}

	return outbound.Batch{
		Admission: admission,
		Context: outbound.EscalatingAlertGroup(outbound.EscalationContext{
			// What the snapshot above was frozen from, checked again under the
			// lock that decides the admission: a plan built a moment too early
			// is refused whole rather than held forever.
			SourceVersion:  ag.RenderSourceVersion,
			PolicyID:       policyID,
			PolicySnapshot: policySnapshot(policyID, policy, planned),
			OnCallSnapshot: plan.onCallSnapshot(ag.ID, people, teamOnCall),
			Unpromised:     unpromised,
		}),
		Actor: "engine",
	}, nil
}

// onCallSnapshot records who was on duty when the alert arrived, for the winner
// of the admission to write on the group.
//
// It takes the projection rather than fetching one: this is the same answer the
// commitments were built from, and that is the whole point of it being a
// parameter. Two reads a moment apart can straddle a handoff, and the group
// would then display one set of people while another set is being paged.
//
// A team with no schedule, or one between shifts, gets an EMPTY snapshot rather
// than none: "nobody was on call" is a fact worth having on the alert group,
// and the readers of the field already treat an empty group as exactly that.
//
// Nothing is recorded when the answer could not be read at all, and that is
// where this parts company with the rest of the plan. A schedule that cannot be
// resolved defers the whole admission, because it decides WHO GETS PAGED and
// the decision is held forever. This decides who is DISPLAYED, and an empty
// field is honest about not knowing - so a database blip costs a line in the
// audit rather than the alert.
//
// Source is what survives of the override information now that the projection
// answers instead of a legacy override row: L1Users already names the stand-in,
// and Source says that is why.
func (p *planner) onCallSnapshot(agID string, people *roster,
	read schedulerender.TeamOnCallResult) json.RawMessage {

	if read.Err() != nil {
		return nil
	}

	team := read.OnCall()
	out := &model.OnCallResult{}
	if l1 := team.OnCall.L1; l1 != nil {
		users, err := people.people(l1.UserIDs)
		if err != nil {
			log.Printf("AlertEngine: the on-call snapshot of %s was not recorded: %v", agID, err)
			return nil
		}
		since, until := l1.AssignmentStart, l1.AssignmentEnd
		out.L1Users = users
		out.L1Since = &since
		out.L1Until = &until
		out.Source = l1.Source
	}
	if l2 := team.OnCall.L2; l2 != nil && len(l2.UserIDs) > 0 {
		users, err := people.people(l2.UserIDs[:1])
		if err != nil {
			log.Printf("AlertEngine: the on-call snapshot of %s was not recorded: %v", agID, err)
			return nil
		}
		if len(users) > 0 {
			out.L2User = users[0]
		}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		log.Printf("AlertEngine: the on-call snapshot of %s was not recorded: %v", agID, err)
		return nil
	}
	return raw
}

// roster is who the people named by one plan are.
//
// It exists so that a plan asks each question once. The commitments and the
// on-call snapshot recorded on the group are the same claim about the same
// moment, and they used to be two reads: a person erased between them, or a
// second answer that came back short, produced a commitment aimed at Alice and
// a group saying Alice was not on call.
//
// People the store does not return are remembered as absent rather than asked
// about again. An erased person has no identity left to notify, and the answer
// is the same however often it is asked.
type roster struct {
	store planStore
	known map[string]*model.User
}

// people resolves ids into people, in the order they were given, dropping
// anyone the store does not have.
func (r *roster) people(ids []string) ([]*model.User, error) {
	if len(ids) == 0 || r.store == nil {
		return nil, nil
	}
	if r.known == nil {
		r.known = map[string]*model.User{}
	}

	var ask []string
	for _, id := range ids {
		if _, asked := r.known[id]; !asked {
			ask = append(ask, id)
		}
	}
	if len(ask) > 0 {
		fetched, err := r.store.GetUsersByIDs(ask)
		if err != nil {
			return nil, err
		}
		for _, id := range ask {
			r.known[id] = nil
		}
		for _, u := range fetched {
			r.known[u.ID] = u
		}
	}

	out := make([]*model.User, 0, len(ids))
	for _, id := range ids {
		if u := r.known[id]; u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// plannedCommitment is one promise, plus what the plan called it.
//
// The two vocabularies are deliberately both here. A commitment names an
// ADDRESS - a user or a channel - because that is what a delivery needs; the
// policy names the SHAPE of the message - a dm or a channel post - because that
// is what somebody configured. Recording one as the other, as an earlier
// version did, makes every direct message read as "channel kind: user" in the
// audit.
type plannedCommitment struct {
	commitment keys.EscalationCommitment
	targetKind string
	firehose   bool
}

// teamRead is the alert's team, read once.
//
// The two questions it answers are asked in different places - which policy
// this alert escalates by, and whether a card may offer buttons - and they used
// to be two reads. That is the whole reason this type exists: with two, a read
// that failed could answer one of them and a read that succeeded the other, and
// the alert would be escalated by nothing at all while its card said the team
// was fine.
type teamRead struct {
	// team is nil when the alert names a team that is not there.
	team *model.Team

	// unnamed: the alert names no team at all. Not the same as a team that is
	// missing - there is nothing here somebody failed to set up, so the card
	// says nothing about it.
	unnamed bool
}

// route is the policy this alert escalates by: the severity's own route if the
// team has one, otherwise the team's default. Empty means the firehose alone.
func (r teamRead) route(severity string) string {
	if r.team == nil {
		return ""
	}
	if policyID, ok := r.team.SeverityRoutes[severity]; ok && policyID != "" {
		return policyID
	}
	return r.team.DefaultPolicyID
}

// onboarded is whether a card may offer buttons that act on this alert.
func (r teamRead) onboarded() bool { return r.unnamed || r.team != nil }

// teamFor reads the alert's team, and distinguishes the two ways that comes
// back empty.
//
// A team that is NOT THERE is a product fact: the alert names a team nobody set
// up, so it escalates by no policy and its card says the buttons would answer
// nobody. A lookup that could not RUN says nothing about the team, and
// answering it either way freezes a guess into an escalation that is held
// forever - firehose-only, permanently, over a database that was busy for a
// second. Nothing is admitted and the next tick asks again.
func (p *planner) teamFor(teamID string) (teamRead, error) {
	if teamID == "" || p.store == nil {
		return teamRead{unnamed: true}, nil
	}

	team, err := p.store.GetTeamByID(teamID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("AlertEngine: team %s does not exist, admitting the firehose only", teamID)
		return teamRead{}, nil
	case err != nil:
		return teamRead{}, fmt.Errorf("%w: team %s: %w", ErrOnCallResolutionUnavailable, teamID, err)
	}
	return teamRead{team: team}, nil
}

// freeze takes the state every message of this escalation will be rendered
// from, at revision 0.
//
// Everything that would otherwise be read again at send time is settled here:
// the links whole rather than a base URL, whether the alert's team is set up in
// TokayOps, and the zone times are printed in. Two instances, or one instance
// an hour later, then render the same bytes.
func (p *planner) freeze(ag *model.AlertGroup, team teamRead) (keys.RenderSnapshot, error) {
	selfURL := ""
	if p.cfg != nil {
		selfURL = p.cfg.Global.SelfURL
	}

	in := providers.ViewOf(providers.GroupView{
		Group:   ag,
		SelfURL: selfURL,
		// Whether the alert's team is set up here, from the same read the
		// routing came from. A card says so where its buttons would be, and
		// asking again at send time would let that answer change between two
		// attempts of one delivery.
		//
		// The channels degrade the other way at send time, on purpose: there
		// the snapshot already holds an answer and the question is only whether
		// to trust a blip over it.
		TeamOnboarded: team.onboarded(),
		// The zone this instance was told to use, frozen now. It is not read
		// again: an instance in another zone rendering this snapshot has to
		// produce the same message.
		Zone: providers.ProcessZone(),
	})

	state, err := keys.NewRenderSnapshot(in)
	if err != nil {
		return keys.RenderSnapshot{}, fmt.Errorf("freeze the state of %s: %w", ag.ID, err)
	}
	return state, nil
}

// commitments turns the plan into promises: the firehose first, then the policy
// steps in order.
func (p *planner) commitments(ctx context.Context, people *roster,
	policy *model.EscalationPolicy, teamOnCall schedulerender.TeamOnCallResult) (
	[]plannedCommitment, []outbound.UnpromisedStep, error) {

	var (
		out        []plannedCommitment
		unpromised []outbound.UnpromisedStep
		seen       = map[string]bool{}
	)

	if p.firehose != "" {
		out = append(out, plannedCommitment{
			commitment: keys.EscalationCommitment{
				Slot:            keys.Slot{Kind: keys.SlotFirehose},
				Provider:        firehoseProvider,
				Target:          keys.Target{Kind: keys.TargetChannel, Ref: p.firehose},
				Editable:        true,
				Interactive:     p.interactiveOn(firehoseProvider),
				Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
				CompletionMode:  keys.CompletionOnAcceptance,
				AmbiguityPolicy: keys.PolicyRetry,
			},
			targetKind: "channel",
			firehose:   true,
		})
	}

	if policy == nil {
		return out, unpromised, nil
	}

	resolver := &scheduleResolver{people: people, oncall: p.oncall, team: teamOnCall}

	// The offset is cumulative from ADMISSION, not from the previous step
	// finishing. A step that is still retrying no longer holds the rest
	// of the escalation back: the promise of step one outlives its own delay,
	// and step two goes out when the policy said it would.
	offset := time.Duration(0)
	for _, step := range policy.Steps {
		if step == nil {
			continue
		}
		offset += time.Duration(step.DelaySeconds) * time.Second

		// The slot is the step's OWN index, not its position in the slice. The
		// two are the same only while a policy's indices happen to be dense,
		// and the slot is part of a commitment's identity: steps 5 and 9
		// admitted as 0 and 1 would be a different escalation than the one the
		// policy describes, and a re-admission after an edit would name them
		// differently again.
		slot := keys.Slot{Kind: keys.SlotPolicy, Index: step.StepIndex}

		// A provider nothing here delivers through is refused by the admission
		// gate, and that refusal takes the WHOLE batch with it: one misspelled
		// step would cost the alert its firehose, on every tick, forever. So it
		// is asked in advance and the step is recorded rather than promised.
		if !outbound.DeliversThrough(step.Provider) {
			unpromised = append(unpromised, outbound.UnpromisedStep{
				Step:   describeStep(step),
				Reason: outbound.ReasonNoChannel,
				Detail: fmt.Sprintf("nothing here delivers through %q", step.Provider),
			})
			continue
		}

		recipients, err := p.recipients(ctx, resolver, step)
		if err != nil {
			return nil, nil, err
		}
		if len(recipients) == 0 {
			// A step that named nobody produces no commitment. An intent that
			// is certain to fail is an alert about a failure the system knew
			// about in advance; the alert's history is where this belongs, and
			// it says which of the two happened.
			reason := outbound.ReasonNobodyOnCall
			if step.TargetType != "schedule" {
				reason = outbound.ReasonNoTarget
			}
			unpromised = append(unpromised, outbound.UnpromisedStep{
				Step: describeStep(step), Reason: reason,
			})
			continue
		}

		for _, target := range recipients {
			// Two steps that share an index AND a recipient are one commitment
			// by identity, and the grammar refuses an admission that contains
			// the same key twice. Refused there it would be refused on every
			// tick, forever, over a policy somebody can fix - so the repeat is
			// recorded here and the escalation goes out.
			if key := slotTarget(slot, step.Provider, target); seen[key] {
				unpromised = append(unpromised, outbound.UnpromisedStep{
					Step:   describeStep(step),
					Reason: outbound.ReasonDuplicate,
					Detail: fmt.Sprintf("%s %s is already promised in step %d",
						target.Kind, target.Ref, step.StepIndex),
				})
				continue
			} else {
				seen[key] = true
			}

			out = append(out, plannedCommitment{
				commitment: keys.EscalationCommitment{
					Slot:            slot,
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
				},
				targetKind: step.TargetKind,
			})
		}
	}
	return out, unpromised, nil
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

// policyFor reads the policy this group escalates by, and distinguishes the two
// ways that can come back empty.
//
// A policy that is NOT THERE is a product fact: somebody deleted it, or the
// routing names one that never existed, and the alert still reaches its
// firehose channel. A policy that could not be READ is a database that is
// unavailable, and answering "firehose only" to that would page a fraction of
// the people this alert is supposed to reach - permanently, because an
// admission is held forever.
func (p *planner) policyFor(policyID string) (*model.EscalationPolicy, error) {
	if policyID == "" || p.store == nil {
		return nil, nil
	}

	policy, err := p.store.GetEscalationPolicyByID(policyID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("AlertEngine: policy %s does not exist, admitting the firehose only", policyID)
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: policy %s: %w", ErrOnCallResolutionUnavailable, policyID, err)
	}
	return policy, nil
}

// scheduleResolver answers "who is on duty for this step" consistently within
// one plan.
//
// The team's answer is read once by the caller and handed in; a schedule read
// by id is remembered here for the same reason one level down - two policy
// steps naming the same schedule are one question, and neither a handoff nor a
// transient failure landing between them may split the answer.
type scheduleResolver struct {
	people *roster
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
	users, err := r.people.people(onCall.L1.UserIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOnCallResolutionUnavailable, err)
	}
	return users, nil
}

// policySnapshot is what the group records about the policy it was escalated
// by: what was decided, in the shape the API already reads.
func policySnapshot(policyID string, policy *model.EscalationPolicy,
	planned []plannedCommitment) json.RawMessage {

	snapshot := model.EscalationPolicySnapshot{PolicyID: policyID}
	if policy != nil {
		snapshot.Name = policy.Name
	}
	for _, step := range planned {
		snapshot.Steps = append(snapshot.Steps, &model.EscalationStepSnapshot{
			Provider: step.commitment.Provider,
			// The shape of the message, as the policy words it, and the kind of
			// address it went to. They are different questions: a dm is
			// addressed to a user, and recording "user" as the kind would make
			// every direct message read as a target kind nobody configures.
			TargetKind: step.targetKind,
			TargetType: string(step.commitment.Target.Kind),
			TargetID:   step.commitment.Target.Ref,
			IsFirehose: step.firehose,
			StageIndex: step.commitment.Slot.Index,
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

// slotTarget is what makes two commitments the same commitment: the same slot
// of the plan, the same provider, the same recipient. It mirrors the business
// key rather than restating it - the grammar is what decides identity, and this
// only has to agree with it about repeats.
func slotTarget(slot keys.Slot, provider string, target keys.Target) string {
	return fmt.Sprintf("%s/%d/%s/%s/%s", slot.Kind, slot.Index, provider, target.Kind, target.Ref)
}

// describeStep names one step of a policy the way its history has to: by its
// index first, because that is what tells two steps apart. A policy naming the
// same schedule twice produces two lines, and without the index they are the
// same sentence about different work.
func describeStep(step *model.EscalationStep) string {
	return fmt.Sprintf("%d (%s)", step.StepIndex, describeTarget(step))
}

func describeTarget(step *model.EscalationStep) string {
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
