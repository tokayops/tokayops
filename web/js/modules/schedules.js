/**
 * TokayOps Schedules Module
 *
 * The editor over the schedule revision model.
 *
 * Three things shape this file. Configuration is saved whole and once, against
 * the version it was loaded at, so a save either lands completely or is
 * refused. Group identity is carried in the DOM, because the server tells "this
 * group gained a member" from "the groups were replaced" by comparing IDs, and
 * losing them across a reorder would restart the rotation. And a save is
 * previewed before it is made, because who is on duty is the kind of thing
 * people should see change before it changes.
 */

import { showToast, escapeHtml } from '/js/core/utils.js';
import { State } from '/js/core/state.js';
import { initTimezonePicker } from '/js/core/timezone-picker.js';
import { resolveNames, assignableMembers, invalidateNames } from '/js/core/users-directory.js';
import { resolveLocalTime, resolveWindow, gapMessage, instantToLocalInput } from '/js/core/zoned-time.js';

// ========================================
// State
// ========================================

let currentTeamId = null;
let teamMembers = [];
let editingOverrideId = null;
let editingOverrideRevision = null;
let editingScheduleId = null;
let returnToCalendar = false;
let currentOverrideTz = null;

// What the editor loaded. expected_version comes from here and nowhere else:
// the preview reports the version it evaluated against, and taking that would
// be pinning the save to whatever was seen last rather than to what was
// edited - the exact class of stale write the version exists to refuse.
let loadedConfig = null;

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
 * @param {Error} error - carries status, code and body
 * @param {Object} handlers - code -> handler, plus optional `fallback`
 * @returns {boolean} whether a handler ran
 */
function onScheduleError(error, handlers = {}) {
    const handler = handlers[error?.code];
    if (handler) {
        handler(error);
        return true;
    }
    // An unrecognised code still has a server message. Inventing an
    // interpretation for it would be worse than passing it through.
    showToast(error?.message || 'Request failed', 'error');
    return false;
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

// ========================================
// Reading the current state of a team
// ========================================

/**
 * Everything the widgets need about a team's schedule, in one request.
 *
 * The on-call endpoint reports whether a schedule exists and whether it was
 * deleted alongside the projection, so this does not also have to ask for the
 * configuration - a request whose ordinary answer is 404, once per team on
 * every page that lists them.
 */
async function loadTeamOnCall(teamId) {
    const response = await API.schedules.currentOnCall(teamId).catch(() => null);

    const onCall = response?.on_call || null;
    const names = await resolveNames([
        ...(onCall?.l1?.user_ids || []),
        ...(onCall?.l2?.user_ids || []),
    ]);

    return {
        onCall,
        // Not derived from the projection: a team with no schedule, a deleted
        // one and a live one between shifts all put nobody on duty, and the
        // widget has to offer "Configure" for the first two and not the third.
        configured: !!response?.schedule_id && !response?.deleted_at,
        scheduleId: response?.schedule_id || '',
        names,
    };
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
        const state = await loadTeamOnCall(teamId);
        container.innerHTML = Components.onCallWidget(state.onCall, {
            teamId,
            scheduleId: state.scheduleId,
            configured: state.configured,
            names: state.names,
        });
        if (window.lucide) lucide.createIcons();
    } catch (error) {
        console.error('Failed to load on-call widget:', error);
    }
}

