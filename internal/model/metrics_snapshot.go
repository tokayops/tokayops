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
	OutboxDeliveriesByStatus []StatusCount

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
