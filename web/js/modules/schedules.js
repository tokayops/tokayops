/**
 * TokayOps Schedules Module
 * Handles on-call schedule UI interactions
 * 
 * Phase 3: Schedule Management
 */

import { showToast, escapeHtml } from '/js/core/utils.js';
import { State } from '/js/core/state.js';
import { renderAlertGroups } from '/js/modules/alerts.js';
import { initTimezonePicker } from '/js/core/timezone-picker.js';

// ========================================
// State
// ========================================

let currentTeamId = null;
let currentSchedule = null;
let teamMembers = [];
let editingOverrideId = null;
let editingScheduleId = null;
let returnToCalendar = false;
let currentOverrideTz = null;

// ========================================
// Override Context Menu (singleton)
// ========================================

let overrideContextMenu = null;

function getOrCreateContextMenu() {
    if (overrideContextMenu && document.body.contains(overrideContextMenu)) {
        return overrideContextMenu;
    }
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
    overrideContextMenu = menu;
    return menu;
}

function showContextMenu(targetEl) {
    const menu = getOrCreateContextMenu();
    menu.classList.remove('active');

    // Copy data attributes from the target element to the menu
    menu.dataset.scheduleId = targetEl.dataset.scheduleId;
    menu.dataset.overrideId = targetEl.dataset.overrideId;
    menu.dataset.userId = targetEl.dataset.userId;
    menu.dataset.startTime = targetEl.dataset.startTime;
    menu.dataset.endTime = targetEl.dataset.endTime;
    menu.dataset.reason = targetEl.dataset.reason || '';

    // Track source context for handling edit from calendar vs modal
    menu.dataset.source = targetEl.classList.contains('calendar-entry') ? 'calendar' : 'modal';

    // Position relative to the target element
    const rect = targetEl.getBoundingClientRect();
    const menuWidth = 160;
    const menuHeight = 80;

    let top = rect.bottom + 4;
    let left = rect.right - menuWidth;

    // If menu would go below viewport, show above
    if (top + menuHeight > window.innerHeight) {
        top = rect.top - menuHeight - 4;
    }
    // If menu would go off left edge, align to left of target
    if (left < 8) {
        left = rect.left;
    }

    menu.style.top = `${top}px`;
    menu.style.left = `${left}px`;

    requestAnimationFrame(() => {
        menu.classList.add('active');
    });
}

function hideContextMenu() {
    if (overrideContextMenu) {
        overrideContextMenu.classList.remove('active');
    }
}

// Close context menu on outside click, Escape, scroll
document.addEventListener('click', (e) => {
    if (overrideContextMenu &&
        !e.target.closest('.override-context-menu') &&
        !e.target.closest('.calendar-entry.layer-override')) {
        hideContextMenu();
    }
});
document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;

    // Priority 1: close context menu
    if (overrideContextMenu && overrideContextMenu.classList.contains('active')) {
        e.stopImmediatePropagation();
        hideContextMenu();
        return;
    }

    // Priority 2: return to calendar from override edit (instead of closing modal)
    if (returnToCalendar && currentTeamId) {
        e.stopImmediatePropagation();
        e.preventDefault();
        document.getElementById('override-cancel')?.click();
    }
});
document.addEventListener('scroll', () => hideContextMenu(), true);

// Intercept X button and overlay-background clicks when returnToCalendar is active —
// redirect to cancel (which returns to calendar) instead of closing modal.
document.addEventListener('click', (e) => {
    if (!returnToCalendar || !currentTeamId) return;

    const isCloseBtn = e.target.closest('#modal-close');
    const isOverlayBg = e.target.id === 'modal-overlay';

    if (isCloseBtn || isOverlayBg) {
        e.stopPropagation();
        e.preventDefault();
        document.getElementById('override-cancel')?.click();
    }
}, true); // capture phase — fires before bubble handlers in app.js

