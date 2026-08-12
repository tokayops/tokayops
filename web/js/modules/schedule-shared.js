/**
 * What state a team's schedule is in.
 *
 * Values and transformations, and nothing else: this module imports nothing,
 * touches no document, calls no API and remembers nothing between calls. The
 * moment a shared module holds state or drives a flow, it becomes the place
 * the whole feature quietly reassembles itself in - so what belongs here is
 * only what every screen has to agree on, in a form none of them can bend.
 *
 * What the UI says when something fails is a different kind of shared, and
 * lives in `schedule-errors.js`: mapping a code to a toast is an opinion about
 * the screen, and this file is meant not to have any.
 */

// ========================================
// What state a schedule is in
// ========================================

/**
 * The four states, as one closed set.
 *
 * They were four booleans once - `exists`, `active`, `deletedAt`,
 * `unavailable` - which let `active && deleted` and `unavailable && exists` be
 * written down, and made three separate places re-derive the same answer from
 * them in the same order by agreement rather than by construction. A team with
 * no schedule, a deleted one, a live one and one we could not read about are
 * four different things to say to whoever is reading the screen, and this is
 * the only place that decides which of them is true.
 */
const SCHEDULE_KINDS = Object.freeze(['active', 'deleted', 'absent', 'unavailable']);

/**
 * The kind, or an exception.
 *
 * A fifth state added later has to be handled everywhere it matters, and the
 * way to guarantee that is for the unhandled case to be loud. Rendering
 * "not configured" over a state nobody taught the UI about is the failure this
 * exists to prevent.
 */
export function assertScheduleKind(kind) {
    if (!SCHEDULE_KINDS.includes(kind)) {
        throw new Error(`Unknown schedule kind: ${String(kind)}`);
    }
    return kind;
}

/** Is there a schedule at all - deleted counts, because it can be recreated. */
export function scheduleExists(kind) {
    switch (assertScheduleKind(kind)) {
        case 'active':
        case 'deleted':
            return true;
        case 'absent':
        case 'unavailable':
            return false;
    }
}

/** Is there a schedule to act on - overrides need one that is running. */
export function scheduleActive(kind) {
    switch (assertScheduleKind(kind)) {
        case 'active':
            return true;
        case 'deleted':
        case 'absent':
        case 'unavailable':
            return false;
    }
}

/**
 * The widget context for a team whose schedule state was read.
 *
 * `exists` and `active` are not stored: they are questions about the kind, and
 * storing the answers is what allowed them to disagree with it.
 *
 * @param {string} teamId
 * @param {{scheduleId: string, deletedAt: ?string, names: Map}} state
 */
export function scheduleContext(teamId, state = {}) {
    const scheduleId = state.scheduleId || '';
    const deletedAt = state.deletedAt || null;

    let kind = 'active';
    if (!scheduleId) kind = 'absent';
    else if (deletedAt) kind = 'deleted';

    return {
        teamId,
        kind,
        scheduleId,
        // Kept as a date rather than as a flag: the kind says the schedule was
        // deleted, this says when, and only the banner needs the second.
        deletedAt,
        names: state.names || new Map(),
    };
}

/** The widget context for a team whose state could not be read at all. */
export function unavailableContext(teamId) {
    return { teamId, kind: 'unavailable', scheduleId: '', deletedAt: null, names: new Map() };
}
