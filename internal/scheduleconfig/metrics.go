package scheduleconfig

import "time"

// Metrics is what the service reports about itself.
//
// It is an interface here rather than a direct call into internal/metrics
// because that package imports the store, and the store implements the
// interfaces in this one: importing it would close the loop
// scheduleconfig -> metrics -> store -> scheduleconfig. Every method takes
// standard library types only, so the Prometheus implementation lives in
// internal/metrics without importing this package either.
type Metrics interface {
	// RevisionCreated counts a committed revision by what triggered it.
	RevisionCreated(trigger string)

	// TransitionNoop counts a save that found nothing to change.
	TransitionNoop()

	// TransitionConflict counts a save rejected by optimistic concurrency.
	TransitionConflict()

	// TransitionDuration observes how long one command took, lock wait
	// included: a rising tail here is contention on one schedule.
	TransitionDuration(d time.Duration)

	// SnapshotDecodeError counts a stored snapshot that would not decode.
	// It is data corruption, so it is worth an alert of its own.
	SnapshotDecodeError()

	// GuardViolation counts a commit-time post-condition failure - the
	// rotation the planner promised is not the rotation the new snapshot
	// produces. It should be flat at zero.
	GuardViolation()
}

// NopMetrics is the default: a service that was never told where to report
// still runs. Tests use it too, which is why the fakes never see metrics.
type NopMetrics struct{}

func (NopMetrics) RevisionCreated(string)           {}
func (NopMetrics) TransitionNoop()                  {}
func (NopMetrics) TransitionConflict()              {}
func (NopMetrics) TransitionDuration(time.Duration) {}
func (NopMetrics) SnapshotDecodeError()             {}
func (NopMetrics) GuardViolation()                  {}

var _ Metrics = NopMetrics{}