function populateOverrideEditForm(data) {
    editingOverrideId = data.overrideId;
    editingScheduleId = data.scheduleId;

    const userSelect = document.getElementById('override-user');
    if (userSelect) userSelect.value = data.userId;

    const toLocalInput = (isoStr) => {
        const d = new Date(isoStr);
        const pad = (n) => String(n).padStart(2, '0');
        return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    };

    const startInput = document.getElementById('override-start');
    const endInput = document.getElementById('override-end');
    if (startInput) startInput.value = toLocalInput(data.startTime);
    if (endInput) endInput.value = toLocalInput(data.endTime);

    const reasonInput = document.getElementById('override-reason');
    if (reasonInput) reasonInput.value = data.reason || '';

    const title = document.querySelector('.override-form-title');
    if (title) title.innerHTML = '<i data-lucide="pencil"></i> Edit Override';
    const submitBtn = document.querySelector('#modal-footer button[type="submit"]');
    if (submitBtn) submitBtn.textContent = 'Save Changes';

    const form = document.getElementById('override-form');
    if (form) form.scrollIntoView({ behavior: 'smooth' });

    if (window.lucide) lucide.createIcons();
}

// ========================================
// On-Call Widget
// ========================================

/**
 * Load and render on-call widget for a team section
 * @param {string} teamId - Team ID
 * @param {HTMLElement} container - Container element
 */
export async function loadOnCallWidget(teamId, container) {
    if (!container) return;

    try {
        // Try to get schedule and on-call data
        const [schedule, onCallResult] = await Promise.all([
            API.schedules.get(teamId).catch(() => null),
            API.schedules.getOnCall(teamId).catch(() => null)
        ]);

        if (schedule || onCallResult) {
            container.innerHTML = Components.onCallWidget(schedule, onCallResult, teamId);
            if (window.lucide) lucide.createIcons();
        } else {
            // No schedule - show setup prompt
            container.innerHTML = `
                <div class="on-call-widget" data-team-id="${teamId}">
                    <div class="on-call-header">
                        <i data-lucide="phone-call" class="on-call-icon"></i>
                        <span class="on-call-title">On-Call</span>
                    </div>
                    <div class="on-call-user">
                        <div class="on-call-user-info">
                            <span class="on-call-user-name" style="color: var(--text-muted);">No schedule configured</span>
                        </div>
                    </div>
                    <div class="on-call-actions">
                        <button class="btn btn-primary edit-schedule-btn" data-team-id="${teamId}">
                            <i data-lucide="calendar"></i>
                            Configure Schedule
                        </button>
                    </div>
                </div>
            `;
            if (window.lucide) lucide.createIcons();
        }
    } catch (error) {
        console.error('Failed to load on-call widget:', error);
    }
}

async function refreshOnCallUI(teamId) {
    if (!teamId) return;
    try {
        const [schedule, onCallResult] = await Promise.all([
            API.schedules.get(teamId).catch(() => null),
            API.schedules.getOnCall(teamId).catch(() => null)
        ]);

        if (State.onCallByTeam) {
            State.onCallByTeam[teamId] = { data: onCallResult, fetchedAt: Date.now() };
            renderAlertGroups();
        }

        const safeId = typeof CSS !== 'undefined' && CSS.escape ? CSS.escape(teamId) : teamId;
        const widgets = document.querySelectorAll(`.oncall-row[data-team-id="${safeId}"], .on-call-widget[data-team-id="${safeId}"]`);
        const team = State.teams.find(t => t.id === teamId) || { id: teamId, name: teamId };

        if (widgets.length > 0) {
            widgets.forEach(widget => {
                const isOverview = widget.classList.contains('oncall-row')
                    || widget.closest('.on-call-overview');
                widget.outerHTML = isOverview
                    ? Components.onCallOverviewRow(schedule, onCallResult, team)
                    : Components.onCallWidget(schedule, onCallResult, teamId);
            });
        } else {
            const rowSlot = document.querySelector(`.oncall-row-slot[data-team-id="${safeId}"]`);
            if (rowSlot) {
                rowSlot.innerHTML = Components.onCallOverviewRow(schedule, onCallResult, team);
            }
        }

        if (window.lucide) lucide.createIcons();

        // Update overrides list if modal is open for this team
        const modalContent = document.getElementById('modal-body');
        const overrideForm = modalContent?.querySelector('#override-form');
        if (overrideForm && overrideForm.dataset.teamId === teamId) {
            const overridesListHtml = Components.overridesList(schedule?.overrides || [], schedule?.id || '');
            const contentWrapper = modalContent.querySelector('.override-modal-content');
            const formSection = contentWrapper?.querySelector('.override-form-section');
            const existingList = contentWrapper?.querySelector('.overrides-list');

            if (overridesListHtml) {
                if (existingList) {
                    existingList.outerHTML = overridesListHtml;
                } else if (formSection) {
                    formSection.insertAdjacentHTML('beforebegin', overridesListHtml);
                }
            } else if (existingList) {
                existingList.remove();
            }

            if (window.lucide) lucide.createIcons();
        }
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
        const [schedule, onCallResult] = await Promise.all([
            API.schedules.get(team.id).catch(() => null),
            API.schedules.getOnCall(team.id).catch(() => null)
        ]);

        container.innerHTML = Components.onCallOverviewRow(schedule, onCallResult, team);
        if (window.lucide) lucide.createIcons();
    } catch (error) {
        console.error('Failed to load on-call overview row:', error);
        container.innerHTML = Components.onCallOverviewRow(null, null, team);
        if (window.lucide) lucide.createIcons();
    }
}

