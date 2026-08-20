// Package jobdedup declares what makes two background jobs the same work.
//
// A job carries a Spec: the pair (Namespace, Key) names the work, and Scope
// says how long that name is exclusive. The pair is the identity; the scope is
// a policy ABOUT that identity and deliberately not part of it. Were scope part
// of the identity, the same business event under two scopes would be two
// different jobs - which is the defect this package exists to remove, not a
// distinction anyone wants.
//
// Scope is therefore never chosen by a caller. Every constructor below fixes
// the scope its family is entitled to, and Policies() is the single place where
// that mapping is written down: the schema seeds its reference table from it,
// and a composite foreign key holds jobs to it.
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

// Namespace is a family of jobs. It is not job.Type: two families
// (ack updates and alert updates) share the type "update", and the rule that
// tells them apart has to live somewhere other than that column.
type Namespace string

const (
	NamespaceEscalation  Namespace = "escalation"
	NamespaceAckUpdate   Namespace = "ack_update"
	NamespaceAlertUpdate Namespace = "alert_update"
	NamespaceResolution  Namespace = "resolution"
	NamespaceHandoff     Namespace = "handoff"
)

// policies is the whole model of "which family is exclusive for how long".
//
// A namespace never changes its policy. Changing one would mean claiming, in
// hindsight, that every row already written under that name meant something
// else; a family whose policy changes takes a NEW name instead, and both stay
// here - the old one owning its history, the new one owning what follows.
var policies = map[Namespace]Scope{
	NamespaceEscalation:  ScopeForever,
	NamespaceAckUpdate:   ScopeWhileActive,
	NamespaceAlertUpdate: ScopeWhileActive,
	NamespaceResolution:  ScopeWhileActive,
	NamespaceHandoff:     ScopeWhileActive,
}

// Policies returns every known namespace with its scope, ordered by namespace
// so a seed writes the same rows in the same order every time.
func Policies() []Policy {
	out := make([]Policy, 0, len(policies))
	for ns, scope := range policies {
		out = append(out, Policy{Namespace: ns, Scope: scope})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

// Policy is one namespace and the scope it is entitled to.
type Policy struct {
	Namespace Namespace
	Scope     Scope
}

// ScopeOf reports the policy of a namespace, and whether it is known at all.
func ScopeOf(ns Namespace) (Scope, bool) {
	scope, ok := policies[ns]
	return scope, ok
}

// Spec is a job's dedup identity together with the policy that governs it.
type Spec struct {
	Namespace Namespace `json:"namespace"`
	Key       string    `json:"key"`
	Scope     Scope     `json:"scope"`
}

// New rebuilds a Spec from stored values. It is for the row scanner; producers
// use the constructors below, which cannot get the scope wrong.
func New(ns Namespace, key string, scope Scope) (*Spec, error) {
	spec := &Spec{Namespace: ns, Key: key, Scope: scope}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

// Validate checks a Spec against the policy table: an unknown namespace, an
// empty key, or a scope its namespace is not entitled to.
func (s *Spec) Validate() error {
	if s == nil {
		return fmt.Errorf("jobdedup: spec is nil")
	}
	want, ok := policies[s.Namespace]
	if !ok {
		return fmt.Errorf("jobdedup: unknown namespace %q", s.Namespace)
	}
	if s.Key == "" {
		return fmt.Errorf("jobdedup: namespace %q has an empty key", s.Namespace)
	}
	if s.Scope != want {
		return fmt.Errorf("jobdedup: namespace %q is %s, not %s", s.Namespace, want, s.Scope)
	}
	return nil
}

// Escalation identifies the escalation of one alert group.
//
// The key is the alert group's ID and not its dedup key, which names the ALERT.
// Alert groups are recreated under the same alert dedup key after every resolve,
// so a forever claim on that string would silently swallow the escalation of
// every repeat incident - nobody would be paged.
func Escalation(alertGroupID string) *Spec {
	return &Spec{Namespace: NamespaceEscalation, Key: alertGroupID, Scope: ScopeForever}
}

// AckUpdate identifies the message update that follows an acknowledgement.
func AckUpdate(alertGroupID string) *Spec {
	return &Spec{Namespace: NamespaceAckUpdate, Key: "update_ack_" + alertGroupID, Scope: ScopeWhileActive}
}

// AlertUpdate identifies the message update that follows a new alert.
func AlertUpdate(alertGroupID string) *Spec {
	return &Spec{Namespace: NamespaceAlertUpdate, Key: "update_alert_" + alertGroupID, Scope: ScopeWhileActive}
}

// Resolution identifies the message update that follows a resolve.
func Resolution(alertGroupID string) *Spec {
	return &Spec{Namespace: NamespaceResolution, Key: "resolve_" + alertGroupID, Scope: ScopeWhileActive}
}

// Handoff identifies one on-call handover notification.
//
// The prefixes above and this key keep the encoding they had before the model
// existed. Sprint 3 decides each family's identity for real; rewriting the
// strings here would mean reading the history two different ways in two
// releases.
func Handoff(occurrenceKey string) *Spec {
	return &Spec{Namespace: NamespaceHandoff, Key: occurrenceKey, Scope: ScopeWhileActive}
}
