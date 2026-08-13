package schedulerender

import "errors"

// Failures a read path can hit in the data of one schedule.
//
// They are sentinels rather than prose because the runtime has to classify
// them: the bulk projection turns a recognized sentinel into a per-schedule
// failure and anything else into a failure of the whole call. Matching on error
// text instead would put that classification in every consumer, and the copies
// would drift.
var (
	// ErrRevisionGap means no revision is in force at an instant the chain was
	// supposed to cover. A deleted period is itself a revision, so this is
	// never a normal state - it is a lost row.
	ErrRevisionGap = errors.New("schedulerender: no revision in force")

	// ErrRotation means the rotation math refused the stored configuration:
	// an unloadable timezone, a policy the grid rejects, a position that
	// cannot be computed.
	ErrRotation = errors.New("schedulerender: rotation error")
)