// ========================================
// Schedule Config Modal
// ========================================

async function openScheduleConfigModal(teamId) {
    currentTeamId = teamId;

    try {
        // Load team members and existing schedule
        const [membersResponse, schedule] = await Promise.all([
            API.teams.members(teamId),
            API.schedules.get(teamId).catch(() => null)
        ]);

        teamMembers = membersResponse?.users || [];
        currentSchedule = schedule;

        // Create modal
        const modal = document.getElementById('modal-overlay');
        const modalTitle = document.getElementById('modal-title');
        const modalContent = document.getElementById('modal-body');
        const modalFooter = document.getElementById('modal-footer');

        modalTitle.textContent = 'Configure On-Call Schedule';
        modalContent.innerHTML = Components.scheduleConfigModal(schedule, teamMembers, teamId);
        modalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="schedule-cancel">Cancel</button>
            <button type="submit" form="schedule-form" class="btn btn-primary" id="schedule-form-submit">Save Schedule</button>
        `;

        modal.classList.add('active');
        document.body.style.overflow = 'hidden';
        if (window.lucide) lucide.createIcons();

        // Bind form events
        bindScheduleFormEvents();

        // Initialize timezone picker
        initTimezonePicker('schedule-timezone', {
            selected: schedule?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone,
        });
    } catch (error) {
        console.error('Failed to open schedule modal:', error);
        showToast('Failed to load schedule configuration', 'error');
    }
}

function bindScheduleFormEvents() {
    const form = document.getElementById('schedule-form');
    if (!form) return;

    // Initialize L1 groups editor
    initGroupsEditor('l1-groups-editor', 'l1-add-group');

    // L2 still uses flat sortable list
    initSortableLists('l2-available', 'l2-users-list');

    // L1 rotation type toggle
    const l1Type = document.getElementById('l1-rotation-type');
    const l1WeeklyOnly = document.querySelector('.l1-weekly-only');
    if (l1Type && l1WeeklyOnly) {
        l1Type.addEventListener('change', () => {
            l1WeeklyOnly.style.display = l1Type.value === 'weekly' ? '' : 'none';
        });
    }

    // L2 enabled toggle
    const l2Enabled = document.getElementById('l2-enabled');
    const l2Config = document.querySelector('.l2-config');
    const l2Panel = document.querySelector('.l2-users-panel');
    if (l2Enabled) {
        l2Enabled.addEventListener('change', () => {
            if (l2Config) l2Config.style.display = l2Enabled.checked ? '' : 'none';
            if (l2Panel) l2Panel.style.display = l2Enabled.checked ? '' : 'none';
        });
    }

    // Cancel button
    document.getElementById('schedule-cancel')?.addEventListener('click', () => {
        document.getElementById('modal-overlay').classList.remove('active');
        document.body.style.overflow = '';
    });

    // Form submit
    form.addEventListener('submit', handleScheduleSubmit);

    // Delete schedule button
    const deleteScheduleBtn = document.querySelector('.delete-schedule-btn');
    if (deleteScheduleBtn) {
        deleteScheduleBtn.addEventListener('click', async () => {
            if (confirm('Delete this schedule? All rotations and overrides will be removed. This cannot be undone.')) {
                try {
                    await API.schedules.delete(deleteScheduleBtn.dataset.teamId);
                    showToast('Schedule deleted', 'success');
                    document.getElementById('modal-overlay').classList.remove('active');
                    document.body.style.overflow = '';
                    await refreshOnCallUI(deleteScheduleBtn.dataset.teamId);
                } catch (err) {
                    showToast('Failed to delete schedule: ' + err.message, 'error');
                }
            }
        });
    }
}

/**
 * Initialize SortableJS for a pair of available/selected lists
 */
function initSortableLists(availableId, selectedId) {
    const availableList = document.getElementById(availableId);
    const selectedList = document.getElementById(selectedId);

    if (!availableList || !selectedList || typeof Sortable === 'undefined') return;

    // Available list - drag from here
    new Sortable(availableList, {
        group: availableId.replace('-available', '-rotation'),
        animation: 150,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        sort: false,
        onEnd: () => {
            if (window.lucide) lucide.createIcons();
        }
    });

    // Selected list - drag to here and reorder
    new Sortable(selectedList, {
        group: availableId.replace('-available', '-rotation'),
        animation: 150,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        handle: '.drag-handle',
        onAdd: (evt) => {
            // Add drag handle when item enters selected list
            const item = evt.item;
            if (!item.querySelector('.drag-handle')) {
                const handle = document.createElement('i');
                handle.setAttribute('data-lucide', 'grip-vertical');
                handle.className = 'drag-handle';
                item.insertBefore(handle, item.firstChild);
                if (window.lucide) lucide.createIcons();
            }
        },
        onRemove: (evt) => {
            // Remove drag handle when item leaves selected list
            const item = evt.item;
            const handle = item.querySelector('.drag-handle');
            if (handle) handle.remove();
        },
        onEnd: () => {
            if (window.lucide) lucide.createIcons();
        }
    });
}

/**
 * Initialize the L1 groups editor: drag-and-drop reorder, add/remove groups,
 * add/remove users within groups.
 */
function initGroupsEditor(editorId, addGroupBtnId) {
    const editor = document.getElementById(editorId);
    const addGroupBtn = document.getElementById(addGroupBtnId);
    if (!editor) return;

    // Build select options from module-level teamMembers (set in openScheduleConfig).
    // Don't snapshot from DOM — the editor may start empty (no l1_groups), in which
    // case there's no existing select to copy options from.
    const buildOptions = () => {
        const opts = ['<option value="">+ Add user</option>'];
        for (const u of teamMembers) {
            opts.push(`<option value="${escapeHtml(u.id)}">${escapeHtml(u.name)}</option>`);
        }
        return opts.join('');
    };

    const renumberGroups = () => {
        editor.querySelectorAll('.group-row').forEach((row, i) => {
            row.dataset.groupIndex = i;
            const label = row.querySelector('.group-label');
            if (label) label.textContent = `Group ${i + 1}`;
        });
    };

    const createGroupRow = (index) => {
        const row = document.createElement('div');
        row.className = 'group-row';
        row.dataset.groupIndex = index;
        row.innerHTML = `
            <i data-lucide="grip-vertical" class="group-drag-handle"></i>
            <span class="group-label">Group ${index + 1}</span>
            <div class="group-chips"></div>
            <select class="form-select group-add-user">${buildOptions()}</select>
            <button type="button" class="btn btn-icon btn-sm group-delete" aria-label="Delete group">
                <i data-lucide="trash-2"></i>
            </button>
        `;
        return row;
    };

    // Sortable for reordering groups
    if (typeof Sortable !== 'undefined') {
        new Sortable(editor, {
            animation: 150,
            handle: '.group-drag-handle',
            ghostClass: 'sortable-ghost',
            chosenClass: 'sortable-chosen',
            onEnd: () => {
                renumberGroups();
                if (window.lucide) lucide.createIcons();
            }
        });
    }

    // Add group button
    if (addGroupBtn) {
        addGroupBtn.addEventListener('click', () => {
            const index = editor.querySelectorAll('.group-row').length;
            const row = createGroupRow(index);
            editor.appendChild(row);
            if (window.lucide) lucide.createIcons();
        });
    }

    // Event delegation for chip removal, group deletion, add-user select
    editor.addEventListener('click', (e) => {
        // Remove user chip
        const chipRemove = e.target.closest('.chip-remove');
        if (chipRemove) {
            chipRemove.closest('.user-chip')?.remove();
            return;
        }
        // Delete group
        const groupDelete = e.target.closest('.group-delete');
        if (groupDelete) {
            groupDelete.closest('.group-row')?.remove();
            renumberGroups();
            return;
        }
    });

    editor.addEventListener('change', (e) => {
        const select = e.target.closest('.group-add-user');
        if (!select || !select.value) return;
        const userId = select.value;
        const row = select.closest('.group-row');
        if (!row) return;
        const chips = row.querySelector('.group-chips');
        if (!chips) return;

        // Skip if user already in this group
        if (chips.querySelector(`.user-chip[data-user-id="${CSS.escape(userId)}"]`)) {
            select.value = '';
            return;
        }

        // Use option text as the user name
        const optionText = select.options[select.selectedIndex]?.text || userId;
        const chip = document.createElement('span');
        chip.className = 'user-chip';
        chip.dataset.userId = userId;
        chip.innerHTML = `${escapeHtml(optionText)}<button type="button" class="chip-remove" aria-label="Remove">×</button>`;
        chips.appendChild(chip);
        select.value = '';
    });
}


async function handleScheduleSubmit(e) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;

    // Get L1 groups from groups editor (order of groups matters!)
    const l1Groups = [];
    document.querySelectorAll('#l1-groups-editor .group-row').forEach(row => {
        const groupUserIds = [];
        row.querySelectorAll('.user-chip').forEach(chip => {
            if (chip.dataset.userId) groupUserIds.push(chip.dataset.userId);
        });
        if (groupUserIds.length > 0) {
            l1Groups.push(groupUserIds);
        }
    });

    // Get L2 users from selected list
    const l2UserIds = [];
    document.querySelectorAll('#l2-users-list .rotation-user').forEach(item => {
        l2UserIds.push(item.dataset.userId);
    });

    const l1Enabled = document.getElementById('l2-enabled')?.checked || false;
    const handoffTime = document.getElementById('l1-handoff-time')?.value || '11:00';

    // Build schedule data
    const scheduleData = {
        timezone: document.querySelector('#schedule-timezone input[type=hidden]')?.value || 'UTC',
        slack_usergroup_id: document.getElementById('slack-usergroup-id')?.value || '',
        l1_rotation_type: document.getElementById('l1-rotation-type')?.value || 'daily',
        l1_handoff_time: handoffTime,
        l1_handoff_day: parseInt(document.getElementById('l1-handoff-day')?.value) || 1,
        l1_rotation_start: new Date().toISOString(),
        l2_enabled: l1Enabled,
        l2_escalation_timeout: parseInt(document.getElementById('l2-escalation-timeout')?.value) || 5,
        l2_rotation_type: 'weekly',
        l2_rotation_start: new Date().toISOString()
    };

    try {
        // Save schedule
        await API.schedules.upsert(teamId, scheduleData);

        // Set L1 groups (always call to clear rotation if list is empty)
        await API.schedules.setL1Groups(teamId, l1Groups);
        if (l1Enabled) {
            await API.schedules.setL2Users(teamId, l2UserIds);
        }

        showToast('Schedule saved successfully', 'success');
        document.getElementById('modal-overlay').classList.remove('active');
        document.body.style.overflow = '';

        await refreshOnCallUI(teamId);
    } catch (error) {
        console.error('Failed to save schedule:', error);
        showToast('Failed to save schedule: ' + error.message, 'error');
    }
}

// ========================================
// Override Modal
// ========================================

async function openOverrideModal(teamId) {
    currentTeamId = teamId;

    try {
        // Fetch both team members and schedule (with overrides)
        const [membersResponse, schedule] = await Promise.all([
            API.teams.members(teamId),
            API.schedules.get(teamId).catch(() => null)
        ]);
        teamMembers = membersResponse?.users || [];
        const existingOverrides = schedule?.overrides || [];

        const modal = document.getElementById('modal-overlay');
        const modalTitle = document.getElementById('modal-title');
        const modalContent = document.getElementById('modal-body');
        const modalFooter = document.getElementById('modal-footer');

        // Reset edit state (don't reset returnToCalendar here — it's set AFTER openOverrideModal)
        editingOverrideId = null;
        editingScheduleId = null;
        // returnToCalendar is intentionally preserved when called from calendar edit flow

        modalTitle.textContent = 'Manage Overrides';
        modalContent.innerHTML = Components.overrideModal(teamMembers, teamId, existingOverrides, schedule?.id);
        modalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="override-cancel">Cancel</button>
            <button type="submit" form="override-form" class="btn btn-primary">Create Override</button>
        `;

        modal.classList.add('active');
        document.body.style.overflow = 'hidden';
        if (window.lucide) lucide.createIcons();

        // Initialize timezone picker
        const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        initTimezonePicker('override-timezone', {
            selected: browserTz,
            onChange: (newTz) => {
                const startInput = document.getElementById('override-start');
                const endInput = document.getElementById('override-end');
                if (startInput?.value) startInput.value = convertLocalTime(startInput.value, currentOverrideTz, newTz);
                if (endInput?.value) endInput.value = convertLocalTime(endInput.value, currentOverrideTz, newTz);
                currentOverrideTz = newTz;
            }
        });
        currentOverrideTz = browserTz;

        // Bind form events
        bindOverrideFormEvents();
    } catch (error) {
        console.error('Failed to open override modal:', error);
        showToast('Failed to load team members', 'error');
    }
}

