/**
 * What every part of the schedule UI agrees on.
 *
 * Values and transformations only. Nothing here opens a modal, calls the API
 * or remembers anything between calls: the moment a shared module holds state
 * or drives a flow, it becomes the place the whole feature quietly reassembles
 * itself in.
 *
 * Two things live here because three different screens have to say the same
 * thing about them: what state a team's schedule is in, and what went wrong
 * with an override.
 */

import { showToast } from '/js/core/utils.js';

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

// ========================================
// Errors
// ========================================

/**
 * React to a schedule API error by what it is, not by its status.
 *
 * The server names every failure; a client that matched on prose would break
 * the first time a message is reworded, and one that matched only on 409 would
 * have to guess between six different recoveries.
 *
 * Handlers may be async; the promise is returned so a caller can wait on the
 * recovery, and a rejection inside one is reported rather than becoming an
 * unhandled rejection.
 *
 * @param {Error} error - carries status, code and body
 * @param {Object} handlers - code -> handler
 * @returns {Promise<void>}
 */
export async function onScheduleError(error, handlers = {}) {
    const handler = handlers[error?.code];
    if (!handler) {
        // An unrecognised code still has a server message. Inventing an
        // interpretation for it would be worse than passing it through.
        showToast(error?.message || 'Request failed', 'error');
        return;
    }
    try {
        await handler(error);
    } catch (failure) {
        console.error('Failed to recover from a schedule error:', failure);
        showToast(error?.message || 'Request failed', 'error');
    }
}

function conflictingOverridesText(error) {
    const conflicts = error?.body?.conflicting_overrides || [];
    if (conflicts.length === 0) return 'It overlaps an existing override.';
    const parts = conflicts.map(c => {
        const from = new Date(c.valid_from).toLocaleString(undefined, {
            month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
        });
        const to = new Date(c.valid_to).toLocaleString(undefined, {
            month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
        });
        return `${from} → ${to}`;
    });
    return `It overlaps: ${parts.join('; ')}.`;
}

/**
 * Say what went wrong with an override.
 *
 * An override can be reached from three places - the list inside its modal,
 * the calendar and the on-call row - and they say the same things about the
 * same codes. What each does next is its own business: `onStale` is called
 * when what the caller is showing is known to be behind, and the caller
 * decides what "reload" means for the screen it owns.
 *
 * @param {Error} error
 * @param {Object} [options]
 * @param {() => Promise<void>|void} [options.onStale]
 */
export function reportOverrideError(error, { onStale } = {}) {
    const stale = async (message) => {
        showToast(message, 'error');
        await onStale?.();
    };

    return onScheduleError(error, {
        override_overlap: (err) => showToast(
            `This override was not saved. ${conflictingOverridesText(err)}`, 'error'),
        override_revision_conflict: () => stale(
            'Someone else changed this override. Reloading the list.'),
        override_not_found: () => stale('This override no longer exists.'),
        schedule_deleted: () => showToast(
            'This schedule was deleted, so it has no overrides to change.', 'error'),
        user_not_team_member: (err) => {
            const ids = err.body?.user_ids || [];
            showToast(`Not a member of this team: ${ids.join(', ')}`, 'error');
        },
    });
}
