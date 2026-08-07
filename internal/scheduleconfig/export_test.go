package scheduleconfig

import "github.com/tokayops/tokayops/internal/rotation"

// WithPlanner replaces the transition planner.
//
// It exists so a test can hand the commit guard a plan that contradicts the
// snapshot it carries, which is the only way to make the guard fire: every
// plan the real planner produces satisfies it by construction. A guard no test
// can trip is a comment rather than a guard, so the seam is deliberate - and it
// lives in a _test file, so no production caller can reach it.
func WithPlanner(plan func(rotation.TransitionInput) (rotation.TransitionPlan, error)) Option {
	return func(s *Service) { s.planTransition = plan }
}