function bindOverrideFormEvents() {
    const form = document.getElementById('override-form');
    if (!form) return;

    document.getElementById('override-cancel')?.addEventListener('click', async () => {
        const shouldReturnToCalendar = returnToCalendar;
        resetOverrideForm();
        if (shouldReturnToCalendar && currentTeamId) {
            await openViewScheduleModal(currentTeamId);
        } else {
            document.getElementById('modal-overlay').classList.remove('active');
            document.body.style.overflow = '';
        }
    });

    form.addEventListener('submit', handleOverrideSubmit);
}

/**
 * Convert a datetime-local value from one timezone to another.
 * E.g. "2026-04-01T09:00" in Asia/Bangkok → "2026-04-01T02:00" in UTC.
 */
function convertLocalTime(value, fromTz, toTz) {
    if (!value || fromTz === toTz) return value;
    const [datePart, timePart] = value.split('T');
    if (!datePart || !timePart) return value;
    const [y, m, d] = datePart.split('-').map(Number);
    const [h, min] = timePart.split(':').map(Number);

    // Build a UTC date, then find offset of fromTz to get the real UTC instant
    const guess = new Date(Date.UTC(y, m - 1, d, h, min));
    const fromOffset = tzOffsetMs(guess, fromTz);
    const utc = new Date(guess.getTime() - fromOffset);

    // Format that UTC instant in the target timezone
    const fmt = new Intl.DateTimeFormat('sv-SE', {
        timeZone: toTz,
        year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hour12: false,
    });
    return fmt.format(utc).replace(' ', 'T');
}

