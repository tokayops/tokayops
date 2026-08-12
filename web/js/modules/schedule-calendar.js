/**
 * The schedule calendar, and the menu that hangs off an override in it.
 *
 * One open of the calendar is one session. The context menu is part of it -
 * it is created when the calendar needs it, it lives in `document.body`
 * because it has to escape the modal's overflow, and it goes when the calendar
 * goes. Its listeners are on `document` for the same reason, and they are the
 * ones that used to be described as "the modal's": closing an override modal
 * took them down and left the calendar behind it inert.
 *
 * Escape belongs to whoever is nearest. With the menu open it closes the menu
 * and nothing else, so this session answers for the key itself rather than
 * letting the app close the whole modal underneath an open menu.
 */

import { escapeHtml, openModal } from '/js/core/utils.js';
import { beginModalSession, modalShell } from '/js/core/modal-session.js';
import { initTimezonePicker } from '/js/core/timezone-picker.js';
import { resolveNames } from '/js/core/users-directory.js';
import { monthlyScheduleCalendar } from '/js/modules/schedule-components.js';

// Which zone the calendar is read in. A preference rather than state: it
// outlives the modal on purpose, and keeping it in storage is what lets it do
// that without a variable beside the module that every open has to remember to
// reset.
const TIMEZONE_KEY = 'tokay.schedule.calendar-timezone';

function preferredTimezone() {
    const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const stored = localStorage.getItem(TIMEZONE_KEY);
    if (!stored) return browserTz;
    try {
        // A zone that the engine no longer knows would throw from inside the
        // render, where it reads as "the calendar is broken" rather than as
        // "that preference is stale".
        Intl.DateTimeFormat(undefined, { timeZone: stored });
        return stored;
    } catch {
        localStorage.removeItem(TIMEZONE_KEY);
        return browserTz;
    }
}

/**
 * Open the calendar for a team.
 *
 * @param {string} teamId
 * @param {Object} [options]
 * @param {(overrideId: string) => Promise<void>|void} [options.onEditOverride]
 * @param {(overrideId: string, handlers: Object) => Promise<boolean>} [options.onRemoveOverride]
 * @param {(teamId: string) => void} [options.onChanged] - an override was removed here
 */
export async function openScheduleCalendar(teamId, options = {}) {
    const view = { teamId, timezone: preferredTimezone(), menu: null };

    const session = beginModalSession({
        // Escape belongs to whatever is nearest, and with the menu open that is
        // the menu. Every other dismissal, and Escape with no menu showing,
        // means the calendar.
        onDismiss: (event) => {
            if (event.type !== 'keydown') return false;
            if (!view.menu?.classList.contains('active')) return false;
            hideContextMenu(view);
            return true;
        },
    });

    const { title, footer } = modalShell();
    title.textContent = 'Schedule Calendar';
    footer.innerHTML = `
        <button class="btn btn-secondary" id="calendar-close">Close</button>
    `;

    openModal('modal-overlay');

    // Bound before the calendar is fetched, not after. The button is on screen
    // as soon as the footer is written, and a button that is visible but inert
    // for as long as a request takes is a button that gets clicked twice.
    document.getElementById('calendar-close')?.addEventListener(
        'click', () => session.closeModal(), { signal: session.signal });

    bindCalendarEvents(session, view, options);

    await renderCalendar(session, view);
    return session;
}

async function renderCalendar(session, view) {
    // Asked for by the timezone picker as well as by this module, and the
    // picker keeps its own listeners: a zone chosen as the modal closed must
    // not draw a calendar into whatever replaced it.
    if (session.closed) return;

    const modalContent = modalShell().body;

    modalContent.innerHTML = `
        <div class="calendar-loading">
            <div class="spinner"></div>
            <span>Loading schedule...</span>
        </div>
    `;

    try {
        const now = new Date();
        const until = new Date(now);
        // Well inside the 90-day cap the server enforces.
        until.setDate(until.getDate() + 30);

        const render = await API.schedules.render(view.teamId, now, until,
            { signal: session.signal });
        if (session.closed) return;
        const names = await resolveNames((render.entries || []).flatMap(e => e.user_ids || []));
        if (session.closed) return;

        modalContent.innerHTML = monthlyScheduleCalendar(render, now, view.timezone, names);

        initTimezonePicker('calendar-timezone-select', {
            selected: view.timezone,
            onChange: (tz) => {
                view.timezone = tz;
                localStorage.setItem(TIMEZONE_KEY, tz);
                renderCalendar(session, view);
            }
        });
    } catch (error) {
        if (session.closed) return;
        console.error('Failed to load schedule:', error);
        modalContent.innerHTML = `
            <div class="schedule-empty">
                <i data-lucide="alert-circle" style="color: var(--status-critical);"></i>
                <p>Failed to load schedule</p>
                <p style="font-size: 0.8em; color: var(--text-muted);">${escapeHtml(error.message)}</p>
            </div>
        `;
    }

    if (window.lucide) lucide.createIcons();
}

