/**
 * What the schedule UI says when the server refuses something.
 *
 * This is policy, not transformation: it reads a machine code, decides what a
 * person should be told, and says it. It lives apart from the values in
 * `schedule-shared.js` for exactly that reason - that file is meant to hold
 * things with no opinion about the screen, and mapping a failure to a toast is
 * nothing but an opinion about the screen.
 *
 * It is shared because an override can be reached from three places and they
 * owe the same explanation for the same code. What each does afterwards is
 * their own: recovery arrives as a callback, because "reload the list" means a
 * different thing to a modal, a calendar and a row in a table.
 *
 * It calls no API and remembers nothing between calls.
 */

import { showToast } from '/js/core/utils.js';

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
 * @param {Error} error
 * @param {Object} [options]
 * @param {() => Promise<void>|void} [options.onStale] - called when what the
 *        caller is showing is known to be behind; the caller decides what
 *        reloading means for the screen it owns
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