/** Get timezone offset in ms for a given UTC date and IANA timezone. */
function tzOffsetMs(utcDate, tz) {
    const fmt = new Intl.DateTimeFormat('en-US', {
        timeZone: tz,
        year: 'numeric', month: 'numeric', day: 'numeric',
        hour: 'numeric', minute: 'numeric', second: 'numeric',
        hour12: false,
    });
    const parts = {};
    for (const p of fmt.formatToParts(utcDate)) {
        parts[p.type] = parseInt(p.value, 10);
    }
    // Reconstruct what UTC would be if the local time were treated as UTC
    const localAsUtc = Date.UTC(parts.year, parts.month - 1, parts.day,
        parts.hour === 24 ? 0 : parts.hour, parts.minute, parts.second);
    return localAsUtc - utcDate.getTime();
}

async function handleOverrideSubmit(e) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;

    const timezone = document.querySelector('#override-timezone input[type=hidden]')?.value || 'UTC';
    const startVal = document.getElementById('override-start')?.value;
    const endVal = document.getElementById('override-end')?.value;

    const overrideData = {
        user_id: document.getElementById('override-user')?.value,
        timezone: timezone,
        start_time_local: startVal, // "2025-12-21T09:00"
        end_time_local: endVal,
        reason: document.getElementById('override-reason')?.value || ''
    };

    if (!overrideData.user_id) {
        showToast('Please select a user', 'error');
        return;
    }
    try {
        if (editingOverrideId) {
            await API.schedules.updateOverride(editingScheduleId, editingOverrideId, overrideData);
            showToast('Override updated successfully', 'success');
        } else {
            await API.schedules.createOverride(teamId, overrideData);
            showToast('Override created successfully', 'success');
        }
        const shouldReturnToCalendar = returnToCalendar;
        resetOverrideForm();

        if (shouldReturnToCalendar && currentTeamId) {
            // Return to the Schedule Calendar modal + refresh background widgets
            await Promise.all([
                openViewScheduleModal(currentTeamId),
                refreshOnCallUI(teamId),
            ]);
        } else {
            document.getElementById('modal-overlay').classList.remove('active');
            document.body.style.overflow = '';
            await refreshOnCallUI(teamId);
        }
    } catch (error) {
        const action = editingOverrideId ? 'update' : 'create';
        console.error(`Failed to ${action} override:`, error);
        showToast(`Failed to ${action} override: ` + error.message, 'error');
    }
}

