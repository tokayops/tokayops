/**
 * The schedule configuration editor, and the preview that stands between it
 * and a save.
 *
 * Three things shape this module. Configuration is saved whole and once,
 * against the version it was loaded at, so a save either lands completely or
 * is refused. Group identity is carried in the DOM, because the server tells
 * "this group gained a member" from "the groups were replaced" by comparing
 * IDs, and losing them across a reorder would restart the rotation. And a save
 * is previewed before it is made, because who is on duty is the kind of thing
 * people should see change before it changes.
 *
 * Everything the editor knows lives inside one open of it. The form is not the
 * only way out - a version conflict reopens it, a delete closes it - and state
 * kept beside the module rather than inside the open is what let one editor
 * finish into another one's modal.
 */

import { showToast, escapeHtml, escapeAttr, openModal } from '/js/core/utils.js';
import { beginModalSession, modalShell } from '/js/core/modal-session.js';
import { initTimezonePicker } from '/js/core/timezone-picker.js';
import { resolveNames, assignableMembers, invalidateNames } from '/js/core/users-directory.js';
import { onScheduleError } from '/js/modules/schedule-errors.js';
import { scheduleConfigModal, schedulePreview } from '/js/modules/schedule-components.js';

/**
 * Open the editor for a team.
 *
 * The session is created first, before anything is awaited: a request started
 * here can outlive the modal, and when it does its answer has to land nowhere
 * rather than in whatever modal took this one's place.
 *
 * @param {string} teamId
 * @param {Object} [options]
 * @param {(teamId: string) => void} [options.onChanged] - the schedule was saved or deleted
 */
export async function openScheduleEditor(teamId, options = {}) {
    const session = beginModalSession();
    await renderEditor(session, teamId, options);
    return session;
}

async function renderEditor(session, teamId, options) {
    try {
        // Names are refreshed rather than reused here: the editor is where
        // someone reads a list of people and decides who covers what, so it is
        // the wrong place to show a name that was removed a minute ago.
        invalidateNames();

        const [members, config] = await Promise.all([
            assignableMembers(teamId, { signal: session.signal }),
            API.schedules.getConfig(teamId, { signal: session.signal }).catch(error => {
                if (error?.status === 404) return null;
                throw error;
            }),
        ]);
        if (session.closed) return;

        const mentioned = [
            ...(config?.config?.l1?.groups || []).flatMap(g => g.user_ids || []),
            ...(config?.config?.l2?.user_ids || []),
        ];
        const names = await resolveNames(mentioned);
        if (session.closed) return;
        for (const member of members) names.set(member.id, member.name);

        const { title, body, footer } = modalShell();

        title.textContent = config?.deleted_at
            ? 'Recreate On-Call Schedule'
            : 'Configure On-Call Schedule';
        body.innerHTML = scheduleConfigModal({
            config: config?.config || null,
            members,
            names,
            version: config?.version || 0,
            deletedAt: config?.deleted_at || null,
        }, teamId);
        footer.innerHTML = `
            <button type="button" class="btn btn-secondary" id="schedule-cancel">Cancel</button>
            <button type="submit" form="schedule-form" class="btn btn-primary" id="schedule-form-submit">
                Review changes
            </button>
        `;

        openModal('modal-overlay');
        if (window.lucide) lucide.createIcons();

        bindScheduleFormEvents(session, { members, options });

        initTimezonePicker('schedule-timezone', {
            selected: config?.config?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone,
        });
    } catch (error) {
        // A request that was abandoned when the modal closed is not a failure
        // worth reporting: nobody is waiting for it, and the toast would land
        // over whatever is on screen now.
        if (session.closed) return;
        console.error('Failed to open schedule modal:', error);
        // The editor never appeared, so this session has nothing to own. Left
        // open, its dismissal listeners would answer for whatever is on
        // screen until the next modal replaced them.
        session.closeModal();
        await onScheduleError(error);
    }
}

