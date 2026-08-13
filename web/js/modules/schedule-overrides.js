/**
 * Overrides: the modal that creates and edits them, and the removal that can
 * be asked for from anywhere they are shown.
 *
 * Everything one open of the modal knows - which override is being edited, at
 * which revision, in which display zone, and which end of an ambiguous hour
 * was chosen - lives in an object created when it opens and reachable only
 * from inside it. It used to live beside the module, which meant it survived
 * the modal: a fold chosen for one override was still chosen for the next, and
 * the fix was a third place to reset it rather than one place that could not
 * leak. A fourth way out would have brought the bug back.
 *
 * The session is created before the first `await`, and every listener is
 * registered with its signal. Both matter for the same reason: closing the
 * modal has to stop the work it started, and a request that was already on its
 * way is work no listener can call back.
 */

import { showToast, escapeHtml, openModal } from '/js/core/utils.js';
import { beginModalSession, modalShell } from '/js/core/modal-session.js';
import { initTimezonePicker } from '/js/core/timezone-picker.js';
import { resolveNames, assignableMembers } from '/js/core/users-directory.js';
import { reportOverrideError } from '/js/modules/schedule-errors.js';
import { overrideModal, overridesList } from '/js/modules/schedule-components.js';
import { resolveLocalTime, gapMessage, instantToLocalInput, foldOf } from '/js/core/zoned-time.js';

/**
 * Open the override manager for a team.
 *
 * @param {string} teamId
 * @param {Object} [options]
 * @param {string} [options.editOverrideId] - open with this override loaded for editing
 * @param {() => Promise<void>|void} [options.returnTo] - where leaving this modal goes;
 *        set when it was opened from the calendar, so every way out returns there
 * @param {(teamId: string) => void} [options.onChanged] - an override was created,
 *        changed or removed
 * @returns {Promise<Object|null>} the session, or null when it could not open
 */
export async function openOverrideModal(teamId, options = {}) {
    // The whole of what this open knows. Nothing outside these braces can
    // reach it, and the next open starts with its own.
    const state = {
        teamId,
        members: [],
        scheduleId: '',
        // The override being edited, or null when the form creates a new one.
        editing: null,
        timezone: null,
        // Which occurrence to take when an entered local time happens twice.
        // The defaults widen the window rather than narrowing it, so an
        // ambiguous hour cannot silently shorten the cover; either can be
        // switched per field.
        fold: { start: 'earlier', end: 'later' },
    };

    // Created before anything is awaited. A modal that closes while its load
    // is in flight leaves that load with nothing to write to - which is not
    // the same as leaving it with somebody else's modal to write to.
    const session = beginModalSession({
        onDismiss: () => {
            // Opened from the calendar: the X, the background and Escape all
            // mean "back to the calendar", not "close everything".
            if (!options.returnTo) return false;
            leave();
            return true;
        },
    });

    /**
     * Leave the modal the way this open is supposed to be left.
     *
     * `closeModal` rather than `close` then `closeModal`: closing the session
     * first would give up the screen, and the shell is only closed by the
     * session that still owns it.
     */
    function leave() {
        if (!options.returnTo) {
            session.closeModal();
            return undefined;
        }
        session.close();
        return options.returnTo();
    }

    try {
        const loaded = await loadOverrideState(teamId, session.signal);
        if (session.closed) return null;

        state.members = loaded.members;
        state.scheduleId = loaded.scheduleId;

        const { title, body, footer } = modalShell();
        title.textContent = 'Manage Overrides';
        body.innerHTML = overrideModal(loaded, teamId);
        footer.innerHTML = `
            <button type="button" class="btn btn-secondary" id="override-cancel">Cancel</button>
            <button type="submit" form="override-form" class="btn btn-primary">Create Override</button>
        `;

        openModal('modal-overlay');
        if (window.lucide) lucide.createIcons();

        const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        initTimezonePicker('override-timezone', {
            selected: browserTz,
            onChange: (newTz) => retimeFields(state, newTz),
        });
        state.timezone = browserTz;

        bindOverrideEvents(session, state, options, leave);

        if (options.editOverrideId) {
            // The calendar draws shifts, not override heads: it knows which
            // override a band came from, but not the revision to edit against.
            // The modal has just read the heads, so the form is filled from
            // what it already has rather than from a second request.
            const head = loaded.overrides.find(o => o.override_id === options.editOverrideId);
            if (!head) {
                showToast('This override no longer exists.', 'error');
                await leave();
                return null;
            }
            populateEditForm(state, head, loaded.scheduleId);
        }

        return session;
    } catch (error) {
        if (session.closed) return null;
        console.error('Failed to open override modal:', error);
        showToast('Failed to load overrides: ' + error.message, 'error');
        // Nothing was drawn, so there is nothing here to keep a session for.
        // Opened from the calendar, that means going back to it: the calendar
        // this replaced is on screen but its session ended when this one
        // began, so what is showing is a picture with no listeners behind it.
        await leave();
        return null;
    }
}

