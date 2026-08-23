package outbound

import "testing"

// The deadlines are not independent numbers. Each one bounds a step of the same
// piece of work, and the relations between them are what keep a commitment from
// being abandoned mid-flight - which is why they are asserted here rather than
// left to whoever edits one of them next.
func TestTheDeadlinesFitInsideEachOther(t *testing.T) {
	chain := NotificationPrepareDeadline + NotificationRecordDeadline +
		NotificationAttemptDeadline + NotificationRecordDeadline

	if NotificationShutdownDeadline < chain {
		t.Errorf("a stopping worker waits %s for work that may take %s; the difference "+
			"is calls whose answers are thrown away", NotificationShutdownDeadline, chain)
	}
	if NotificationAttemptDeadline >= NotificationLease {
		t.Errorf("an attempt may run %s under a lease of %s, so it can still be running "+
			"when somebody else is told to redo it",
			NotificationAttemptDeadline, NotificationLease)
	}
	if NotificationLockTimeout >= NotificationLease {
		t.Errorf("a mutation may wait %s for a row under a lease of %s, and would then "+
			"apply a decision that has been reassigned",
			NotificationLockTimeout, NotificationLease)
	}
	if NotificationRecordDeadline <= NotificationLockTimeout {
		t.Errorf("recording gives up after %s while the store waits %s for a contended "+
			"row, so the refusal would come from the context rather than the rule",
			NotificationRecordDeadline, NotificationLockTimeout)
	}
}
