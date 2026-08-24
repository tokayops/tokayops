// Package jobdedup declares what makes two background jobs the same work.
//
// A job carries a Spec: the pair (Namespace, Key) names the work, and Scope
// says how long that name is exclusive. The pair is the identity; the scope is
// a policy ABOUT that identity and deliberately not part of it. Were scope part
// of the identity, the same business event under two scopes would be two
// different jobs - which is the defect this package exists to remove, not a
// distinction anyone wants.
//
// Everything else about a family follows from its namespace, and follows from
// it HERE. The alternative - independent fields on a job that a caller fills in
// and something else checks for agreement - is how a job came to be able to
// claim one family while answering for another, which is a page nobody gets.
// So the namespace is a closed set, each member declares the scope and the job
// type that go with it, and a Spec cannot be assembled any other way: its
// fields are readable and not writable, and the constructors below are the only
// way to make one.
package jobdedup

import (
	"fmt"
	"sort"
)

// Scope is how long an identity stays exclusive.
type Scope string

const (
	// ScopeWhileActive lets the work happen again once the previous job has
	// left pending/running.
	ScopeWhileActive Scope = "while_active"
	// ScopeForever admits the work once, whatever became of the job.
	ScopeForever Scope = "forever"
)

// Namespace is a family of jobs.
type Namespace string

const (
	// NamespaceEscalation is history and has no constructor. The escalation
	// left the job engine in Epic 12: it is a set of commitments in the
	// outbound domain now, and nothing writes a job under this name any more.
	//
	// It stays declared for one reason only - the registry is how a row is
	// READ, and PolicyOf on an unknown namespace is an error. Rows written
	// before the cutover stay readable; that is all this name does. The whole
	// registry goes in Sprint 3.
	//
	// It is NOT a start-up guard. Nothing refuses to serve a database that
	// still holds an active escalation job, and nothing needs to: the upgrade
	// is a stop-the-world cutover, and a leftover job fails on its first step
	// because no executor takes "dm", "channel" or "firehose" any more. Failing
	// is the right end for it - the group it belongs to has no admission, so
	// the producer picks the group up and escalates it properly.
	NamespaceEscalation  Namespace = "escalation"
	NamespaceAckUpdate   Namespace = "ack_update"
	NamespaceAlertUpdate Namespace = "alert_update"
	NamespaceResolution  Namespace = "resolution"

	// NamespaceHandoff is history and has no constructor. Handover
	// notifications are written under NamespaceHandoffOccurrence below; this
	// name owns the rows written before they were identified by the occurrence
	// they announce, under the policy those rows were written with.
	//
	// It stays declared because the registry is how a row is read: an unknown
	// namespace makes the row unreadable, jobs are not deleted, and a
	// while_active row that has finished holds no claim anyone needs to
	// reclaim - so there is nothing to migrate and nothing to gain by
	// forgetting the name.
	NamespaceHandoff Namespace = "handoff"

	// NamespaceHandoffOccurrence identifies a handover by the occurrence it
	// announces, and holds that identity forever.
	NamespaceHandoffOccurrence Namespace = "handoff_occurrence"
)

// Policy is everything a namespace determines: how long its identities are
// exclusive, and the job type its rows carry.
//
// The job type is not a second name for the family. It predates the namespace
// and is what the engine's own queries read - "has this group been escalated"
// asks about the type - so a row whose type and namespace disagree holds a
// claim under one name while answering under another. Keeping the two in one
// table is what makes them impossible to state separately.
type Policy struct {
	Namespace Namespace
	Scope     Scope
	JobType   string
}

// determined is what a namespace decides about its family. The namespace itself
// is not in here: it is the key, and writing it twice would let a typo make
// PolicyOf and Policies describe two different registries.
type determined struct {
	scope   Scope
	jobType string
}

// policies is the whole model of what each family is.
//
// A namespace never changes its policy. Changing one would mean claiming, in
// hindsight, that every row already written under that name meant something
// else; a family whose policy changes takes a NEW name instead, and both stay
// here - the old one owning its history, the new one owning what follows.
var policies = map[Namespace]determined{
	NamespaceEscalation:  {ScopeForever, "escalation"},
	NamespaceAckUpdate:   {ScopeWhileActive, "update"},
	NamespaceAlertUpdate: {ScopeWhileActive, "update"},
	NamespaceResolution:  {ScopeWhileActive, "resolution"},

	// Both handover families carry the same job type and differ in what a row
	// of theirs claims: the first for as long as the job runs, the second for
	// good. That is exactly why they are two names.
	NamespaceHandoff:           {ScopeWhileActive, "handoff_notify"},
	NamespaceHandoffOccurrence: {ScopeForever, "handoff_notify"},
}