async function loadOverrideState(teamId, signal) {
    // Only a 404 on the config is an answer: this team has no schedule, and
    // an empty form is the honest way to say so. Every other failure - 403,
    // 500, a dropped connection - means we do not KNOW, and turning that into
    // "no schedule, no overrides" makes existing overrides vanish from the
    // screen and opens a form with no schedule_id behind it. Those propagate
    // to the caller, which already has an error path.
    const [members, config, overrideList] = await Promise.all([
        assignableMembers(teamId, { signal }),
        API.schedules.getConfig(teamId, { signal }).catch(e => {
            if (e?.status === 404) return null;
            throw e;
        }),
        API.schedules.listOverrides(teamId, { signal }),
    ]);

    const overrides = overrideList?.overrides || [];
    const names = await resolveNames(overrides.map(o => o.user_id));
    for (const member of members) names.set(member.id, member.name);

    return { members, overrides, names, scheduleId: config?.schedule_id || '' };
}

function bindOverrideEvents(session, state, options, leave) {
    const form = document.getElementById('override-form');
    if (!form) return;

    const bind = (element, type, handler) =>
        element?.addEventListener(type, handler, { signal: session.signal });

    bind(document.getElementById('override-cancel'), 'click', () => {
        resetForm(state);
        leave();
    });

    for (const id of ['override-start', 'override-end']) {
        bind(document.getElementById(id), 'change', () => describeTimes(state));
    }

    bind(document.getElementById('override-time-note'), 'click', (e) => {
        const toggle = e.target.closest('.override-fold-toggle');
        if (!toggle) return;
        e.preventDefault();
        state.fold = { ...state.fold, [toggle.dataset.field]: toggle.dataset.prefer };
        describeTimes(state);
    });

    bind(form, 'submit', (e) => handleSubmit(e, session, state, options, leave));

    // The list of existing overrides belongs to this modal, so its buttons are
    // handled here rather than by the dispatcher that opens modals. The click
    // is stopped on the way up for that reason: outside this body, the same
    // button class means a row in a widget, and that has a different owner.
    bind(modalShell().body, 'click', async (e) => {
        const editBtn = e.target.closest('.edit-override-btn');
        if (editBtn) {
            e.preventDefault();
            e.stopPropagation();
            populateEditForm(state, {
                override_id: editBtn.dataset.overrideId,
                revision: editBtn.dataset.revision,
                user_id: editBtn.dataset.userId,
                valid_from: editBtn.dataset.validFrom,
                valid_to: editBtn.dataset.validTo,
                reason: editBtn.dataset.reason,
            }, editBtn.dataset.scheduleId);
            return;
        }

        const deleteBtn = e.target.closest('.delete-override-btn');
        if (deleteBtn) {
            e.preventDefault();
            e.stopPropagation();

            const removed = await removeOverride({
                teamId: state.teamId,
                scheduleId: deleteBtn.dataset.scheduleId,
                overrideId: deleteBtn.dataset.overrideId,
                revision: parseInt(deleteBtn.dataset.revision, 10),
            }, {
                onStale: () => refreshList(session, state),
            });
            if (!removed || session.closed) return;

            await refreshList(session, state);
            await options.onChanged?.(state.teamId);
        }
    });
}

/**
 * Fill the form from an override that already exists.
 *
 * @param {Object} state - this open's state
 * @param {Object} head - the override, as the list reports it
 * @param {string} scheduleId - which schedule it belongs to
 */