function resetOverrideForm() {
    editingOverrideId = null;
    editingScheduleId = null;
    returnToCalendar = false;
    const title = document.querySelector('.override-form-title');
    if (title) title.innerHTML = '<i data-lucide="plus-circle"></i> Create New Override';
    const submitBtn = document.querySelector('#modal-footer button[type="submit"]');
    if (submitBtn) submitBtn.textContent = 'Create Override';
}

// ========================================
// View Schedule Modal
// ========================================

// ========================================
// View Schedule Modal
// ========================================

let currentViewTimezone = null;

async function renderCalendarView(teamId, timezone) {
    const modalContent = document.getElementById('modal-body');

    // Show loading
    modalContent.innerHTML = `
        <div class="calendar-loading">
            <div class="spinner"></div>
            <span>Loading schedule...</span>
        </div>
    `;

    try {
        const now = new Date();
        const until = new Date(now);
        until.setDate(until.getDate() + 30);

        const response = await API.schedules.render(teamId, now, until, timezone);
        const entries = response.entries || [];
        // Use requested timezone or server returned one
        const activeTz = timezone || response.timezone || 'UTC';

        modalContent.innerHTML = Components.monthlyScheduleCalendar(entries, now, activeTz);

        // Initialize timezone picker
        initTimezonePicker('calendar-timezone-select', {
            selected: activeTz,
            onChange: (tz) => {
                currentViewTimezone = tz;
                renderCalendarView(teamId, currentViewTimezone);
            }
        });

    } catch (error) {
        console.error('Failed to load schedule:', error);
        modalContent.innerHTML = `
            <div class="schedule-empty">
                <i data-lucide="alert-circle" style="color: var(--status-critical);"></i>
                <p>Failed to load schedule</p>
                <p style="font-size: 0.8em; color: var(--text-muted);">${error.message}</p>
            </div>
        `;
    }

    if (window.lucide) lucide.createIcons();
}