async function refreshOnCallUI(teamId) {
    if (!teamId) return;
    try {
        const state = await loadTeamOnCall(teamId);

        const safeId = typeof CSS !== 'undefined' && CSS.escape ? CSS.escape(teamId) : teamId;
        const widgets = document.querySelectorAll(`.oncall-row[data-team-id="${safeId}"], .on-call-widget[data-team-id="${safeId}"]`);
        const team = State.teams.find(t => t.id === teamId) || { id: teamId, name: teamId };
        const ctx = {
            teamId,
            scheduleId: state.scheduleId,
            configured: state.configured,
            names: state.names,
        };

        if (widgets.length > 0) {
            widgets.forEach(widget => {
                const isOverview = widget.classList.contains('oncall-row')
                    || widget.closest('.on-call-overview');
                widget.outerHTML = isOverview
                    ? Components.onCallOverviewRow(state.onCall, team, ctx)
                    : Components.onCallWidget(state.onCall, ctx);
            });
        } else {
            const rowSlot = document.querySelector(`.oncall-row-slot[data-team-id="${safeId}"]`);
            if (rowSlot) {
                rowSlot.innerHTML = Components.onCallOverviewRow(state.onCall, team, ctx);
            }
        }

        if (window.lucide) lucide.createIcons();

        // Update overrides list if the override modal is open for this team
        const modalContent = document.getElementById('modal-body');
        const overrideForm = modalContent?.querySelector('#override-form');
        if (overrideForm && overrideForm.dataset.teamId === teamId) {
            await refreshOverridesList(teamId, state.scheduleId);
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
        const state = await loadTeamOnCall(team.id);
        container.innerHTML = Components.onCallOverviewRow(state.onCall, team, {
            teamId: team.id,
            scheduleId: state.scheduleId,
            configured: state.configured,
            names: state.names,
        });
        if (window.lucide) lucide.createIcons();
    } catch (error) {
        console.error('Failed to load on-call overview row:', error);
        container.innerHTML = Components.onCallOverviewRow(null, team, { teamId: team.id });
        if (window.lucide) lucide.createIcons();
    }
}

// ========================================
// Schedule Config Modal
// ========================================

async function openScheduleConfigModal(teamId) {
    currentTeamId = teamId;

    try {
        // Names are refreshed rather than reused here: the editor is where
        // someone reads a list of people and decides who covers what, so it is
        // the wrong place to show a name that was removed a minute ago.
        invalidateNames();

        const [members, config] = await Promise.all([
            assignableMembers(teamId),
            API.schedules.getConfig(teamId).catch(error => {
                if (error?.status === 404) return null;
                throw error;
            }),
        ]);

        teamMembers = members;
        loadedConfig = config;

        const mentioned = [
            ...(config?.config?.l1?.groups || []).flatMap(g => g.user_ids || []),
            ...(config?.config?.l2?.user_ids || []),
        ];
        const names = await resolveNames(mentioned);
        for (const member of members) names.set(member.id, member.name);

        const modal = document.getElementById('modal-overlay');
        const modalTitle = document.getElementById('modal-title');
        const modalContent = document.getElementById('modal-body');
        const modalFooter = document.getElementById('modal-footer');

        modalTitle.textContent = config?.deleted_at
            ? 'Recreate On-Call Schedule'
            : 'Configure On-Call Schedule';
        modalContent.innerHTML = Components.scheduleConfigModal({
            config: config?.config || null,
            members,
            names,
            version: config?.version || 0,
            deletedAt: config?.deleted_at || null,
        }, teamId);
        modalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="schedule-cancel">Cancel</button>
            <button type="submit" form="schedule-form" class="btn btn-primary" id="schedule-form-submit">
                Review changes
            </button>
        `;

        modal.classList.add('active');
        document.body.style.overflow = 'hidden';
        if (window.lucide) lucide.createIcons();

        bindScheduleFormEvents();

        initTimezonePicker('schedule-timezone', {
            selected: config?.config?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone,
        });
    } catch (error) {
        console.error('Failed to open schedule modal:', error);
        onScheduleError(error, {
            legacy_schedule: () => showToast(
                'This schedule predates the current data model and cannot be edited until the ' +
                'upgrade reset has been run.', 'error'),
        });
    }
}

function bindScheduleFormEvents() {
    const form = document.getElementById('schedule-form');
    if (!form) return;

    initGroupsEditor('l1-groups-editor', 'l1-add-group');
    initSortableLists('l2-available', 'l2-users-list');

    // Cadence toggles. The handoff day is meaningless for a daily rotation and
    // the server canonicalizes it away, so the field is hidden rather than
    // sent as a value that will not survive.
    const bindCadence = (selectId, weeklyClass) => {
        const select = document.getElementById(selectId);
        const weeklyOnly = document.querySelector(weeklyClass);
        if (!select || !weeklyOnly) return;
        select.addEventListener('change', () => {
            weeklyOnly.style.display = select.value === 'weekly' ? '' : 'none';
        });
    };
    bindCadence('l1-rotation-type', '.l1-weekly-only');
    bindCadence('l2-rotation-type', '.l2-weekly-only');

    const l2Enabled = document.getElementById('l2-enabled');
    const l2Config = document.querySelector('.l2-config');
    const l2Panel = document.querySelector('.l2-users-panel');
    if (l2Enabled) {
        l2Enabled.addEventListener('change', () => {
            if (l2Config) l2Config.style.display = l2Enabled.checked ? '' : 'none';
            if (l2Panel) l2Panel.style.display = l2Enabled.checked ? '' : 'none';
        });
    }

    document.getElementById('schedule-cancel')?.addEventListener('click', closeModal);

    form.addEventListener('submit', handleScheduleSubmit);

    const deleteScheduleBtn = document.querySelector('.delete-schedule-btn');
    if (deleteScheduleBtn) {
        deleteScheduleBtn.addEventListener('click', () => handleScheduleDelete(deleteScheduleBtn.dataset.teamId));
    }
}

function closeModal() {
    document.getElementById('modal-overlay').classList.remove('active');
    document.body.style.overflow = '';
}

/**
 * Initialize SortableJS for a pair of available/selected lists
 */
function initSortableLists(availableId, selectedId) {
    const availableList = document.getElementById(availableId);
    const selectedList = document.getElementById(selectedId);

    if (!availableList || !selectedList || typeof Sortable === 'undefined') return;

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

    new Sortable(selectedList, {
        group: availableId.replace('-available', '-rotation'),
        animation: 150,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        handle: '.drag-handle',
        onAdd: (evt) => {
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
 * The L1 groups editor: reorder, add and remove groups, and move people
 * between them.
 *
 * Every row owns a stable ID. Existing rows keep the one the server gave them;
 * a new row gets a fresh UUID here rather than being numbered by position,
 * because position is exactly what reordering changes. Renumbering only
 * rewrites the visible label - the row element, and with it the ID, travels
 * with the drag.
 */
function initGroupsEditor(editorId, addGroupBtnId) {
    const editor = document.getElementById(editorId);
    const addGroupBtn = document.getElementById(addGroupBtnId);
    if (!editor) return;

    const buildOptions = () => {
        const opts = ['<option value="">+ Add user</option>'];
        for (const u of teamMembers) {
            opts.push(`<option value="${escapeHtml(u.id)}">${escapeHtml(u.name)}</option>`);
        }
        return opts.join('');
    };

    const renumberGroups = () => {
        editor.querySelectorAll('.group-row').forEach((row, i) => {
            const label = row.querySelector('.group-label');
            if (label) label.textContent = `Group ${i + 1}`;
        });
    };

    const createGroupRow = (index) => {
        const row = document.createElement('div');
        row.className = 'group-row';
        row.dataset.groupId = crypto.randomUUID();
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

    if (addGroupBtn) {
        addGroupBtn.addEventListener('click', () => {
            const index = editor.querySelectorAll('.group-row').length;
            const row = createGroupRow(index);
            editor.appendChild(row);
            if (window.lucide) lucide.createIcons();
        });
    }

    editor.addEventListener('click', (e) => {
        const chipRemove = e.target.closest('.chip-remove');
        if (chipRemove) {
            chipRemove.closest('.user-chip')?.remove();
            clearGroupErrors(editor);
            return;
        }
        const groupDelete = e.target.closest('.group-delete');
        if (groupDelete) {
            groupDelete.closest('.group-row')?.remove();
            renumberGroups();
            clearGroupErrors(editor);
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

        if (chips.querySelector(`.user-chip[data-user-id="${CSS.escape(userId)}"]`)) {
            select.value = '';
            return;
        }

        const optionText = select.options[select.selectedIndex]?.text || userId;
        const chip = document.createElement('span');
        chip.className = 'user-chip';
        chip.dataset.userId = userId;
        chip.innerHTML = `${escapeHtml(optionText)}<button type="button" class="chip-remove" aria-label="Remove">×</button>`;
        chips.appendChild(chip);
        select.value = '';
        clearGroupErrors(editor);
    });
}

function clearGroupErrors(editor) {
    editor.querySelectorAll('.group-row.has-error').forEach(row => row.classList.remove('has-error'));
}

/**
 * Read the form into the configuration the API takes.
 *
 * The generated parts of a schedule - where the rotation's phase is anchored,
 * which group is up first - are absent by design. They are the server's to
 * derive; sending them back would let the editor pin a rotation to whatever it
 * last saw.
 */
function collectConfig() {
    const l1Groups = [];
    document.querySelectorAll('#l1-groups-editor .group-row').forEach(row => {
        const userIds = [];
        row.querySelectorAll('.user-chip').forEach(chip => {
            if (chip.dataset.userId) userIds.push(chip.dataset.userId);
        });
        l1Groups.push({ id: row.dataset.groupId, user_ids: userIds });
    });

    const l2UserIds = [];
    document.querySelectorAll('#l2-users-list .rotation-user').forEach(item => {
        l2UserIds.push(item.dataset.userId);
    });

    const l1Type = document.getElementById('l1-rotation-type')?.value || 'daily';
    const l2Type = document.getElementById('l2-rotation-type')?.value || 'weekly';
    const dayOf = (id) => {
        const value = parseInt(document.getElementById(id)?.value, 10);
        return Number.isNaN(value) ? 1 : value;
    };

    return {
        timezone: document.querySelector('#schedule-timezone input[type=hidden]')?.value || 'UTC',
        slack_usergroup_id: document.getElementById('slack-usergroup-id')?.value || '',
        l1: {
            enabled: true,
            rotation_type: l1Type,
            handoff_time: document.getElementById('l1-handoff-time')?.value || '11:00',
            handoff_day: l1Type === 'weekly' ? dayOf('l1-handoff-day') : null,
            groups: l1Groups,
        },
        // The L2 policy is sent whether or not the layer is on. The server
        // validates both layers regardless, so a disabled layer still needs a
        // cadence and a handoff time that parse.
        l2: {
            enabled: document.getElementById('l2-enabled')?.checked || false,
            escalation_timeout_minutes: parseInt(document.getElementById('l2-escalation-timeout')?.value, 10) || 5,
            rotation_type: l2Type,
            handoff_time: document.getElementById('l2-handoff-time')?.value || '11:00',
            handoff_day: l2Type === 'weekly' ? dayOf('l2-handoff-day') : null,
            user_ids: l2UserIds,
        },
    };
}

/**
 * Refuse a configuration the server would refuse, before previewing it.
 *
 * An empty group is not dropped silently: someone added that row on purpose
 * and is about to fill it. Removing it for them would be a different
 * configuration than the one on screen.
 */
function validateConfig(config) {
    const editor = document.getElementById('l1-groups-editor');
    if (editor) clearGroupErrors(editor);

    const emptyRows = [];
    config.l1.groups.forEach((group, index) => {
        if ((group.user_ids || []).length === 0) emptyRows.push(index);
    });

    if (emptyRows.length > 0) {
        const rows = editor?.querySelectorAll('.group-row') || [];
        emptyRows.forEach(index => rows[index]?.classList.add('has-error'));
        return emptyRows.length === 1
            ? `Group ${emptyRows[0] + 1} has nobody in it. Add someone, or remove the group.`
            : `Groups ${emptyRows.map(i => i + 1).join(', ')} have nobody in them. Add someone, or remove them.`;
    }
    return null;
}

// ========================================
// Preview -> Confirm
// ========================================

/**
 * Whether two projections put the same people on duty.
 *
 * Mirrors what the server compares. Provenance is deliberately left out: every
 * save produces a new revision and a new assignment start, so comparing those
 * would report "this differs from the preview" for every save, including the
 * ones that changed nothing about who is on call.
 */
function sameDuty(a, b) {
    const sameLayer = (x, y) => {
        if (!x || !y) return !x && !y;
        if (x.source !== y.source) return false;
        const left = [...(x.user_ids || [])].sort();
        const right = [...(y.user_ids || [])].sort();
        return left.length === right.length && left.every((id, i) => id === right[i]);
    };
    return sameLayer(a?.l1, b?.l1) && sameLayer(a?.l2, b?.l2);
}

async function handleScheduleSubmit(e) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;
    const expectedVersion = parseInt(form.dataset.expectedVersion, 10) || 0;
    const config = collectConfig();

    const invalid = validateConfig(config);
    if (invalid) {
        showToast(invalid, 'error');
        return;
    }

    const submitBtn = document.getElementById('schedule-form-submit');
    if (submitBtn) submitBtn.disabled = true;

    try {
        const until = new Date();
        until.setDate(until.getDate() + 14);
        const preview = await API.schedules.preview(teamId, config, until);

        // The preview evaluated against a version; if it is not the one being
        // edited, someone else has saved in the meantime and this form is
        // describing a schedule that no longer exists.
        if (preview.base_version !== expectedVersion) {
            showToast(
                'Someone else saved this schedule while you were editing. Reopen it to see their changes.',
                'error');
            return;
        }

        await showPreviewStep(teamId, config, preview, expectedVersion);
    } catch (error) {
        console.error('Failed to preview schedule:', error);
        handleSaveError(error, teamId);
    } finally {
        if (submitBtn) submitBtn.disabled = false;
    }
}

async function showPreviewStep(teamId, config, preview, expectedVersion) {
    const modalTitle = document.getElementById('modal-title');
    const modalContent = document.getElementById('modal-body');
    const modalFooter = document.getElementById('modal-footer');

    const ids = [
        ...(preview.on_call_before?.l1?.user_ids || []),
        ...(preview.on_call_after?.l1?.user_ids || []),
        ...(preview.entries || []).flatMap(e => e.user_ids || []),
    ];
    const names = await resolveNames(ids);

    const previousTitle = modalTitle.textContent;
    const previousBody = modalContent.innerHTML;
    const previousFooter = modalFooter.innerHTML;

    modalTitle.textContent = 'Review changes';
    modalContent.innerHTML = Components.schedulePreview(preview, names, config.timezone);
    modalFooter.innerHTML = `
        <button type="button" class="btn btn-secondary" id="preview-back">Back to editing</button>
        <button type="button" class="btn btn-primary" id="preview-confirm">Save schedule</button>
    `;
    if (window.lucide) lucide.createIcons();

    // Going back restores the form as it was, with its group IDs intact -
    // re-rendering it from the loaded configuration would silently discard
    // everything typed since.
    document.getElementById('preview-back')?.addEventListener('click', () => {
        modalTitle.textContent = previousTitle;
        modalContent.innerHTML = previousBody;
        modalFooter.innerHTML = previousFooter;
        if (window.lucide) lucide.createIcons();
        bindScheduleFormEvents();
        initTimezonePicker('schedule-timezone', { selected: config.timezone });
    });

    document.getElementById('preview-confirm')?.addEventListener('click', async () => {
        const confirmBtn = document.getElementById('preview-confirm');
        if (confirmBtn) confirmBtn.disabled = true;
        try {
            const reason = document.getElementById('schedule-reason')?.value || '';
            const saved = await API.schedules.saveConfig(teamId, config, expectedVersion, reason);

            if (saved.noop) {
                showToast('No changes to save', 'info');
            } else if (saved.created) {
                showToast('Schedule created', 'success');
            } else if (saved.recreated) {
                showToast('Schedule recreated', 'success');
            } else {
                showToast('Schedule saved', 'success');
            }

            // The save recalculated under a lock. If that produced a different
            // duty than the preview showed, say so: the person approved a
            // specific outcome, and quietly delivering another one is how
            // trust in the preview is lost.
            if (!saved.noop && !sameDuty(preview.on_call_after, saved.on_call_after)) {
                const actual = Components.onCallNames(saved.on_call_after?.l1, names) || 'nobody';
                showToast(
                    `Saved, but on-call differs from the preview: ${actual} is on duty now.`,
                    'warning');
            }

            closeModal();
            await refreshOnCallUI(teamId);
        } catch (error) {
            console.error('Failed to save schedule:', error);
            handleSaveError(error, teamId);
        } finally {
            if (confirmBtn) confirmBtn.disabled = false;
        }
    });
}

function handleSaveError(error, teamId) {
    onScheduleError(error, {
        schedule_version_conflict: () => {
            showToast('Someone else changed this schedule. Reopening it with their changes.', 'error');
            closeModal();
            openScheduleConfigModal(teamId);
        },
        schedule_deleted: () => {
            showToast('This schedule was deleted. Reopening it so you can recreate it.', 'error');
            closeModal();
            openScheduleConfigModal(teamId);
        },
        schedule_exists: () => {
            showToast('This team already has a schedule. Reopening it.', 'error');
            closeModal();
            openScheduleConfigModal(teamId);
        },
        legacy_schedule: () => showToast(
            'This schedule predates the current data model. It has to be reset as part of the ' +
            'upgrade before it can be edited.', 'error'),
        user_not_team_member: (err) => {
            const ids = err.body?.user_ids || [];
            showToast(
                ids.length > 0
                    ? `These people are not members of this team: ${ids.join(', ')}`
                    : 'Some people in this rotation are not members of this team.',
                'error');
        },
        validation_failed: (err) => {
            const field = err.body?.field;
            showToast(field ? `${field}: ${err.message}` : err.message, 'error');
        },
    });
}

async function handleScheduleDelete(teamId) {
    if (!confirm('Delete this schedule? The rotation stops and its overrides are cleared. ' +
        'Past shifts stay in the calendar, and you can recreate it later.')) {
        return;
    }
    try {
        const version = parseInt(
            document.getElementById('schedule-form')?.dataset.expectedVersion, 10) || 0;
        await API.schedules.deleteSchedule(teamId, version);
        showToast('Schedule deleted', 'success');
        closeModal();
        await refreshOnCallUI(teamId);
    } catch (error) {
        console.error('Failed to delete schedule:', error);
        onScheduleError(error, {
            schedule_version_conflict: () => {
                showToast('Someone else changed this schedule. Reopening it.', 'error');
                closeModal();
                openScheduleConfigModal(teamId);
            },
            schedule_deleted: () => {
                showToast('This schedule was already deleted.', 'info');
                closeModal();
            },
        });
    }
}

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

function hideContextMenu() {
    if (overrideContextMenu) {
        overrideContextMenu.classList.remove('active');
    }
}

document.addEventListener('click', (e) => {
    if (overrideContextMenu &&
        !e.target.closest('.override-context-menu') &&
        !e.target.closest('.calendar-entry.layer-override')) {
        hideContextMenu();
    }
});
document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;

    if (overrideContextMenu && overrideContextMenu.classList.contains('active')) {
        e.stopImmediatePropagation();
        hideContextMenu();
        return;
    }

    if (returnToCalendar && currentTeamId) {
        e.stopImmediatePropagation();
        e.preventDefault();
        document.getElementById('override-cancel')?.click();
    }
});
document.addEventListener('scroll', () => hideContextMenu(), true);

// Intercept X button and overlay-background clicks when returnToCalendar is
// active - redirect to cancel (which returns to the calendar) instead of
// closing the modal.
document.addEventListener('click', (e) => {
    if (!returnToCalendar || !currentTeamId) return;

    const isCloseBtn = e.target.closest('#modal-close');
    const isOverlayBg = e.target.id === 'modal-overlay';

    if (isCloseBtn || isOverlayBg) {
        e.stopPropagation();
        e.preventDefault();
        document.getElementById('override-cancel')?.click();
    }
}, true);

// ========================================
// Override Modal
// ========================================

function populateOverrideEditForm(data) {
    editingOverrideId = data.overrideId;
    editingOverrideRevision = data.revision !== undefined ? parseInt(data.revision, 10) : null;
    editingScheduleId = data.scheduleId;

    const userSelect = document.getElementById('override-user');
    if (userSelect) userSelect.value = data.userId;

    const tz = currentOverrideTz || Intl.DateTimeFormat().resolvedOptions().timeZone;
    const startInput = document.getElementById('override-start');
    const endInput = document.getElementById('override-end');
    if (startInput) startInput.value = instantToLocalInput(data.validFrom, tz);
    if (endInput) endInput.value = instantToLocalInput(data.validTo, tz);

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

async function loadOverrideState(teamId) {
    const [members, config, overrideList] = await Promise.all([
        assignableMembers(teamId),
        API.schedules.getConfig(teamId).catch(() => null),
        API.schedules.listOverrides(teamId).catch(() => ({ overrides: [] })),
    ]);

    const overrides = overrideList?.overrides || [];
    const names = await resolveNames(overrides.map(o => o.user_id));
    for (const member of members) names.set(member.id, member.name);

    return { members, overrides, names, scheduleId: config?.schedule_id || '' };
}

async function refreshOverridesList(teamId, scheduleId) {
    const modalContent = document.getElementById('modal-body');
    const contentWrapper = modalContent?.querySelector('.override-modal-content');
    if (!contentWrapper) return;

    const overrideList = await API.schedules.listOverrides(teamId).catch(() => ({ overrides: [] }));
    const overrides = overrideList?.overrides || [];
    const names = await resolveNames(overrides.map(o => o.user_id));

    const html = Components.overridesList(overrides, scheduleId, names);
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
 * @returns {Object|null} the state it rendered, so a caller that needs the
 * override heads does not fetch them a second time.
 */
async function openOverrideModal(teamId) {
    currentTeamId = teamId;

    try {
        const state = await loadOverrideState(teamId);
        teamMembers = state.members;

        const modal = document.getElementById('modal-overlay');
        const modalTitle = document.getElementById('modal-title');
        const modalContent = document.getElementById('modal-body');
        const modalFooter = document.getElementById('modal-footer');

        // Reset edit state. returnToCalendar is intentionally preserved: it is
        // set before this runs, by the calendar edit flow.
        editingOverrideId = null;
        editingOverrideRevision = null;
        editingScheduleId = null;

        modalTitle.textContent = 'Manage Overrides';
        modalContent.innerHTML = Components.overrideModal(state, teamId);
        modalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="override-cancel">Cancel</button>
            <button type="submit" form="override-form" class="btn btn-primary">Create Override</button>
        `;

        modal.classList.add('active');
        document.body.style.overflow = 'hidden';
        if (window.lucide) lucide.createIcons();

        const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        initTimezonePicker('override-timezone', {
            selected: browserTz,
            onChange: (newTz) => {
                // The typed wall-clock time is kept and reinterpreted in the
                // new zone. Converting the instant instead would silently move
                // the times someone just entered.
                currentOverrideTz = newTz;
                describeOverrideTimes();
            }
        });
        currentOverrideTz = browserTz;

        bindOverrideFormEvents();
        return state;
    } catch (error) {
        console.error('Failed to open override modal:', error);
        showToast('Failed to load overrides: ' + error.message, 'error');
        return null;
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
            closeModal();
        }
    });

    for (const id of ['override-start', 'override-end']) {
        document.getElementById(id)?.addEventListener('change', describeOverrideTimes);
    }

    form.addEventListener('submit', handleOverrideSubmit);
}

