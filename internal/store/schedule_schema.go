package store

// RevisionOverlapConstraintPresent reports whether the exclusion constraint
// that forbids two revisions of one schedule covering the same instant exists.
//
// It is asked at startup and REPORTED, not enforced, and the difference is a
// deliberate contract rather than a compromise. The constraint is defence in
// depth: non-overlap is guaranteed by the schedule row lock inside a single
// transaction, which is what the write path relies on and what its tests pin.
// The constraint needs btree_gist, which needs privileges an installation may
// not grant, and refusing to start there would take a working deployment down
// for the loss of a second line of defence rather than the first.
//
// What it must not do is disappear silently. The DDL that creates it swallows a
// missing extension with a RAISE NOTICE, which no operator sees; a database
// without the constraint should be a fact somebody can read in the log.
//
// The lookup names the table and the kind, not just the constraint name.
// Constraint names are unique per table, not per database, so a same-named
// constraint on anything else would answer this question - and it would answer
// it "present", which is the direction that hides the absence this exists to
// report. to_regclass rather than a cast: a cast raises when the table is
// missing, and a missing table is a different failure than a missing backstop.
func (s *Store) RevisionOverlapConstraintPresent() (bool, error) {
	var present bool
	err := s.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conname = $1
			  AND conrelid = to_regclass('schedule_revisions')
			  AND contype = 'x')`,
		"no_overlapping_schedule_revisions").Scan(&present)
	return present, err
}
