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