// ========================================
// The menu on an override
// ========================================

function bindCalendarEvents(session, view, options) {
    const listen = (target, type, handler, opts = {}) =>
        target.addEventListener(type, handler, { ...opts, signal: session.signal });

    // The menu is the calendar's, and it leaves with it. Otherwise it stays in
    // the body over whatever opens next, pointing at an override that screen
    // knows nothing about.
    session.signal.addEventListener('abort', () => {
        view.menu?.remove();
        view.menu = null;
    });

    listen(modalShell().body, 'click', (e) => {
        const entry = e.target.closest('.calendar-entry.layer-override');
        if (!entry?.dataset.overrideId) return;
        e.preventDefault();
        e.stopPropagation();
        showContextMenu(view, entry);
    });

    // The menu itself is outside the modal, so its clicks are caught here -
    // as is every click that means "not the menu".
    listen(document, 'click', (e) => {
        if (!view.menu) return;

        const item = e.target.closest('.override-context-menu-item');
        if (item) {
            e.preventDefault();
            e.stopPropagation();
            handleMenuAction(session, view, options, item.dataset.action);
            return;
        }

        if (e.target.closest('.override-context-menu')) return;
        hideContextMenu(view);
    });

    listen(document, 'scroll', () => hideContextMenu(view), { capture: true });
}

function getOrCreateContextMenu(view) {
    if (view.menu && document.body.contains(view.menu)) return view.menu;

    const menu = document.createElement('div');
    menu.className = 'override-context-menu';
    menu.innerHTML = `
        <button type="button" class="override-context-menu-item" data-action="edit">
            <i data-lucide="pencil"></i>
            Edit
        </button>
        <button type="button" class="override-context-menu-item danger" data-action="delete">
            <i data-lucide="trash-2"></i>
            Delete
        </button>
    `;
    document.body.appendChild(menu);
    if (window.lucide) lucide.createIcons();
    view.menu = menu;
    return menu;
}

function showContextMenu(view, targetEl) {
    const menu = getOrCreateContextMenu(view);
    menu.classList.remove('active');

    menu.dataset.overrideId = targetEl.dataset.overrideId;
    menu.dataset.userId = targetEl.dataset.userId;
    menu.dataset.validFrom = targetEl.dataset.validFrom;
    menu.dataset.validTo = targetEl.dataset.validTo;

    const rect = targetEl.getBoundingClientRect();
    const menuWidth = 160;
    const menuHeight = 80;

    let top = rect.bottom + 4;
    let left = rect.right - menuWidth;

    if (top + menuHeight > window.innerHeight) {
        top = rect.top - menuHeight - 4;
    }
    if (left < 8) {
        left = rect.left;
    }

    menu.style.top = `${top}px`;
    menu.style.left = `${left}px`;

    requestAnimationFrame(() => {
        menu.classList.add('active');
    });
}

function hideContextMenu(view) {
    view.menu?.classList.remove('active');
}

async function handleMenuAction(session, view, options, action) {
    const overrideId = view.menu?.dataset.overrideId;
    hideContextMenu(view);
    if (!overrideId) return;

    if (action === 'edit') {
        // Editing leaves the calendar: the override modal takes the screen,
        // and every way out of it comes back here.
        await options.onEditOverride?.(overrideId);
        return;
    }

    if (action === 'delete') {
        const removed = await options.onRemoveOverride?.(overrideId, {
            // The calendar draws shifts, so an override that is already gone
            // is still on screen until this is redrawn.
            onStale: () => renderCalendar(session, view),
        });
        if (!removed || session.closed) return;
        await Promise.all([
            renderCalendar(session, view),
            options.onChanged?.(view.teamId),
        ]);
        if (window.lucide) lucide.createIcons();
    }
}
