package outbound

import (
	"fmt"

	"github.com/tokayops/tokayops/internal/metrics"
)

// Families is every execution partition this build runs, in a fixed order.
//
// It is the one list the metrics snapshot and the counters below zero-fill
// from: a family that exists in the code and not here would have workers,
// claims and a queue, and no series for any alert to be written against.
func Families() []string {
	return []string{FamilyNotification, FamilyHandoff, FamilyWebhook}
}

// LaneDefault is the lane every family's worker runs on unless it is given
// another, and LanePhone the paging family's second one, for calls.
const (
	LaneDefault = "default"
	LanePhone   = "phone"
)

// Lane is one worker of one family.
type Lane struct {
	Family string
	Lane   string
}

// Lanes is every worker this build runs, for the liveness counter's zero
// series. A family's second worker is a lane of its own: counted together, a
// healthy worker would hide one that stopped.
func Lanes() []Lane {
	return []Lane{
		{FamilyNotification, LaneDefault},
		{FamilyNotification, LanePhone},
		{FamilyHandoff, LaneDefault},
		{FamilyWebhook, LaneDefault},
	}
}

// lanePool is the pool of one lane: the family's own for its default lane, the
// lane's for the others. A lane that is not in Lanes is refused - its ticks
// would have no zero series for the liveness rule to read.
func lanePool(family, lane string, policy Policy) (int, error) {
	for _, known := range Lanes() {
		if known.Family != family || known.Lane != lane {
			continue
		}
		if lane == LanePhone {
			return PhonePoolSize, nil
		}
		return policy.PoolSize, nil
	}
	return 0, fmt.Errorf("outbound: %s is not a lane of family %s", lane, family)
}

// Statuses is every status a commitment can be in, for the doors that take a
// status from a caller and have to refuse one this build does not know.
func Statuses() []Status {
	return []Status{StatusPending, StatusSending, StatusIdle, StatusManualReview,
		StatusAwaitingReceipt,
		StatusSucceeded, StatusPermanentFailed, StatusExpired, StatusCanceled, StatusNoContact}
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
	for _, lane := range Lanes() {
		metrics.OutboundWorkerTicksTotal.WithLabelValues(lane.Family, lane.Lane)
	}
	for _, family := range Families() {
		for _, to := range RecoveryTargets() {
			metrics.OutboundLeasesExpiredTotal.WithLabelValues(family, string(to))
		}
	}
	for _, table := range RetentionTables() {
		metrics.OutboundRetentionDeletedTotal.WithLabelValues(table)
	}
}