/**
 * Say what the entered times resolve to, when it is not obvious.
 *
 * Most of the year this stays silent. On a daylight-saving boundary a local
 * time can fail to exist or happen twice, and an override is recorded as an
 * instant - so the moment it is ambiguous, which one was chosen has to be
 * visible rather than decided quietly.
 */
function describeOverrideTimes() {
    const note = document.getElementById('override-time-note');
    if (!note) return;

    const tz = currentOverrideTz || 'UTC';
    const startValue = document.getElementById('override-start')?.value;
    const endValue = document.getElementById('override-end')?.value;
    if (!startValue || !endValue) {
        note.innerHTML = '';
        return;
    }

    const { from, to } = resolveWindow(startValue, endValue, tz);
    const messages = [];

    if (from.kind === 'gap') messages.push({ level: 'error', text: `Start: ${gapMessage(tz)}` });
    if (to.kind === 'gap') messages.push({ level: 'error', text: `End: ${gapMessage(tz)}` });
    if (from.kind === 'ambiguous') {
        messages.push({
            level: 'info',
            text: `Start happens twice in ${tz}; using the first (${from.offsetLabel}).`,
        });
    }
    if (to.kind === 'ambiguous') {
        messages.push({
            level: 'info',
            text: `End happens twice in ${tz}; using the second (${to.offsetLabel}), so the cover is not cut short.`,
        });
    }

    note.innerHTML = messages.map(m =>
        `<div class="override-time-note-line is-${m.level}">${escapeHtml(m.text)}</div>`).join('');
}

