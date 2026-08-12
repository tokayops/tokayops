/**
 * Who is on duty, as the page shows it.
 *
 * This module owns the widgets that sit in a team page and in the on-call
 * list, and nothing else: it reads the current state of a team's schedule and
 * renders it. Opening an editor, a calendar or an override modal is somebody
 * else's job, reached through the delegated dispatcher rather than from here.
 */

import { State } from '/js/core/state.js';
import { resolveNames } from '/js/core/users-directory.js';
import { scheduleContext, unavailableContext } from '/js/modules/schedule-shared.js';
import { onCallOverviewRow, onCallWidget } from '/js/modules/schedule-components.js';

/**
 * Everything the widgets need about a team's schedule, in one request.
 *
 * The on-call endpoint reports whether a schedule exists and whether it was
 * deleted alongside the projection, so this does not also have to ask for the
 * configuration - a request whose ordinary answer is 404, once per team on
 * every page that lists them.
 */
async function loadTeamOnCall(teamId, { resolveOwnNames = true } = {}) {
    // Not caught here. The endpoint answers 200 for a team with no schedule,
    // so anything thrown is a real failure - and turning it into an empty
    // projection would render "not configured" over a database that is down,
    // which is the one thing the server side of this refuses to do.
    const response = await API.schedules.currentOnCall(teamId);

    const onCall = response?.on_call || null;
    // Skipped when the caller is resolving for several teams at once: doing it
    // here as well would be the per-row lookup this exists to avoid.
    const names = resolveOwnNames
        ? await resolveNames([
            ...(onCall?.l1?.user_ids || []),
            ...(onCall?.l2?.user_ids || []),
        ])
        : new Map();

    // The schedule id and the deletion date are what the context is derived
    // from, and they are not derivable from the projection. A team with no
    // schedule, a deleted one and a live one between shifts all put nobody on
    // duty: the calendar is worth offering for the last two, an override only
    // for the last, and "Deleted" is not "Not configured".
    return {
        onCall,
        scheduleId: response?.schedule_id || '',
        deletedAt: response?.deleted_at || null,
        names,
    };
}

/**
 * Load and render on-call widget for a team section
 * @param {string} teamId - Team ID
 * @param {HTMLElement} container - Container element
 */
export async function loadOnCallWidget(teamId, container) {
    if (!container) return;

    try {
        const state = await loadTeamOnCall(teamId);
        container.innerHTML = onCallWidget(state.onCall, scheduleContext(teamId, state));
    } catch (error) {
        console.error('Failed to load on-call widget:', error);
        container.innerHTML = onCallWidget(null, unavailableContext(teamId));
    }
    if (window.lucide) lucide.createIcons();
}

/**
 * Redraw every widget that speaks for a team, after something changed it.
 */
export async function refreshOnCallUI(teamId) {
    if (!teamId) return;
    try {
        const state = await loadTeamOnCall(teamId);

        const safeId = typeof CSS !== 'undefined' && CSS.escape ? CSS.escape(teamId) : teamId;
        const widgets = document.querySelectorAll(`.oncall-row[data-team-id="${safeId}"], .on-call-widget[data-team-id="${safeId}"]`);
        const team = State.teams.find(t => t.id === teamId) || { id: teamId, name: teamId };
        const ctx = scheduleContext(teamId, state);

        if (widgets.length > 0) {
            widgets.forEach(widget => {
                const isOverview = widget.classList.contains('oncall-row')
                    || widget.closest('.on-call-overview');
                widget.outerHTML = isOverview
                    ? onCallOverviewRow(state.onCall, team, ctx)
                    : onCallWidget(state.onCall, ctx);
            });
        } else {
            const rowSlot = document.querySelector(`.oncall-row-slot[data-team-id="${safeId}"]`);
            if (rowSlot) {
                rowSlot.innerHTML = onCallOverviewRow(state.onCall, team, ctx);
            }
        }

        if (window.lucide) lucide.createIcons();
    } catch (error) {
        console.error('Failed to refresh on-call UI:', error);
    }
}

/**
 * Load and render compact on-call overview row for a team
 * @param {Object} team - Team object with id/name
 * @param {HTMLElement} container - Container element
 */
export async function loadOnCallOverviewRow(team, container) {
    if (!container || !team?.id) return;

    try {
        const state = await loadTeamOnCall(team.id);
        container.innerHTML = onCallOverviewRow(
            state.onCall, team, scheduleContext(team.id, state));
    } catch (error) {
        console.error('Failed to load on-call overview row:', error);
        container.innerHTML = onCallOverviewRow(null, team, unavailableContext(team.id));
    }
    if (window.lucide) lucide.createIcons();
}

/**
 * Render the on-call row for every team, resolving names once.
 *
 * Drawing the rows one at a time means each asks after its own two or three
 * people, so a page listing twenty teams made twenty lookups. Loading them
 * together lets one lookup cover the lot.
 *
 * The loads are settled rather than joined: a team whose state cannot be read
 * still gets a row saying so, and does not take the other nineteen down with
 * it. That is what the per-row version already did, and a batched version has
 * no business being less robust than the loop it replaces.
 *
 * @param {Array<{id, name}>} teams
 * @param {(teamId: string) => HTMLElement|null} containerFor
 */
export async function loadOnCallOverviewRows(teams, containerFor) {
    const loaded = await Promise.allSettled(
        (teams || []).map(team => loadTeamOnCall(team.id, { resolveOwnNames: false })));

    // Names for everyone the successful rows put on duty, in one pass. The
    // directory chunks it if the page is large enough to need chunking.
    const ids = [];
    for (const result of loaded) {
        if (result.status !== 'fulfilled') continue;
        ids.push(...(result.value.onCall?.l1?.user_ids || []));
        ids.push(...(result.value.onCall?.l2?.user_ids || []));
    }
    const names = await resolveNames(ids);

    teams.forEach((team, i) => {
        const container = containerFor(team.id);
        if (!container) return;

        const result = loaded[i];
        if (result.status !== 'fulfilled') {
            console.error('Failed to load on-call overview row:', result.reason);
            container.innerHTML = onCallOverviewRow(null, team, unavailableContext(team.id));
            return;
        }
        const state = result.value;
        container.innerHTML = onCallOverviewRow(
            state.onCall, team, { ...scheduleContext(team.id, state), names });
    });

    if (window.lucide) lucide.createIcons();
}