function populateEditForm(state, head, scheduleId) {
    // Derived, not assumed. The create form defaults to earlier/later because
    // nobody has chosen yet; here both instants already exist, and taking the
    // default would move an override anchored to the second pass of a repeated
    // local hour back by an hour the moment someone opens this form and saves
    // it unchanged.
    const foldZone = state.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone;
    state.fold = {
        start: foldOf(head.valid_from, foldZone),
        end: foldOf(head.valid_to, foldZone),
    };
    state.editing = {
        overrideId: head.override_id,
        revision: head.revision !== undefined && head.revision !== null
            ? parseInt(head.revision, 10)
            : null,
        scheduleId,
    };

    const userSelect = document.getElementById('override-user');
    if (userSelect) userSelect.value = head.user_id;

    const tz = state.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone;
    const startInput = document.getElementById('override-start');
    const endInput = document.getElementById('override-end');
    if (startInput) startInput.value = instantToLocalInput(head.valid_from, tz);
    if (endInput) endInput.value = instantToLocalInput(head.valid_to, tz);

    const reasonInput = document.getElementById('override-reason');
    if (reasonInput) reasonInput.value = head.reason || '';

    const title = document.querySelector('.override-form-title');
    if (title) title.innerHTML = '<i data-lucide="pencil"></i> Edit Override';
    const submitBtn = document.querySelector('#modal-footer button[type="submit"]');
    if (submitBtn) submitBtn.textContent = 'Save Changes';

    const form = document.getElementById('override-form');
    if (form) form.scrollIntoView({ behavior: 'smooth' });

    if (window.lucide) lucide.createIcons();
}

function resetForm(state) {
    state.fold = { start: 'earlier', end: 'later' };
    state.editing = null;
    const title = document.querySelector('.override-form-title');
    if (title) title.innerHTML = '<i data-lucide="plus-circle"></i> Create New Override';
    const submitBtn = document.querySelector('#modal-footer button[type="submit"]');
    if (submitBtn) submitBtn.textContent = 'Create Override';
}

async function refreshList(session, state) {
    if (session.closed) return;

    const body = modalShell().body;
    const contentWrapper = body?.querySelector('.override-modal-content');
    if (!contentWrapper) return;

    let overrideList;
    try {
        overrideList = await API.schedules.listOverrides(state.teamId, { signal: session.signal });
    } catch (e) {
        if (session.closed) return;
        // The list on screen was correct a moment ago. Replacing it with an
        // empty one because a refresh failed turns a transient error into
        // "your overrides are gone".
        showToast('Could not refresh overrides: ' + (e?.message || 'request failed'), 'error');
        return;
    }
    if (session.closed) return;
    const overrides = overrideList?.overrides || [];
    const names = await resolveNames(overrides.map(o => o.user_id));
    if (session.closed) return;

    const html = overridesList(overrides, state.scheduleId, names);
    const formSection = contentWrapper.querySelector('.override-form-section');
    const existingList = contentWrapper.querySelector('.overrides-list');

    if (html) {
        if (existingList) {
            existingList.outerHTML = html;
        } else if (formSection) {
            formSection.insertAdjacentHTML('beforebegin', html);
        }
    } else if (existingList) {
        existingList.remove();
    }
    if (window.lucide) lucide.createIcons();
}

/**
 * Show the same moments in a different zone.
 *
 * The picker chooses how times are displayed and entered; it is not part of an
 * override, which is stored as two instants. So changing it re-renders those
 * instants and never moves them. Keeping the wall-clock digits instead would
 * mean that opening an override saved as 09:00Z, switching the display from
 * Moscow to UTC and editing only the reason would save it as 12:00Z - the
 * override silently rescheduled by three hours by a control that claims to
 * change nothing.
 *
 * A time that does not exist in the old zone has no instant to carry over, so
 * its digits stay put and the note explains why.
 */
function retimeFields(state, newTz) {
    const oldTz = state.timezone;
    state.timezone = newTz;
    if (!oldTz || oldTz === newTz) {
        describeTimes(state);
        return;
    }

    const startInput = document.getElementById('override-start');
    const endInput = document.getElementById('override-end');
    // Resolved with the occurrence that was chosen, not the default: someone
    // who picked the second pass of an ambiguous hour and then changed the
    // display zone would otherwise be handed the first one back.
    const from = resolveLocalTime(startInput?.value, oldTz, { prefer: state.fold.start });
    const to = resolveLocalTime(endInput?.value, oldTz, { prefer: state.fold.end });

    if (startInput && from.instant) startInput.value = instantToLocalInput(from.instant, newTz);
    if (endInput && to.instant) endInput.value = instantToLocalInput(to.instant, newTz);

    describeTimes(state);
}