function bindScheduleFormEvents(session, { members, options }) {
    const form = document.getElementById('schedule-form');
    if (!form) return;

    initGroupsEditor(session, 'l1-groups-editor', 'l1-add-group', members);
    initOrderedUserList(session, 'l2-users-list', 'l2-add-user', members);

    // Cadence toggles. The handoff day is meaningless for a daily rotation and
    // the server canonicalizes it away, so the field is hidden rather than
    // sent as a value that will not survive.
    const bindCadence = (selectId, weeklyClass) => {
        const select = document.getElementById(selectId);
        const weeklyOnly = document.querySelector(weeklyClass);
        if (!select || !weeklyOnly) return;
        select.addEventListener('change', () => {
            weeklyOnly.style.display = select.value === 'weekly' ? '' : 'none';
        }, { signal: session.signal });
    };
    bindCadence('l1-rotation-type', '.l1-weekly-only');
    bindCadence('l2-rotation-type', '.l2-weekly-only');

    const l2Enabled = document.getElementById('l2-enabled');
    const l2Config = document.querySelector('.l2-config');
    const l2Panel = document.querySelector('.l2-users-panel');
    const l2Timeout = document.getElementById('l2-escalation-timeout');
    const l2TimeoutStatic = document.querySelector('.l2-timeout-static');
    if (l2Enabled) {
        l2Enabled.addEventListener('change', () => {
            if (l2Config) l2Config.style.display = l2Enabled.checked ? '' : 'none';
            if (l2Panel) l2Panel.style.display = l2Enabled.checked ? '' : 'none';
            // Switching the layer off leaves the number in the sentence as
            // text, so it still says what would happen if it were on. It
            // carries whatever was typed, not what the schedule was opened
            // with, or turning the layer off and on again would appear to
            // discard the edit.
            if (l2Timeout && l2TimeoutStatic) {
                if (!l2Enabled.checked) l2TimeoutStatic.textContent = l2Timeout.value;
                l2Timeout.hidden = !l2Enabled.checked;
                l2TimeoutStatic.hidden = l2Enabled.checked;
            }
        }, { signal: session.signal });
    }

    document.getElementById('schedule-cancel')?.addEventListener(
        'click', () => session.closeModal(), { signal: session.signal });

    form.addEventListener('submit', (event) => handleScheduleSubmit(event, session, options),
        { signal: session.signal });

    const deleteScheduleBtn = document.querySelector('.delete-schedule-btn');
    if (deleteScheduleBtn) {
        deleteScheduleBtn.addEventListener(
            'click', () => handleScheduleDelete(session, deleteScheduleBtn.dataset.teamId, options),
            { signal: session.signal });
    }
}

/**
 * The L2 backup order: one person per row, reordered by dragging, added from a
 * picker and removed from the row.
 *
 * It is the L1 editor with the group step taken out, and that is the whole
 * point: L2 has no groups to manage - the API takes a flat order - so the only
 * honest difference between the two editors is that this one has no chips.
 *
 * The list it replaced asked to be operated a third way, by dragging people
 * across from a second column of everyone available. That column also silently
 * doubled as the roster, which made removing someone and re-adding them a trip
 * in each direction.
 */
