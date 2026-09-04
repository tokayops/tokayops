package outbound

import "github.com/tokayops/tokayops/internal/metrics"

// Families is every execution partition this build runs, in a fixed order.
//
// It is the one list the metrics snapshot and the counters below zero-fill
// from: a family that exists in the code and not here would have workers,
// claims and a queue, and no series for any alert to be written against.
func Families() []string {
	return []string{FamilyNotification, FamilyHandoff, FamilyWebhook}
}

// Statuses is every status a commitment can be in, for the doors that take a
// status from a caller and have to refuse one this build does not know.
func Statuses() []Status {
	return []Status{StatusPending, StatusSending, StatusIdle, StatusManualReview,
		StatusSucceeded, StatusPermanentFailed, StatusExpired, StatusCanceled}
}

// RecoveryTargets is every status recovery can move a commitment to when its
// worker's lease ran out with an attempt open - the "to" label of
// outbound_leases_expired_total. It is the closed set of T7's answers in the
// machine: retry goes back to pending, manual review waits for a person, a
// withdrawn send is canceled, an overdue one expired, and assume_accepted
// settles it as succeeded.
func RecoveryTargets() []Status {
	return []Status{StatusPending, StatusManualReview, StatusCanceled, StatusExpired, StatusSucceeded}
}

// The liveness counters exist from the moment the binary starts, at zero, for
// every family and every recovery target.
//
// A CounterVec exports nothing until somebody asks for a label set, and the
// worker that never started never asks. rate(outbound_worker_ticks_total) == 0
// then has no input series to be zero about, and the rule written for exactly
// that failure stays silent. Initialising here, in the package that owns the
// closed list of families, is what makes the rule's input exist independently
// of whether cmd/tokayops built the worker.
func init() {
	for _, family := range Families() {
		metrics.OutboundWorkerTicksTotal.WithLabelValues(family)
		for _, to := range RecoveryTargets() {
			metrics.OutboundLeasesExpiredTotal.WithLabelValues(family, string(to))
		}
	}
	for _, table := range RetentionTables() {
		metrics.OutboundRetentionDeletedTotal.WithLabelValues(table)
	}
}