async function handleOverrideSubmit(e) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;
    const scheduleId = editingScheduleId || form.dataset.scheduleId;

    const timezone = currentOverrideTz
        || document.querySelector('#override-timezone input[type=hidden]')?.value
        || 'UTC';
    const startValue = document.getElementById('override-start')?.value;
    const endValue = document.getElementById('override-end')?.value;
    const userId = document.getElementById('override-user')?.value;

    if (!userId) {
        showToast('Please select a user', 'error');
        return;
    }

    const { from, to } = resolveWindow(startValue, endValue, timezone);
    if (from.kind === 'gap' || to.kind === 'gap' || !from.instant || !to.instant) {
        describeOverrideTimes();
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
        if (editingOverrideId) {
            await API.schedules.updateOverride(
                scheduleId, editingOverrideId, overrideData, editingOverrideRevision);
            showToast('Override updated', 'success');
        } else {
            await API.schedules.createOverride(teamId, overrideData);
            showToast('Override created', 'success');
        }

        const shouldReturnToCalendar = returnToCalendar;
        resetOverrideForm();

        if (shouldReturnToCalendar && currentTeamId) {
            await Promise.all([
                openViewScheduleModal(currentTeamId),
                refreshOnCallUI(teamId),
            ]);
        } else {
            closeModal();
            await refreshOnCallUI(teamId);
        }
    } catch (error) {
        console.error('Failed to save override:', error);
        handleOverrideError(error, teamId, scheduleId);
    }
}