// Policies returns every known namespace with what it determines, ordered by
// namespace so a seed writes the same rows in the same order every time.
func Policies() []Policy {
	out := make([]Policy, 0, len(policies))
	for ns, d := range policies {
		out = append(out, Policy{Namespace: ns, Scope: d.scope, JobType: d.jobType})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

// PolicyOf reports what a namespace determines, and whether it is known at all.
func PolicyOf(ns Namespace) (Policy, bool) {
	d, ok := policies[ns]
	if !ok {
		return Policy{}, false
	}
	return Policy{Namespace: ns, Scope: d.scope, JobType: d.jobType}, true
}

// Spec is a job's dedup identity together with everything its namespace
// determines. Read it; you cannot write it.
type Spec struct {
	namespace Namespace
	key       string
	scope     Scope
	jobType   string
}

// Namespace is the family this job belongs to.
func (s *Spec) Namespace() Namespace { return s.namespace }

// Key names the subject or the event inside that family.
func (s *Spec) Key() string { return s.key }

// Scope is how long this identity is exclusive.
func (s *Spec) Scope() Scope { return s.scope }

// JobType is the type a row with this identity carries.
func (s *Spec) JobType() string { return s.jobType }

// New rebuilds a Spec from stored values. It is for the row scanner; producers
// use the constructors below, which cannot get anything wrong.
//
// The scope is not taken on trust: it is checked against the namespace's policy
// rather than copied, so a row that somehow carries the wrong one is a read
// error and not a job that quietly means something else.
func New(ns Namespace, key string, scope Scope) (*Spec, error) {
	d, ok := policies[ns]
	if !ok {
		return nil, fmt.Errorf("jobdedup: unknown namespace %q", ns)
	}
	if key == "" {
		return nil, fmt.Errorf("jobdedup: namespace %q has an empty key", ns)
	}
	if scope != d.scope {
		return nil, fmt.Errorf("jobdedup: namespace %q is %s, not %s", ns, d.scope, scope)
	}
	return &Spec{namespace: ns, key: key, scope: d.scope, jobType: d.jobType}, nil
}

// Validate refuses a Spec that is missing or that names a family this build
// does not know. A Spec built here cannot be inconsistent; this is the check on
// its existence.
func (s *Spec) Validate() error {
	if s == nil {
		return fmt.Errorf("jobdedup: spec is nil")
	}
	if _, ok := policies[s.namespace]; !ok {
		return fmt.Errorf("jobdedup: unknown namespace %q", s.namespace)
	}
	if s.key == "" {
		return fmt.Errorf("jobdedup: namespace %q has an empty key", s.namespace)
	}
	return nil
}

func mustSpec(ns Namespace, key string) *Spec {
	d, ok := policies[ns]
	if !ok {
		// Unreachable: every constructor below names a namespace declared
		// above, and both are in this file.
		panic("jobdedup: no policy for " + string(ns))
	}
	return &Spec{namespace: ns, key: key, scope: d.scope, jobType: d.jobType}
}

// AckUpdate identifies the message update that follows an acknowledgement.
//
// The prefix is one-to-one with the alert group ID and predates the model. It
// stays: the namespace is what says which family this is, so the prefix says
// nothing twice - but rewriting it would give one identity two spellings under
// one name, for as long as any job written with the old one is still running.
func AckUpdate(alertGroupID string) *Spec {
	return mustSpec(NamespaceAckUpdate, "update_ack_"+alertGroupID)
}

// AlertUpdate identifies the message update that follows a new alert.
func AlertUpdate(alertGroupID string) *Spec {
	return mustSpec(NamespaceAlertUpdate, "update_alert_"+alertGroupID)
}

// Resolution identifies the message update that follows a resolve.
func Resolution(alertGroupID string) *Spec {
	return mustSpec(NamespaceResolution, "resolve_"+alertGroupID)
}