/**
 * Say what the entered times resolve to, when it is not obvious.
 *
 * Most of the year this stays silent. On a daylight-saving boundary a local
 * time can fail to exist or happen twice, and an override is recorded as an
 * instant - so the moment it is ambiguous, which one was chosen has to be
 * visible rather than decided quietly.
 */
function describeTimes(state) {
    const note = document.getElementById('override-time-note');
    if (!note) return;

    const tz = state.timezone || 'UTC';
    if (!document.getElementById('override-start')?.value ||
        !document.getElementById('override-end')?.value) {
        note.innerHTML = '';
        return;
    }

    const { from, to } = enteredWindow(state);
    const lines = [];

    if (from.kind === 'gap') {
        lines.push(`<div class="override-time-note-line is-error">${escapeHtml('Start: ' + gapMessage(tz))}</div>`);
    }
    if (to.kind === 'gap') {
        lines.push(`<div class="override-time-note-line is-error">${escapeHtml('End: ' + gapMessage(tz))}</div>`);
    }

    // An ambiguous time is not decided quietly. The choice is stated, and it
    // can be changed - the default only says which way to lean, not which
    // moment someone meant.
    const foldLine = (field, result, chosenWord, otherWord) => {
        const other = state.fold[field] === 'earlier' ? 'later' : 'earlier';
        return `
            <div class="override-time-note-line is-info">
                ${escapeHtml(`${field === 'start' ? 'Start' : 'End'} happens twice in ${tz}; using the ${chosenWord} one (${result.offsetLabel}).`)}
                <button type="button" class="btn-link override-fold-toggle" data-field="${field}" data-prefer="${other}">
                    ${escapeHtml(`Use the ${otherWord} one instead`)}
                </button>
            </div>`;
    };
    if (from.kind === 'ambiguous') {
        const first = state.fold.start === 'earlier';
        lines.push(foldLine('start', from, first ? 'first' : 'second', first ? 'second' : 'first'));
    }
    if (to.kind === 'ambiguous') {
        const first = state.fold.end === 'earlier';
        lines.push(foldLine('end', to, first ? 'first' : 'second', first ? 'second' : 'first'));
    }

    note.innerHTML = lines.join('');
}

/** The entered window, resolved with this open's fold choices. */
function enteredWindow(state) {
    const tz = state.timezone || 'UTC';
    return {
        from: resolveLocalTime(document.getElementById('override-start')?.value, tz,
            { prefer: state.fold.start }),
        to: resolveLocalTime(document.getElementById('override-end')?.value, tz,
            { prefer: state.fold.end }),
    };
}

async function handleSubmit(e, session, state, options, leave) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;
    const scheduleId = state.editing?.scheduleId || form.dataset.scheduleId;

    const timezone = state.timezone
        || document.querySelector('#override-timezone input[type=hidden]')?.value
        || 'UTC';
    const userId = document.getElementById('override-user')?.value;

    if (!userId) {
        showToast('Please select a user', 'error');
        return;
    }

    const { from, to } = enteredWindow(state);
    if (from.kind === 'gap' || to.kind === 'gap' || !from.instant || !to.instant) {
        describeTimes(state);
        showToast(gapMessage(timezone), 'error');
        return;
    }
    if (to.instant <= from.instant) {
        showToast('The override must end after it starts', 'error');
        return;
    }

    const overrideData = {
        user_id: userId,
        valid_from: from.instant.toISOString(),
        valid_to: to.instant.toISOString(),
        reason: document.getElementById('override-reason')?.value || '',
    };

    try {
        // Writes carry no session signal: closing the modal must not abort a
        // change that may already have landed.
        if (state.editing) {
            await API.schedules.updateOverride(
                scheduleId, state.editing.overrideId, overrideData, state.editing.revision);
            showToast('Override updated', 'success');
        } else {
            await API.schedules.createOverride(teamId, overrideData);
            showToast('Override created', 'success');
        }

        // A change that lands after this modal was replaced is still worth
        // reporting and still worth redrawing the widgets for. What it must
        // not do is reset a form or navigate away from somebody else's modal.
        let leaving;
        if (!session.closed) {
            resetForm(state);
            leaving = leave();
        }
        await Promise.all([leaving, options.onChanged?.(teamId)]);
    } catch (error) {
        console.error('Failed to save override:', error);
        await reportOverrideError(error, {
            // The refusal is reported either way - that happens above, and it
            // is true wherever the user is. Recovering the form is not: this
            // one reaches for the title and the footer by shape, and by now
            // they may belong to a modal that is in the middle of its own
            // edit. The whole recovery goes, not just the part that redraws.
            onStale: async () => {
                if (session.closed) return;
                resetForm(state);
                await refreshList(session, state);
            },
        });
    }
}