async function openViewScheduleModal(teamId) {
    currentTeamId = teamId;
    // Default to browser timezone if not set
    if (!currentViewTimezone) {
        currentViewTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    }

    const modal = document.getElementById('modal-overlay');
    const modalTitle = document.getElementById('modal-title');
    const modalFooter = document.getElementById('modal-footer');

    modalTitle.textContent = 'Schedule Calendar';
    modalFooter.innerHTML = `
        <button class="btn btn-secondary" id="calendar-close">Close</button>
    `;

    modal.classList.add('active');
    document.body.style.overflow = 'hidden';

    // Initial render
    await renderCalendarView(teamId, currentViewTimezone);

    // Close button
    document.getElementById('calendar-close')?.addEventListener('click', () => {
        modal.classList.remove('active');
        document.body.style.overflow = '';
    });
}

// ========================================
// Event Bindings
// ========================================

export function bindScheduleEvents() {
    // Use event delegation on document for dynamically created buttons
    document.addEventListener('click', async (e) => {
        // Edit Schedule button
        const editBtn = e.target.closest('.edit-schedule-btn');
        if (editBtn) {
            const teamId = editBtn.dataset.teamId;
            if (teamId) {
                openScheduleConfigModal(teamId);
            }
            return;
        }

        // Create Override button
        const overrideBtn = e.target.closest('.create-override-btn');
        if (overrideBtn) {
            const teamId = overrideBtn.dataset.teamId;
            if (teamId) {
                returnToCalendar = false;
                openOverrideModal(teamId);
            }
            return;
        }

        // View Schedule button
        const viewBtn = e.target.closest('.view-schedule-btn');
        if (viewBtn) {
            const teamId = viewBtn.dataset.teamId;
            if (teamId) {
                openViewScheduleModal(teamId);
            }
            return;
        }

        // Edit Override button (modal list)
        const editOverrideBtn = e.target.closest('.edit-override-btn');
        if (editOverrideBtn) {
            e.preventDefault();
            e.stopPropagation();
            populateOverrideEditForm({
                overrideId: editOverrideBtn.dataset.overrideId,
                scheduleId: editOverrideBtn.dataset.scheduleId,
                userId: editOverrideBtn.dataset.userId,
                startTime: editOverrideBtn.dataset.startTime,
                endTime: editOverrideBtn.dataset.endTime,
                reason: editOverrideBtn.dataset.reason,
            });
            return;
        }

        // Calendar override entry click → show context menu
        const calendarOverride = e.target.closest('.calendar-entry.layer-override');
        if (calendarOverride && calendarOverride.dataset.overrideId) {
            e.preventDefault();
            e.stopPropagation();
            showContextMenu(calendarOverride);
            return;
        }

        // Context menu action click
        const menuAction = e.target.closest('.override-context-menu-item');
        if (menuAction && overrideContextMenu) {
            e.preventDefault();
            e.stopPropagation();

            const action = menuAction.dataset.action;
            const menu = overrideContextMenu;

            if (action === 'edit') {
                hideContextMenu();
                // From calendar: open override modal, then populate form
                if (currentTeamId) {
                    const editData = {
                        overrideId: menu.dataset.overrideId,
                        scheduleId: menu.dataset.scheduleId,
                        userId: menu.dataset.userId,
                        startTime: menu.dataset.startTime,
                        endTime: menu.dataset.endTime,
                        reason: menu.dataset.reason,
                    };
                    returnToCalendar = true;
                    await openOverrideModal(currentTeamId);
                    populateOverrideEditForm(editData);
                }
            }

            if (action === 'delete') {
                const scheduleId = menu.dataset.scheduleId;
                const overrideId = menu.dataset.overrideId;
                hideContextMenu();

                if (!confirm('Remove this override?')) return;

                try {
                    await API.schedules.deleteOverride(scheduleId, overrideId);
                    showToast('Override removed', 'success');

                    // Refresh calendar view + background widgets
                    if (currentTeamId && currentViewTimezone) {
                        await Promise.all([
                            renderCalendarView(currentTeamId, currentViewTimezone),
                            refreshOnCallUI(currentTeamId),
                        ]);
                        if (window.lucide) lucide.createIcons();
                    }
                } catch (error) {
                    console.error('Failed to delete override:', error);
                    showToast('Failed to remove override: ' + error.message, 'error');
                }
            }
            return;
        }

        // Delete Override button (modal list + inline X in on-call widget rows)
        const inlineDeleteBtn = e.target.closest('.delete-override-btn');
        if (inlineDeleteBtn) {
            e.preventDefault();
            e.stopPropagation();

            const scheduleId = inlineDeleteBtn.dataset.scheduleId;
            const overrideId = inlineDeleteBtn.dataset.overrideId;
            const modalTeamId = document.querySelector('#override-form')?.dataset.teamId;

            if (!confirm('Remove this override?')) return;

            try {
                await API.schedules.deleteOverride(scheduleId, overrideId);
                showToast('Override removed', 'success');

                const widget = inlineDeleteBtn.closest('.oncall-row') || inlineDeleteBtn.closest('.on-call-widget');
                const teamId = widget?.dataset.teamId || modalTeamId;
                if (teamId) {
                    await refreshOnCallUI(teamId);
                }
            } catch (error) {
                console.error('Failed to delete override:', error);
                showToast('Failed to remove override: ' + error.message, 'error');
            }
            return;
        }
    });
}
