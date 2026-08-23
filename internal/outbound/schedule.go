package outbound

import "sort"

// How the free slots of one tick are divided.
//
// The naive version - one queue, oldest first - has two failures that are only
// visible under load. A provider that is down accumulates retries, and those
// retries sort ahead of everything, so a healthy provider waits behind an
// unhealthy one. And a steady stream of freshly admitted pages, each of them
// due immediately, keeps arriving ahead of an old retry that has been waiting
// for minutes: strict priority for new work means that retry is never sent at
// all, which is not a latency question but a broken promise.
//
// So: slots are shared max-min across providers by the demand each can actually
// give up, and inside a provider the two queues split, with the older one
// guaranteed a share. Both rules are pure functions of numbers, and both are
// wrong in ways that only appear at the edges - which is why they are here,
// testable, rather than woven into the loop.

// shareSlots divides free slots among providers by water filling: everybody
// gets an equal share of what is left, whoever cannot use all of theirs gives
// the rest back, and the remainder is shared again among those still asking.
//
// Demand is what a claim could ACTUALLY take, not what is due: a row already
// leased by another instance is demand on paper, and allocating a slot to it
// would leave this instance working at half capacity while its own backlog
// waits.
//
// The rotation is for the smallest case. With one free slot and two providers
// asking, somebody has to lose, and without rotating the starting point the
// loser would always be the same one - starvation produced by alphabetical
// order.
func shareSlots(free int, demand map[string]int, tick uint64) map[string]int {
	if free <= 0 || len(demand) == 0 {
		return nil
	}

	candidates := make([]string, 0, len(demand))
	for provider, want := range demand {
		if want > 0 {
			candidates = append(candidates, provider)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Strings(candidates)
	if offset := int(tick % uint64(len(candidates))); offset > 0 {
		candidates = append(candidates[offset:], candidates[:offset]...)
	}

	shares := make(map[string]int, len(candidates))
	remaining := free
	for remaining > 0 && len(candidates) > 0 {
		// At least one, or a pool smaller than the number of providers would
		// hand out nothing at all and the tick would idle with work waiting.
		each := remaining / len(candidates)
		if each < 1 {
			each = 1
		}

		// Filtered in place: every candidate still asking survives into the
		// next round, and the round always gives at least one slot away, so
		// this terminates on `remaining` alone.
		still := candidates[:0]
		for _, provider := range candidates {
			if remaining == 0 {
				break
			}
			give := each
			if unmet := demand[provider] - shares[provider]; give > unmet {
				give = unmet
			}
			if give > remaining {
				give = remaining
			}
			shares[provider] += give
			remaining -= give
			if shares[provider] < demand[provider] {
				still = append(still, provider)
			}
		}
		candidates = still
	}
	return shares
}

// splitPhases divides one provider's slots between the commitments nobody has
// tried yet and everything else.
//
// Three to one in favour of first attempts, because a page that has just been
// raised is the one somebody is waiting for - but never all of them. The last
// slot belongs to the older queue, and at a single slot the two alternate from
// tick to tick, because a guarantee that evaporates exactly when the pool is
// tight is not a guarantee.
//
// The retry share is claimed as "anything due", not as "retries only": the
// store hands out the oldest due work first, which is what makes the share
// reach the commitments that have been waiting longest instead of whatever
// happens to be at hand.
func splitPhases(limit, fresh, retries int, tick uint64) (first, any int) {
	switch {
	case limit <= 0:
		return 0, 0
	case fresh <= 0:
		return 0, limit
	case retries <= 0:
		return limit, 0
	case limit == 1:
		if tick%2 == 0 {
			return 1, 0
		}
		return 0, 1
	}

	first = limit * 3 / 4
	if first < 1 {
		first = 1
	}
	if first > fresh {
		first = fresh
	}
	if first > limit-1 {
		first = limit - 1
	}
	return first, limit - first
}