function initOrderedUserList(session, listId, pickerId, members) {
    const list = document.getElementById(listId);
    const picker = document.getElementById(pickerId);
    if (!list) return;

    const renumber = () => {
        list.querySelectorAll('.group-row').forEach((row, i) => {
            const label = row.querySelector('.group-label');
            if (!label) return;
            label.textContent = String(i + 1);
            label.setAttribute('aria-label', `Position ${i + 1}`);
        });
    };

    // Kept in step with renderL2Row in schedule-components.js.
    const createL2Row = (userId, name, index) => {
        const row = document.createElement('div');
        row.className = 'group-row user-row';
        row.dataset.userId = userId;
        row.innerHTML = `
            <i data-lucide="grip-vertical" class="group-drag-handle"></i>
            <span class="group-label" aria-label="Position ${index + 1}">${index + 1}</span>
            <span class="row-name" title="${escapeAttr(name)}">${escapeHtml(name)}</span>
            <button type="button" class="btn btn-icon btn-sm group-delete" aria-label="Remove from rotation">
                <i data-lucide="trash-2"></i>
            </button>
        `;
        return row;
    };

    if (typeof Sortable !== 'undefined') {
        new Sortable(list, {
            animation: 150,
            handle: '.group-drag-handle',
            ghostClass: 'sortable-ghost',
            chosenClass: 'sortable-chosen',
            onEnd: () => {
                renumber();
                if (window.lucide) lucide.createIcons();
            }
        });
    }

    list.addEventListener('click', (e) => {
        const remove = e.target.closest('.group-delete');
        if (!remove) return;
        remove.closest('.group-row')?.remove();
        renumber();
        clearGroupErrors(list);
    }, { signal: session.signal });

    // The picker offers every member, including one already in the order, and
    // refuses the duplicate on selection - the same bargain the L1 picker
    // makes, and it keeps the list of people from changing under a click.
    picker?.addEventListener('change', () => {
        const userId = picker.value;
        if (!userId) return;
        picker.value = '';
        if (list.querySelector(`.group-row[data-user-id="${CSS.escape(userId)}"]`)) return;

        const name = members.find(u => u.id === userId)?.name || userId;
        list.appendChild(createL2Row(userId, name, list.querySelectorAll('.group-row').length));
        if (window.lucide) lucide.createIcons();
        clearGroupErrors(list);
    }, { signal: session.signal });
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
function initGroupsEditor(session, editorId, addGroupBtnId, members) {
    const editor = document.getElementById(editorId);
    const addGroupBtn = document.getElementById(addGroupBtnId);
    if (!editor) return;

    const buildOptions = () => {
        const opts = ['<option value="">+ Add</option>'];
        for (const u of members) {
            opts.push(`<option value="${escapeAttr(u.id)}">${escapeHtml(u.name)}</option>`);
        }
        return opts.join('');
    };

    const renumberGroups = () => {
        editor.querySelectorAll('.group-row').forEach((row, i) => {
            const label = row.querySelector('.group-label');
            if (!label) return;
            // The number is the group's place in the rotation; the word
            // "group" is in the panel heading, and only assistive tech needs
            // it repeated here.
            label.textContent = String(i + 1);
            label.setAttribute('aria-label', `Group ${i + 1}`);
        });
    };

    const createGroupRow = (index) => {
        const row = document.createElement('div');
        row.className = 'group-row';
        row.dataset.groupId = crypto.randomUUID();
        // Kept in step with renderGroupRow in schedule-components.js: the two
        // build the same row, one from stored config and one from a click.
        row.innerHTML = `
            <i data-lucide="grip-vertical" class="group-drag-handle"></i>
            <span class="group-label" aria-label="Group ${index + 1}">${index + 1}</span>
            <div class="group-chips"></div>
            <select class="form-select group-add-user" aria-label="Add a user to this group">${buildOptions()}</select>
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
        }, { signal: session.signal });
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
    }, { signal: session.signal });

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
        chip.innerHTML = `<span class="chip-name" title="${escapeAttr(optionText)}">${escapeHtml(optionText)}</span>`
            + `<button type="button" class="chip-remove" aria-label="Remove">×</button>`;
        chips.appendChild(chip);
        select.value = '';
        clearGroupErrors(editor);
    }, { signal: session.signal });
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
    document.querySelectorAll('#l2-users-list .group-row').forEach(item => {
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

async function handleScheduleSubmit(e, session, options) {
    e.preventDefault();

    const form = e.target;
    const teamId = form.dataset.teamId;
    // The version the form was loaded at, carried on the form itself. Never
    // the preview's base_version: taking that would pin the save to whatever
    // was seen last rather than to what was edited, which is the stale write
    // the version exists to refuse.
    const expectedVersion = parseInt(form.dataset.expectedVersion, 10) || 0;
    const config = collectConfig();
    // Read here, with everything else. By the time Confirm is pressed the
    // field is off screen, and reaching for it then is how it silently became
    // an empty string.
    const reason = document.getElementById('schedule-reason')?.value || '';

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
        const preview = await API.schedules.preview(teamId, config, until,
            { signal: session.signal });
        if (session.closed) return;

        // The preview evaluated against a version; if it is not the one being
        // edited, someone else has saved in the meantime and this form is
        // describing a schedule that no longer exists.
        if (preview.base_version !== expectedVersion) {
            showToast(
                'Someone else saved this schedule while you were editing. Reopen it to see their changes.',
                'error');
            return;
        }

        await showPreviewStep(session, { teamId, config, preview, expectedVersion, reason, options });
    } catch (error) {
        if (session.closed) return;
        console.error('Failed to preview schedule:', error);
        await handleSaveError(error, session, teamId, options);
    } finally {
        if (submitBtn) submitBtn.disabled = false;
    }
}

async function showPreviewStep(session, { teamId, config, preview, expectedVersion, reason, options }) {
    const { title: modalTitle, body: modalContent, footer: modalFooter } = modalShell();

    const ids = [
        ...(preview.on_call_before?.l1?.user_ids || []),
        ...(preview.on_call_after?.l1?.user_ids || []),
        ...(preview.entries || []).flatMap(e => e.user_ids || []),
    ];
    const names = await resolveNames(ids);
    if (session.closed) return;

    const previousTitle = modalTitle.textContent;

    // The form is hidden, not serialized. Reading innerHTML captures markup,
    // and what someone typed lives in the elements rather than in their
    // attributes - so restoring from a string would quietly hand back the
    // handoff time, the L2 toggle and the reason as they were when the modal
    // opened. Keeping the live nodes also keeps their listeners and the
    // timezone picker, so nothing has to be rebound on the way back.
    const form = document.getElementById('schedule-form');
    if (form) form.style.display = 'none';

    const previewHost = document.createElement('div');
    previewHost.id = 'schedule-preview-host';
    previewHost.innerHTML = schedulePreview(preview, names, config.timezone);
    modalContent.appendChild(previewHost);

    // The footer is hidden rather than replaced, for the same reason as the
    // form: rewriting its markup gives back buttons that look right and do
    // nothing, because the listeners were bound to the nodes that were thrown
    // away. Cancel is one of those.
    const previousButtons = [...modalFooter.children];
    for (const button of previousButtons) button.style.display = 'none';

    const previewButtons = document.createElement('span');
    previewButtons.className = 'preview-footer-buttons';
    previewButtons.innerHTML = `
        <button type="button" class="btn btn-secondary" id="preview-back">Back to editing</button>
        <button type="button" class="btn btn-primary" id="preview-confirm">Save schedule</button>
    `;
    modalFooter.appendChild(previewButtons);

    modalTitle.textContent = 'Review changes';
    if (window.lucide) lucide.createIcons();

    const restoreForm = () => {
        previewHost.remove();
        previewButtons.remove();
        for (const button of previousButtons) button.style.display = '';
        if (form) form.style.display = '';
        modalTitle.textContent = previousTitle;
        if (window.lucide) lucide.createIcons();
    };

    document.getElementById('preview-back')?.addEventListener('click', restoreForm,
        { signal: session.signal });

    document.getElementById('preview-confirm')?.addEventListener('click', async () => {
        const confirmBtn = document.getElementById('preview-confirm');
        if (confirmBtn) confirmBtn.disabled = true;
        try {
            // No session signal on the way out. Reads are abandoned when the
            // modal closes, because their answer has nowhere to land; a write
            // that is already in flight has already changed something, and
            // aborting it would only cost us the answer about what.
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

            // Nothing is compared with the preview. The save no longer
            // reports who is on duty afterwards - it says what it did, and
            // the refresh below asks the separate question of who is on
            // call now. The preview was always advisory; a warning that it
            // "differed" only ever restated that.
            //
            // What the save did is said either way; the screen is only
            // touched while this is still the screen. A save that lands
            // after its modal was replaced has nothing here to close.
            if (!session.closed) {
                previewHost.remove();
                session.closeModal();
            }
            await options.onChanged?.(teamId);
        } catch (error) {
            console.error('Failed to save schedule:', error);
            await handleSaveError(error, session, teamId, options);
        } finally {
            if (confirmBtn) confirmBtn.disabled = false;
        }
    }, { signal: session.signal });
}

/**
 * What to do about a save the server refused.
 *
 * Three of these reopen the editor. Reopening starts a new session, and this
 * one is closed on the way: whatever is still in flight here answers to
 * nobody, which is the point.
 */
function handleSaveError(error, session, teamId, options) {
    const reopen = (message) => {
        showToast(message, 'error');
        // Reopening replaces whatever is on screen with this team's editor,
        // which is right while this editor is on screen and wrong once
        // something else is. The refusal is still reported.
        if (session.closed) return;
        session.closeModal();
        openScheduleEditor(teamId, options);
    };

    return onScheduleError(error, {
        schedule_version_conflict: () => reopen(
            'Someone else changed this schedule. Reopening it with their changes.'),
        schedule_deleted: () => reopen(
            'This schedule was deleted. Reopening it so you can recreate it.'),
        schedule_exists: () => reopen('This team already has a schedule. Reopening it.'),
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

async function handleScheduleDelete(session, teamId, options) {
    if (!confirm('Delete this schedule? The rotation stops and its overrides are cleared. ' +
        'Past shifts stay in the calendar, and you can recreate it later.')) {
        return;
    }
    try {
        const version = parseInt(
            document.getElementById('schedule-form')?.dataset.expectedVersion, 10) || 0;
        await API.schedules.deleteSchedule(teamId, version);
        showToast('Schedule deleted', 'success');
        if (!session.closed) session.closeModal();
        await options.onChanged?.(teamId);
    } catch (error) {
        console.error('Failed to delete schedule:', error);
        await onScheduleError(error, {
            schedule_version_conflict: () => {
                showToast('Someone else changed this schedule. Reopening it.', 'error');
                if (session.closed) return;
                session.closeModal();
                openScheduleEditor(teamId, options);
            },
            schedule_deleted: () => {
                showToast('This schedule was already deleted.', 'info');
                session.closeModal();
            },
        });
    }
}
