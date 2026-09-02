package model

// MetricsSnapshot is the read model the Prometheus collector asks for: every
// count one scrape reports, gathered in one pass.
//
// It lives here and not in the store because both sides name it - the store
// fills it in, the collector reads it - and a type owned by one of them would
// make the other depend on a package it has no other business with.
type MetricsSnapshot struct {
	ActiveAlertGroups        []AlertGroupCount
	AlertGroupsByStatus      []AlertGroupStatusCount
	TeamsWithoutOnCall       int
	TeamsWithPermanentOnCall int
	TeamsWithoutPolicy       int
	OutboxEventsByStatus     []StatusCount

	// OutboundIntentsByStatus is what each delivery family currently owes and
	// what it has finished with.
	OutboundIntentsByStatus []OutboundStatusCount

	// OutboundLatenessSeconds is how far behind each family is: the age of the
	// oldest commitment that is due NOW and has not started, measured by the
	// database in the same statement that found it.
	//
	// A family with nothing overdue reports zero rather than being left out.
	// A gauge nobody touches keeps its last value forever, and a backlog that
	// has been worked off would go on ringing.
	OutboundLatenessSeconds []OutboundLateness

	// OutboundCardsBehind is how many editable messages are showing something
	// older than the alert they are about, grouped by whether anything is going
	// to fix that.
	OutboundCardsBehind []OutboundCardsBehind

	// OutboundCardStalenessSeconds is how long the oldest of them has been
	// behind, counted only over the ones somebody or something is still going
	// to catch up.
	OutboundCardStalenessSeconds float64

	// OutboundNoTargetsAdmissions is how many admission claims of each family
	// were accepted and promised nobody, counted from the claim rows
	// themselves. Claims are never deleted, so the number only grows, and it
	// is the durable form of "an alert had nobody to page" - the one the alert
	// rule reads, because the process counter beside it can miss an increment
	// when the process dies between commit and Inc. Every family reports, zero
	// included, so increase() has a base from the first scrape.
	OutboundNoTargetsAdmissions []OutboundFamilyCount
}

// OutboundFamilyCount is one delivery family's count of something.
type OutboundFamilyCount struct {
	Family string
	Count  int
}

// OutboundCardsBehind is one state's count of messages behind their alert.
//
// The state is not the commitment's status: it is what the status means for
// this question. "queued" will be caught up by a worker; "stuck" needs a
// person; "abandoned" is a person having decided not to catch it up, which is
// a normal end and not a fault.
type OutboundCardsBehind struct {
	State string
	Count int
}

// OutboundStatusCount is one delivery family's count in one status.
type OutboundStatusCount struct {
	Family string
	Status string
	Count  int
}

// OutboundLateness is one delivery family's worst overdue commitment, in
// seconds. Leased rows are included on purpose: a commitment claimed by a
// worker that then hung is exactly what a health signal must not hide.
type OutboundLateness struct {
	Family  string
	Seconds float64
}

// AlertGroupCount holds the count of active alert groups for a team/severity pair.
type AlertGroupCount struct {
	TeamID   string
	Severity string
	Count    int
}

// AlertGroupStatusCount holds the count of alert groups for a team/severity/status triple.
type AlertGroupStatusCount struct {
	TeamID   string
	Severity string
	Status   string
	Count    int
}

// StatusCount holds a count for a single status value.
type StatusCount struct {
	Status string
	Count  int
}