/**
 * Remove an override, wherever it was reached from.
 *
 * The revision is what makes the removal safe, and only the list of override
 * heads carries it: a calendar band and an on-call row both know which
 * override is in force without knowing which revision it is at, so the head is
 * read here when the caller has no number to give.
 *
 * @param {Object} target - {teamId, overrideId, scheduleId?, revision?}
 * @param {Object} [handlers]
 * @param {() => Promise<void>|void} [handlers.onStale] - what the caller shows is behind
 * @returns {Promise<boolean>} whether the override is gone
 */
export async function removeOverride(target, { onStale } = {}) {
    let { scheduleId, revision } = target;
    const { teamId, overrideId } = target;

    if (!Number.isInteger(revision) || !scheduleId) {
        // The whole preflight under one guard, not just its first read.
        //
        // Reaching here from the calendar means neither the revision nor the
        // schedule id was carried, so this is two requests, and the second one
        // fails the same ways as the first. Guarding only the first left the
        // second to reject out of an event handler, where nothing catches it:
        // no error UI, and an unhandled rejection in the console.
        let head;
        try {
            head = await currentOverrideHead(teamId, overrideId);
            if (head && !scheduleId) {
                // Same rule as loadOverrideState: only a 404 means "no schedule".
                const config = await API.schedules.getConfig(teamId).catch(e => {
                    if (e?.status === 404) return null;
                    throw e;
                });
                scheduleId = config?.schedule_id || '';
            }
        } catch (error) {
            console.error('Failed to read the override before removing it:', error);
            showToast('Could not reach the server: ' + (error?.message || 'request failed'), 'error');
            return false;
        }
        if (!head) {
            showToast('This override no longer exists.', 'error');
            await onStale?.();
            return false;
        }
        revision = head.revision;
    }

    if (!confirm('Remove this override?')) return false;

    try {
        await API.schedules.deleteOverride(scheduleId, overrideId, revision);
        showToast('Override removed', 'success');
        return true;
    } catch (error) {
        console.error('Failed to delete override:', error);
        await reportOverrideError(error, { onStale });
        return false;
    }
}

/**
 * Remove the override an on-call widget's button names.
 *
 * The button lives in a page widget rather than in a modal, so it is routed
 * here by the dispatcher. Which team it belongs to is a fact about the widget
 * around it, not about the button.
 *
 * @param {HTMLElement} button
 * @param {Object} [handlers] - {onRemoved}
 */
export async function removeOverrideFromWidget(button, { onRemoved } = {}) {
    const widget = button.closest('.oncall-row') || button.closest('.on-call-widget');
    const teamId = widget?.dataset.teamId;
    if (!teamId) return;

    const removed = await removeOverride({
        teamId,
        scheduleId: button.dataset.scheduleId,
        overrideId: button.dataset.overrideId,
        revision: parseInt(button.dataset.revision, 10),
    }, {
        onStale: () => onRemoved?.(teamId),
    });

    if (removed) await onRemoved?.(teamId);
}

/**
 * The revision an override is currently at.
 *
 * A removal has to name the revision it is removing, and the list of override
 * heads is the only place that number comes from.
 *
 * The read is allowed to fail, and a failure is NOT an answer: swallowing it
 * into an empty list made "the server did not respond" and "somebody already
 * removed this" the same sentence on screen, and only one of those means the
 * user should stop trying.
 */
async function currentOverrideHead(teamId, overrideId) {
    const list = await API.schedules.listOverrides(teamId);
    return (list?.overrides || []).find(o => o.override_id === overrideId) || null;
}