function handleOverrideError(error, teamId, scheduleId) {
    onScheduleError(error, {
        override_overlap: (err) => showToast(
            `This override was not saved. ${conflictingOverridesText(err)}`, 'error'),
        override_revision_conflict: async () => {
            showToast('Someone else changed this override. Reloading the list.', 'error');
            resetOverrideForm();
            await refreshOverridesList(teamId, scheduleId);
        },
        override_not_found: async () => {
            showToast('This override no longer exists.', 'error');
            resetOverrideForm();
            await refreshOverridesList(teamId, scheduleId);
        },
        schedule_deleted: () => showToast(
            'This schedule was deleted, so it has no overrides to change.', 'error'),
        user_not_team_member: (err) => {
            const ids = err.body?.user_ids || [];
            showToast(`Not a member of this team: ${ids.join(', ')}`, 'error');
        },
    });
}

function resetOverrideForm() {
    editingOverrideId = null;
    editingOverrideRevision = null;
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

let currentViewTimezone = null;

async function renderCalendarView(teamId, timezone) {
    const modalContent = document.getElementById('modal-body');

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

        const render = await API.schedules.render(teamId, now, until);
        const names = await resolveNames((render.entries || []).flatMap(e => e.user_ids || []));
        const activeTz = timezone || 'UTC';

        modalContent.innerHTML = Components.monthlyScheduleCalendar(render, now, activeTz, names);

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
                <p style="font-size: 0.8em; color: var(--text-muted);">${escapeHtml(error.message)}</p>
            </div>
        `;
    }

    if (window.lucide) lucide.createIcons();
}

async function openViewScheduleModal(teamId) {
    currentTeamId = teamId;
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

    // Bound before the calendar is fetched, not after. The button is on screen
    // as soon as the footer is written, and a button that is visible but inert
    // for as long as a request takes is a button that gets clicked twice.
    document.getElementById('calendar-close')?.addEventListener('click', closeModal);

    await renderCalendarView(teamId, currentViewTimezone);
}

// ========================================
// Deleting an override
// ========================================

async function deleteOverride(scheduleId, overrideId, revision, teamId) {
    try {
        await API.schedules.deleteOverride(scheduleId, overrideId, revision);
        showToast('Override removed', 'success');
        return true;
    } catch (error) {
        console.error('Failed to delete override:', error);
        handleOverrideError(error, teamId, scheduleId);
        return false;
    }
}

/**
 * The revision an override is currently at.
 *
 * A delete has to name the revision it is deleting, and the list of override
 * heads is the only place that number comes from. The calendar draws shifts,
 * not override heads, so an override reached from there is looked up rather
 * than guessed at.
 */
async function currentOverrideRevision(teamId, overrideId) {
    const list = await API.schedules.listOverrides(teamId).catch(() => ({ overrides: [] }));
    const head = (list?.overrides || []).find(o => o.override_id === overrideId);
    return head ? head : null;
}

// ========================================
// Event Bindings
// ========================================

export function bindScheduleEvents() {
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
                revision: editOverrideBtn.dataset.revision,
                scheduleId: editOverrideBtn.dataset.scheduleId,
                userId: editOverrideBtn.dataset.userId,
                validFrom: editOverrideBtn.dataset.validFrom,
                validTo: editOverrideBtn.dataset.validTo,
                reason: editOverrideBtn.dataset.reason,
            });
            return;
        }

        // Calendar override entry click -> show context menu
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
            const overrideId = menu.dataset.overrideId;
            hideContextMenu();

            if (!currentTeamId || !overrideId) return;

            if (action === 'edit') {
                // The modal loads the override heads and the schedule ID on
                // its way in, so the form is filled from what it already has.
                // A calendar entry is a shift: it knows which override it came
                // from, but not the revision to edit against.
                returnToCalendar = true;
                const state = await openOverrideModal(currentTeamId);
                const head = (state?.overrides || []).find(o => o.override_id === overrideId);
                if (!head) {
                    showToast('This override no longer exists.', 'error');
                    returnToCalendar = false;
                    await openViewScheduleModal(currentTeamId);
                    return;
                }
                populateOverrideEditForm({
                    overrideId: head.override_id,
                    revision: head.revision,
                    scheduleId: state.scheduleId,
                    userId: head.user_id,
                    validFrom: head.valid_from,
                    validTo: head.valid_to,
                    reason: head.reason,
                });
            }

            if (action === 'delete') {
                const head = await currentOverrideRevision(currentTeamId, overrideId);
                if (!head) {
                    showToast('This override no longer exists.', 'error');
                    await renderCalendarView(currentTeamId, currentViewTimezone);
                    return;
                }
                if (!confirm('Remove this override?')) return;
                const config = await API.schedules.getConfig(currentTeamId).catch(() => null);
                const removed = await deleteOverride(
                    config?.schedule_id || '', head.override_id, head.revision, currentTeamId);
                if (removed && currentViewTimezone) {
                    await Promise.all([
                        renderCalendarView(currentTeamId, currentViewTimezone),
                        refreshOnCallUI(currentTeamId),
                    ]);
                    if (window.lucide) lucide.createIcons();
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
            const widget = inlineDeleteBtn.closest('.oncall-row') || inlineDeleteBtn.closest('.on-call-widget');
            const teamId = widget?.dataset.teamId
                || document.querySelector('#override-form')?.dataset.teamId
                || currentTeamId;

            if (!confirm('Remove this override?')) return;

            // The widget knows which override is in force but not which
            // revision it is at, so the head is read before deleting.
            let revision = parseInt(inlineDeleteBtn.dataset.revision, 10);
            if (Number.isNaN(revision)) {
                const head = await currentOverrideRevision(teamId, overrideId);
                if (!head) {
                    showToast('This override no longer exists.', 'error');
                    await refreshOnCallUI(teamId);
                    return;
                }
                revision = head.revision;
            }

            const removed = await deleteOverride(scheduleId, overrideId, revision, teamId);
            if (removed && teamId) {
                await refreshOnCallUI(teamId);
            }
            return;
        }
    });
}
