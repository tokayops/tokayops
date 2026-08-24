package dispatcher

import (
	"log"

	"github.com/tokayops/tokayops/internal/model"
)

// A gate is the state in the store that says an alert group is owed a job, and
// lowering it is how a producer says the work is now someone's.
//
// Three families produce jobs for an alert group, and each has its own gate: a
// timestamp for the acknowledgement, the group's own status for the resolution,
// a flag with a version for the message update. What they do NOT have is three
// different shapes - each reads what is waiting, builds a job, offers it, and
// lowers the gate. That shape lives here once, and the two answers a family
// gives differently are fields below, where they can be read side by side
// instead of inferred from three loops that happen to differ.
type jobGate struct {
	// family names the gate in the log, in the words the family is known by.
	family string

	// down lowers the gate for the state of the group the producer read, and
	// reports whether it was still in that state.
	//
	// It is a question rather than a command because for one family the answer
	// can be no: the alert update's gate goes back up whenever an alert
	// arrives, and lowering the one the producer read is not the same as
	// lowering the one that is up now.
	down func(ag *model.AlertGroup) (bool, error)

	// lowerOnDuplicate says whether "a job with this identity is already in
	// flight" counts as this group's work being admitted.
	//
	// True where the event happens once in the life of a group - an
	// acknowledgement, a resolution. The job in flight can then only be this
	// producer's own earlier attempt, and it carries the very work the gate is
	// up for.
	//
	// False where the event repeats. A new alert changes the card, and an
	// update built before that alert arrived is not the update this alert is
	// owed; lowering the gate for it is how an alert used to vanish from a
	// message.
	lowerOnDuplicate bool
}

// ackUpdateGate is `ack_processed_at`: raised by an acknowledgement, lowered
// once the update that follows it has been admitted.
//
// Unconditional, and that is a statement about the family: a group is
// acknowledged once - the transition applies only from processing or triggered
// - so there is no second acknowledgement whose update could be lost by
// lowering the gate for the first.
func (d *Dispatcher) ackUpdateGate() jobGate {
	return jobGate{
		family: "ack update",
		down: func(ag *model.AlertGroup) (bool, error) {
			return true, d.store.MarkAckProcessed(ag.ID)
		},
		lowerOnDuplicate: true,
	}
}

// resolutionGate is the group's own status: resolved means the message still
// has to be told, closed means it has been.
//
// Lowering it is a transition out of `resolved` and not an assignment of
// `closed`: a group that left that state meanwhile is not this producer's to
// move, and a setter that could close a group from any state at all is how one
// mistaken ID silently ends an incident nobody has finished.
func (d *Dispatcher) resolutionGate() jobGate {
	return jobGate{
		family: "resolution",
		down: func(ag *model.AlertGroup) (bool, error) {
			return d.store.TransitionAlertGroupStatus(ag.ID,
				model.AlertGroupStatusResolved, model.AlertGroupStatusClosed)
		},
		lowerOnDuplicate: true,
	}
}

// alertUpdateGate is `slack_update_pending`, and it is the one gate that
// carries a version, because it is the one whose event repeats: every alert
// that changes the group raises it again.
//
// The version it carries is the group's render source version, which moves for
// any change to what a message would say - not only for the alerts that raise
// this flag. So a group acknowledged while the update job was being built also
// fails to clear, and the update is built once more. That is the safe direction:
// the card is redrawn from state that has since changed.
func (d *Dispatcher) alertUpdateGate() jobGate {
	return jobGate{
		family: "alert update",
		down: func(ag *model.AlertGroup) (bool, error) {
			return d.store.ClearSlackUpdate(ag.ID, ag.RenderSourceVersion)
		},
		lowerOnDuplicate: false,
	}
}

// offer hands a built job to the engine and lowers the gate if this family's
// rule says the group's work is now someone's.
//
// Anything else leaves the gate up, which is the only retry there is: these
// producers are ticks over a queue, and a group whose gate is still up comes
// back on the next one.
func (d *Dispatcher) offer(g jobGate, ag *model.AlertGroup, job *model.Job,
	stages []*model.JobStage, steps []*model.JobStep) {

	created, err := d.store.CreateJobWithDedup(job, stages, steps)
	if err != nil {
		log.Printf("JobController: %s: failed to create the job for %s (will retry): %v",
			g.family, ag.ID, err)
		return
	}
	if !created && !g.lowerOnDuplicate {
		log.Printf("JobController: %s: %s already has one in flight, keeping the gate up",
			g.family, ag.ID)
		return
	}
	if created {
		log.Printf("JobController: %s: job created for %s", g.family, ag.ID)
	}
	g.lower(ag)
}

// lower puts the gate down for the state the producer read and says what
// happened.
//
// Being overtaken is not a failure and is not logged as one: it means the group
// changed while the job was being created, and the next tick will look at it
// again. Calling it an error here would teach the reader to skip the line that
// matters.
func (g jobGate) lower(ag *model.AlertGroup) {
	lowered, err := g.down(ag)
	if err != nil {
		log.Printf("JobController: %s: failed to lower the gate for %s: %v", g.family, ag.ID, err)
		return
	}
	if !lowered {
		log.Printf("JobController: %s: %s changed while its job was created, leaving the gate up",
			g.family, ag.ID)
	}
}
