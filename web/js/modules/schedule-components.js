/**
 * The markup the schedule feature renders.
 *
 * Every function here takes data and returns a string. None of them read the
 * document, call the API or decide what happens next - the modules that own a
 * flow do that, and hand the result to these.
 *
 * They were part of the app-wide component object, reached through a global.
 * They are ordinary exports now, so that what uses them is visible in the
 * imports rather than discovered at run time, and so that removing one is a
 * build-time question instead of a page-load surprise.
 */

import { escapeHtml, escapeAttr } from '/js/core/utils.js';
import { Permissions } from '/js/modules/permissions.js';
import { assertScheduleKind, scheduleActive, scheduleExists } from '/js/modules/schedule-shared.js';

/**
 * Render enhanced on-call widget with schedule data
 * @param {Object} onCall - Current on-call result
 * @param {Object} ctx - widget context: {teamId, kind, scheduleId, deletedAt, names}
 */
export function onCallWidget(onCall, ctx = {}) {
    const teamId = ctx.teamId || '';
    const scheduleId = ctx.scheduleId || '';
    const names = ctx.names || new Map();
    const l1 = onCall?.l1 || null;
    const l2 = onCall?.l2 || null;
    const isOverride = l1?.source === 'override';
    const l1Names = onCallNames(l1, names);

    // Four states, not two. "Nobody is on duty" is a fact about a working
    // schedule; "we could not find out" is not, and rendering the second
    // as the first turns an outage into a reassuring blank.
    const status = onCallStatus(ctx, l1Names);

    // Two values, never one. The grid slot is where the handoff math puts
    // the shift; the assignment is when this particular composition
    // actually took effect. After a mid-shift edit or an override they
    // differ, and a single "since" would have to lie about one of them.
    const times = assignmentTimes(l1);

    return `
        <div class="on-call-widget" data-team-id="${escapeAttr(teamId)}"
             data-schedule-id="${escapeAttr(scheduleId)}"
             data-schedule-state="${scheduleState(ctx)}">
            <div class="on-call-header">
                <i data-lucide="phone-call" class="on-call-icon"></i>
                <span class="on-call-title">On-Call Now</span>
            </div>
            <div class="on-call-user">
                <div class="on-call-user-avatar">
                    <i data-lucide="user"></i>
                </div>
                <div class="on-call-user-info">
                    <span class="on-call-user-name-wrap${l1Names ? ' text-tip' : ''}"${l1Names ? ` data-tip="${escapeAttr(l1Names)}"` : ''}>
                        <span class="on-call-user-name ${status.className}">${escapeHtml(status.label)}</span>
                    </span>
                    ${isOverride ? `
                        <span class="on-call-override-group">
                            <span class="on-call-override-badge">Override</span>
                            ${Permissions.can('delete_override', { teamId: teamId }) ? `
                            <button class="delete-override-btn"
                                    data-schedule-id="${escapeAttr(scheduleId)}"
                                    data-override-id="${escapeAttr(l1.override_id || '')}"
                                    title="Remove override">
                                <i data-lucide="x"></i>
                            </button>
                            ` : ''}
                        </span>
                    ` : ''}
                    ${times.assignmentLine ? `<span class="on-call-until">${escapeHtml(times.assignmentLine)}</span>` : ''}
                    ${times.shiftLine ? `<span class="on-call-shift">${escapeHtml(times.shiftLine)}</span>` : ''}
                </div>
            </div>
            ${l2 ? `
                <div class="on-call-l2">
                    <span class="on-call-l2-label">L2 Backup:</span>
                    <span class="on-call-l2-name">${escapeHtml(onCallNames(l2, names))}</span>
                </div>
            ` : ''}
            <div class="on-call-actions">
                ${Permissions.can('manage_schedule', { teamId: teamId }) ? `
                <button class="btn btn-sm btn-secondary edit-schedule-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="settings"></i>
                    ${ctx.kind === 'deleted' ? 'Recreate' : 'Configure'}
                </button>
                ` : ''}
                ${scheduleExists(ctx.kind) ? `
                <!-- Offered for a deleted schedule too: its past shifts are
                     still there, and they are often exactly what someone is
                     looking for after it was turned off. Not offered when
                     there is no schedule at all, where it opens onto a 404. -->
                <button class="btn btn-sm btn-secondary view-schedule-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="calendar-days"></i>
                    View Schedule
                </button>
                ` : ''}
                ${scheduleActive(ctx.kind) && Permissions.can('create_override', { teamId: teamId }) ? `
                <button class="btn btn-sm btn-primary create-override-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="user-plus"></i>
                    Override
                </button>
                ` : ''}
            </div>
        </div>
    `;
}

/**
 * The state of a team's schedule, as one word.
 *
 * Written into the DOM because the presence of a schedule id does not mean
 * there is a schedule to act on - a deleted one keeps its id so it can be
 * recreated - and anything reading the page, tests included, would otherwise
 * have to reconstruct that rule for itself.
 *
 * It reads the state rather than working it out: the context carries the kind,
 * and a second derivation here is how three places came to hold the same rule
 * in three slightly different forms.
 */
export function scheduleState(ctx) {
    return assertScheduleKind(ctx?.kind);
}

/**
 * What to say when nobody's name can be shown.
 *
 * Each of these means something different to whoever is reading it: set one
 * up, wait for the handoff, recreate it, or go and look at why the request
 * failed. Collapsing them into "Not configured" sends three of those four
 * people to the wrong place.
 */
export function onCallStatus(ctx, names) {
    const kind = assertScheduleKind(ctx?.kind);
    // A name answers the question the widget asks, whatever state the schedule
    // is in - except when the state is that we could not ask.
    if (kind !== 'unavailable' && names) return { label: names, className: '' };

    switch (kind) {
        case 'unavailable':
            return { label: 'On-call unavailable', className: 'is-unavailable' };
        case 'absent':
            return { label: 'Not configured', className: 'is-muted' };
        case 'deleted':
            return { label: 'Schedule deleted', className: 'is-muted' };
        case 'active':
            return { label: 'No one on duty', className: 'is-muted' };
    }
}

/**
 * The people on duty on one layer, as a display string.
 * @param {Object} layer - LayerOnCallDTO or null
 * @param {Map} names - id -> name
 */
export function onCallNames(layer, names) {
    if (!layer) return '';
    return (layer.user_ids || [])
        .map(id => (names && names.get(id)) || `Unknown user (${id})`)
        .join(', ');
}

/**
 * The two time facts about an assignment, kept apart: when the shift began by
 * the rotation grid, and when this particular assignment took effect. They
 * coincide most of the time, which is exactly why conflating them survives so
 * long unnoticed.
 */
export function assignmentTimes(layer) {
    if (!layer) return { assignmentLine: '', shiftLine: '' };

    const fmt = (value) => value ? new Date(value).toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
    }) : '';

    const assignmentStart = fmt(layer.assignment_start);
    const assignmentEnd = fmt(layer.assignment_end);
    const gridStart = fmt(layer.grid_slot_start);

    const assignmentLine = assignmentEnd ? `on duty until ${assignmentEnd}` : '';

    // Only worth saying when the two differ - otherwise it is the same
    // fact twice, and repeating it would train people to ignore the line
    // that matters when they do diverge.
    const differs = layer.grid_slot_start && layer.assignment_start &&
        new Date(layer.grid_slot_start).getTime() !== new Date(layer.assignment_start).getTime();
    const shiftLine = differs
        ? `assigned ${assignmentStart} · shift began ${gridStart}`
        : '';

    return { assignmentLine, shiftLine };
}

/**
 * Render on-call list header (similar to teams list header)
 */
export function onCallListHeader() {
    return `
        <div class="oncall-list-header">
            <div class="oncall-cell oncall-cell-primary">Team</div>
            <div class="oncall-cell">On-Call</div>
            <div class="oncall-cell">Until</div>
            <div class="oncall-cell">Status</div>
            <div class="oncall-cell oncall-cell-actions"></div>
        </div>
    `;
}

/**
 * Render compact on-call overview row
 * @param {Object} onCall - Current on-call result
 * @param {Object} team - Team object
 * @param {Object} ctx - widget context
 */
export function onCallOverviewRow(onCall, team, ctx = {}) {
    const teamId = team?.id || '';
    const teamName = team?.name || teamId || 'Team';
    const teamInitial = teamName ? teamName.charAt(0).toUpperCase() : '?';
    const names = ctx.names || new Map();
    const scheduleId = ctx.scheduleId || '';

    const l1 = onCall?.l1 || null;
    const isOverride = l1?.source === 'override';
    const canManage = Permissions.can('manage_schedule', { teamId });
    const canOverride = Permissions.can('create_override', { teamId });
    const canDeleteOverride = Permissions.can('delete_override', { teamId });

    const namesRaw = onCallNames(l1, names);
    const hasOnCall = !!namesRaw;
    const nameText = hasOnCall ? escapeHtml(namesRaw) : '—';

    const untilText = l1?.assignment_end
        ? new Date(l1.assignment_end).toLocaleString(undefined, {
            month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
        })
        : '';

    // The badge says more than the state does - who is covering and why - but
    // only once the schedule is running. The other three states are the whole
    // answer on their own, and they are read from the context rather than
    // pieced back together from flags.
    let statusLabel;
    let statusClass;
    switch (assertScheduleKind(ctx.kind)) {
        case 'unavailable':
            statusLabel = 'Unavailable';
            statusClass = 'unavailable';
            break;
        case 'absent':
            statusLabel = 'Not configured';
            statusClass = 'unconfigured';
            break;
        case 'deleted':
            statusLabel = 'Deleted';
            statusClass = 'unconfigured';
            break;
        case 'active':
            if (isOverride) {
                statusLabel = 'Override';
                statusClass = 'override';
            } else if (hasOnCall) {
                statusLabel = 'Scheduled';
                statusClass = 'scheduled';
            } else {
                statusLabel = 'No one on-call';
                statusClass = 'unconfigured';
            }
            break;
    }

    return `
        <div class="oncall-row" data-team-id="${escapeHtml(teamId)}"
             data-schedule-id="${escapeAttr(scheduleId)}"
             data-schedule-state="${scheduleState(ctx)}">
            <div class="oncall-cell oncall-cell-primary">
                <div class="oncall-avatar">
                    <span>${escapeHtml(teamInitial)}</span>
                </div>
                <div class="oncall-primary-text">
                    <div class="oncall-team-name">${escapeHtml(teamName)}</div>
                    <div class="oncall-team-id">${escapeHtml(teamId)}</div>
                </div>
            </div>
            <div class="oncall-cell${hasOnCall ? ' text-tip' : ''}" data-label="On-Call"${hasOnCall ? ` data-tip="${escapeAttr(namesRaw)}"` : ''}>
                <span class="oncall-user-name ${hasOnCall ? '' : 'is-muted'}">${nameText}</span>
            </div>
            <div class="oncall-cell" data-label="Until">
                <span class="oncall-until ${untilText ? '' : 'is-muted'}">${untilText || '—'}</span>
            </div>
            <div class="oncall-cell" data-label="Status">
                <span class="badge badge-status oncall-status-badge ${statusClass}">
                    ${statusLabel}
                    ${isOverride && canDeleteOverride ? `<button class="delete-override-btn" data-schedule-id="${escapeAttr(scheduleId)}" data-override-id="${escapeAttr(l1.override_id || '')}" title="Remove override"><i data-lucide="x"></i></button>` : ''}
                </span>
            </div>
            <div class="oncall-cell oncall-cell-actions">
                ${canOverride && scheduleActive(ctx.kind) ? `
                <button class="btn btn-sm btn-secondary create-override-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="user-plus"></i>
                    Override
                </button>
                ` : ''}
                ${scheduleExists(ctx.kind) ? `
                <button class="btn btn-sm btn-secondary view-schedule-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="calendar-days"></i>
                    Schedule
                </button>
                ` : ''}
                ${canManage ? `
                <button class="btn btn-sm btn-secondary edit-schedule-btn" data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="settings"></i>
                    Configure
                </button>
                ` : ''}
            </div>
        </div>
    `;
}

/**
 * Structured render warnings, as banners.
 *
 * The codes are branched on rather than the messages: the server sends a
 * code, a layer and an interval precisely so that a client does not have
 * to read prose to know what happened.
 *
 * @param {Array} warnings - ScheduleWarningDTO[]
 * @param {Object} [extra] - {historyCompleteFrom, deletedAt}
 */
export function scheduleWarnings(warnings, extra = {}) {
    const banners = [];
    const when = (value) => value ? new Date(value).toLocaleString(undefined, {
        month: 'short', day: 'numeric', year: 'numeric',
    }) : '';

    if (extra.deletedAt) {
        banners.push({
            level: 'warning',
            icon: 'archive',
            text: `This schedule was deleted on ${when(extra.deletedAt)}. Past shifts are still shown.`,
        });
    }

    const seen = new Set();
    for (const warning of warnings || []) {
        // One banner per kind. A gap that spans four revisions is one
        // thing that went wrong, not four.
        if (seen.has(warning.code)) continue;
        seen.add(warning.code);

        switch (warning.code) {
            case 'history_incomplete':
                banners.push({
                    level: 'info',
                    icon: 'history',
                    text: extra.historyCompleteFrom
                        ? `Exact history starts ${when(extra.historyCompleteFrom)}. Earlier shifts were never recorded and are not shown.`
                        : 'Part of this range predates the recorded history and is not shown.',
                });
                break;
            // revision_gap, revision_overlap and override_collision are
            // not warnings any more: the server refuses to render damaged
            // data rather than drawing a calendar around it, so they
            // arrive as an error, not as a banner over a plausible answer.
            case 'schedule_inactive':
                banners.push({
                    level: 'info',
                    icon: 'pause-circle',
                    text: `The schedule was not active from ${when(warning.from)} to ${when(warning.until)}.`,
                });
                break;
            default:
                break;
        }
    }

    if (banners.length === 0) return '';
    return `
        <div class="schedule-banners">
            ${banners.map(b => `
                <div class="schedule-banner schedule-banner-${b.level}">
                    <i data-lucide="${b.icon}"></i>
                    <div>${escapeHtml(b.text)}</div>
                </div>
            `).join('')}
        </div>
    `;
}

/**
 * Schedule configuration editor.
 *
 * @param {Object} state
 * @param {Object} state.config - ScheduleConfigDTO, or null for a new schedule
 * @param {Array}  state.members - who may be assigned: {id, name}
 * @param {Map}    state.names - id -> display name, covering everyone the
 *                 config mentions, including people who have left the team
 * @param {number} state.version - config_version the editor loaded (0 when new)
 * @param {string} state.deletedAt - set when the schedule was deleted and this is a recreate
 * @param {string} teamId
 */
export function scheduleConfigModal(state, teamId) {
    const config = state.config || null;
    const members = state.members || [];
    const names = state.names || new Map();
    const l1 = config?.l1 || {};
    const l2 = config?.l2 || {};
    const l1Groups = l1.groups || [];
    const l2UserIds = l2.user_ids || [];

    const nameOf = (id) => names.get(id) || id;

    // The L2 order can name someone who has since left the team. They are
    // shown so they can be seen and removed; they are not in `members`, so
    // the picker will not offer them again.
    const l2SelectedIds = new Set(l2UserIds);
    const l2Available = members.filter(u => !l2SelectedIds.has(u.id));

    const renderUserItem = (id, name) => `
        <div class="rotation-user" data-user-id="${escapeAttr(id)}">
            <i data-lucide="grip-vertical" class="drag-handle"></i>
            <div class="rotation-user-avatar">${escapeHtml((name || '?').charAt(0).toUpperCase())}</div>
            <span class="rotation-user-name">${escapeHtml(name)}</span>
        </div>
    `;

    const renderUserChip = (id) => `
        <span class="user-chip" data-user-id="${escapeAttr(id)}">
            ${escapeHtml(nameOf(id))}
            <button type="button" class="chip-remove" aria-label="Remove">×</button>
        </span>
    `;

    const renderAddUserSelect = () => `
        <select class="form-select group-add-user">
            <option value="">+ Add user</option>
            ${members.map(u => `<option value="${escapeAttr(u.id)}">${escapeHtml(u.name)}</option>`).join('')}
        </select>
    `;

    // data-group-id is the row's identity, and it is what lets the server
    // tell "this group gained a member" from "the groups were replaced".
    // Losing it across a reorder would restart the rotation.
    const renderGroupRow = (group, index) => `
        <div class="group-row" data-group-id="${escapeAttr(group.id)}">
            <i data-lucide="grip-vertical" class="group-drag-handle"></i>
            <span class="group-label">Group ${index + 1}</span>
            <div class="group-chips">
                ${(group.user_ids || []).map(renderUserChip).join('')}
            </div>
            ${renderAddUserSelect()}
            <button type="button" class="btn btn-icon btn-sm group-delete" aria-label="Delete group">
                <i data-lucide="trash-2"></i>
            </button>
        </div>
    `;

    const dayOption = (value, label, selected) =>
        `<option value="${value}" ${selected === value ? 'selected' : ''}>${label}</option>`;
    const dayOptions = (selected) => [
        dayOption(0, 'Sunday', selected),
        dayOption(1, 'Monday', selected),
        dayOption(2, 'Tuesday', selected),
        dayOption(3, 'Wednesday', selected),
        dayOption(4, 'Thursday', selected),
        dayOption(5, 'Friday', selected),
        dayOption(6, 'Saturday', selected),
    ].join('');

    const l1Daily = l1.rotation_type === 'daily';
    const l2Daily = l2.rotation_type === 'daily';
    const l2Enabled = !!l2.enabled;

    return `
        <form id="schedule-form" data-team-id="${escapeAttr(teamId)}"
              data-expected-version="${state.version || 0}">
            ${state.deletedAt ? `
            <div class="schedule-banner schedule-banner-warning" id="recreate-banner">
                <i data-lucide="rotate-ccw"></i>
                <div>
                    <strong>This schedule was deleted.</strong>
                    Saving recreates it from its last configuration, shown below.
                    Past shifts stay in the calendar either way.
                </div>
            </div>
            ` : ''}
            <div class="schedule-config-grid">
                <!-- Left Column: Settings -->
                <div class="schedule-settings">
                    <div class="form-section">
                        <h4 class="form-section-title">
                            <i data-lucide="globe"></i>
                            General Settings
                        </h4>
                        <div class="form-group">
                            <label for="schedule-timezone">Timezone</label>
                            <div id="schedule-timezone" class="tz-picker" data-name="timezone"></div>
                        </div>
                        <div class="form-group">
                            <label for="slack-usergroup-id">
                                Slack Usergroup
                                <span class="tooltip-icon" data-tooltip="Syncs @usergroup with current on-call. Get ID: Slack → Usergroup Settings → Copy ID">
                                    <i data-lucide="info" style="width:14px;height:14px;opacity:0.6;vertical-align:middle;margin-left:4px;"></i>
                                </span>
                            </label>
                            <input type="text" id="slack-usergroup-id" name="slack_usergroup_id"
                                   class="form-input"
                                   value="${escapeAttr(config?.slack_usergroup_id || '')}"
                                   placeholder="S12345678">
                        </div>
                    </div>

                    <div class="form-section">
                        <h4 class="form-section-title">
                            <i data-lucide="clock"></i>
                            Rotation Settings
                        </h4>
                        <div class="form-row">
                            <div class="form-group">
                                <label for="l1-rotation-type">Type</label>
                                <select id="l1-rotation-type" name="l1_rotation_type" class="form-select">
                                    <option value="daily" ${l1Daily ? 'selected' : ''}>Daily</option>
                                    <option value="weekly" ${l1Daily ? '' : 'selected'}>Weekly</option>
                                </select>
                            </div>
                            <div class="form-group">
                                <label for="l1-handoff-time">Handoff</label>
                                <input type="time" id="l1-handoff-time" name="l1_handoff_time"
                                       value="${escapeAttr(l1.handoff_time || '11:00')}" class="form-input">
                            </div>
                        </div>
                        <div class="form-group l1-weekly-only" style="${l1Daily ? 'display:none' : ''}">
                            <label for="l1-handoff-day">Handoff Day</label>
                            <select id="l1-handoff-day" name="l1_handoff_day" class="form-select">
                                ${dayOptions(l1.handoff_day === null || l1.handoff_day === undefined ? 1 : l1.handoff_day)}
                            </select>
                        </div>
                    </div>

                    <div class="form-section">
                        <h4 class="form-section-title">
                            <label class="toggle-label">
                                <input type="checkbox" id="l2-enabled" name="l2_enabled" ${l2Enabled ? 'checked' : ''}>
                                <i data-lucide="shield"></i>
                                L2 Escalation
                            </label>
                        </h4>
                        <!--
                            The L2 policy is sent whether or not the layer is on: the
                            server validates both layers either way, and a disabled
                            layer with no handoff time is not a valid configuration.
                            Hidden, not omitted.
                        -->
                        <div class="l2-config" style="${l2Enabled ? '' : 'display:none'}">
                            <div class="form-group">
                                <label for="l2-escalation-timeout">Escalation after (minutes)</label>
                                <input type="number" id="l2-escalation-timeout" name="l2_escalation_timeout"
                                       value="${l2.escalation_timeout_minutes || 5}" min="1" max="1440" class="form-input">
                            </div>
                            <div class="form-row">
                                <div class="form-group">
                                    <label for="l2-rotation-type">Type</label>
                                    <select id="l2-rotation-type" name="l2_rotation_type" class="form-select">
                                        <option value="daily" ${l2Daily ? 'selected' : ''}>Daily</option>
                                        <option value="weekly" ${l2Daily ? '' : 'selected'}>Weekly</option>
                                    </select>
                                </div>
                                <div class="form-group">
                                    <label for="l2-handoff-time">Handoff</label>
                                    <input type="time" id="l2-handoff-time" name="l2_handoff_time"
                                           value="${escapeAttr(l2.handoff_time || '11:00')}" class="form-input">
                                </div>
                            </div>
                            <div class="form-group l2-weekly-only" style="${l2Daily ? 'display:none' : ''}">
                                <label for="l2-handoff-day">Handoff Day</label>
                                <select id="l2-handoff-day" name="l2_handoff_day" class="form-select">
                                    ${dayOptions(l2.handoff_day === null || l2.handoff_day === undefined ? 1 : l2.handoff_day)}
                                </select>
                            </div>
                        </div>
                    </div>

                    <div class="form-section">
                        <div class="form-group">
                            <label for="schedule-reason">Reason for this change <span class="form-optional">(optional)</span></label>
                            <input type="text" id="schedule-reason" name="reason" class="form-input"
                                   maxlength="200" placeholder="Recorded with this revision">
                        </div>
                    </div>
                </div>

                <!-- Right Column: User Rotation -->
                <div class="schedule-users">
                    <div class="rotation-panel">
                        <h4 class="rotation-panel-title">
                            <i data-lucide="users"></i>
                            L1 Primary Rotation
                        </h4>
                        <p class="rotation-panel-hint">Each group rotates as a unit. Multiple users in one group share the on-call shift simultaneously.</p>
                        <div class="groups-editor" id="l1-groups-editor">
                            ${l1Groups.map((g, i) => renderGroupRow(g, i)).join('')}
                        </div>
                        <button type="button" class="btn btn-secondary btn-sm" id="l1-add-group">
                            <i data-lucide="plus"></i>
                            Add Group
                        </button>
                    </div>

                    <div class="rotation-panel l2-users-panel" style="${l2Enabled ? '' : 'display:none'}">
                        <h4 class="rotation-panel-title">
                            <i data-lucide="shield"></i>
                            L2 Backup Rotation
                        </h4>
                        <div class="rotation-columns">
                            <div class="rotation-column">
                                <div class="rotation-column-header">Available</div>
                                <div class="rotation-list" id="l2-available">
                                    ${l2Available.map(u => renderUserItem(u.id, u.name)).join('')}
                                </div>
                            </div>
                            <div class="rotation-column">
                                <div class="rotation-column-header">On-Call Order</div>
                                <div class="rotation-list rotation-list-selected" id="l2-users-list">
                                    ${l2UserIds.map(id => renderUserItem(id, nameOf(id))).join('')}
                                </div>
                            </div>
                        </div>
                    </div>
                </div>
            </div>

            ${config && !state.deletedAt ? `
            <div class="team-modal-section" style="margin-top: 24px; padding-top: 16px; border-top: 1px solid var(--border-color);">
                <h4 class="team-modal-section-title" style="color: var(--severity-critical);">
                    <i data-lucide="alert-triangle"></i>
                    Danger Zone
                </h4>
                <p style="font-size: 0.85rem; color: var(--text-muted); margin-bottom: 12px;">
                    Deleting this schedule stops the rotation and clears its overrides.
                    Past shifts stay in the calendar, and the schedule can be recreated later.
                </p>
                <button type="button" class="btn btn-sm btn-danger delete-schedule-btn"
                        data-team-id="${escapeAttr(teamId)}">
                    <i data-lucide="trash-2" style="width:14px;height:14px;"></i>
                    Delete Schedule
                </button>
            </div>
            ` : ''}
        </form>
    `;
}

/**
 * The overrides that currently exist.
 *
 * Each row carries the revision it was read at. That number is the only
 * thing that makes an edit safe: the server refuses a change made against
 * a revision that is no longer the head, which is what stops two people
 * silently overwriting each other.
 *
 * @param {Array} overrides - ScheduleOverrideDTO[]
 * @param {string} scheduleId
 * @param {Map} names - id -> display name
 */
export function overridesList(overrides = [], scheduleId = '', names = new Map()) {
    if (!overrides || overrides.length === 0) return '';

    const formatDateTime = (dateStr) => new Date(dateStr).toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
    });

    return `
        <div class="overrides-list">
            <h4 class="overrides-list-title">
                <i data-lucide="calendar-clock"></i>
                Current & Upcoming Overrides
            </h4>
            ${overrides.map(o => {
                const name = names.get(o.user_id) || `Unknown user (${o.user_id})`;
                return `
                <div class="override-item">
                    <div class="override-item-user">
                        <div class="override-item-avatar">${escapeHtml(name.charAt(0).toUpperCase())}</div>
                        <span>${escapeHtml(name)}</span>
                    </div>
                    <div class="override-item-time">
                        ${formatDateTime(o.valid_from)} → ${formatDateTime(o.valid_to)}
                    </div>
                    ${o.reason ? `<div class="override-item-reason">${escapeHtml(o.reason)}</div>` : ''}
                    <div class="override-item-actions">
                        <button type="button" class="btn btn-sm btn-secondary edit-override-btn"
                                data-schedule-id="${escapeAttr(scheduleId)}"
                                data-override-id="${escapeAttr(o.override_id)}"
                                data-revision="${o.revision}"
                                data-user-id="${escapeAttr(o.user_id)}"
                                data-valid-from="${escapeAttr(o.valid_from)}"
                                data-valid-to="${escapeAttr(o.valid_to)}"
                                data-reason="${escapeAttr(o.reason || '')}"
                                title="Edit override">
                            <i data-lucide="pencil" style="width:14px;height:14px;"></i>
                        </button>
                        <button type="button" class="btn btn-sm btn-secondary delete-override-btn"
                                data-schedule-id="${escapeAttr(scheduleId)}"
                                data-override-id="${escapeAttr(o.override_id)}"
                                data-revision="${o.revision}"
                                title="Delete override">
                            <i data-lucide="trash-2" style="width:14px;height:14px;"></i>
                        </button>
                    </div>
                </div>
            `;
            }).join('')}
        </div>
    `;
}

/**
 * Override management modal.
 * @param {Object} state - {members, overrides, scheduleId, names}
 * @param {string} teamId
 */
export function overrideModal(state, teamId) {
    const members = state.members || [];
    const now = new Date();
    now.setSeconds(0, 0);
    const pad = (n) => String(n).padStart(2, '0');
    const local = (d) => `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    const nowLocal = local(now);
    const tomorrowLocal = local(new Date(now.getTime() + 24 * 60 * 60 * 1000));

    return `
        <div class="override-modal-content">
            ${overridesList(state.overrides, state.scheduleId, state.names)}

            <div class="override-form-section">
                <h4 class="override-form-title">
                    <i data-lucide="plus-circle"></i>
                    Create New Override
                </h4>
                <form id="override-form" data-team-id="${escapeAttr(teamId)}"
                      data-schedule-id="${escapeAttr(state.scheduleId || '')}">
                    <div class="form-row">
                        <div class="form-group">
                            <label for="override-user">Assign to User *</label>
                            <select id="override-user" name="user_id" class="form-select" required>
                                <option value="">Select user...</option>
                                ${members.map(u => `
                                    <option value="${escapeAttr(u.id)}">${escapeHtml(u.name)}</option>
                                `).join('')}
                            </select>
                        </div>
                        <div class="form-group">
                            <label for="override-timezone">Timezone</label>
                            <div id="override-timezone" class="tz-picker" data-name="timezone"></div>
                        </div>
                    </div>
                    <div class="form-row">
                        <div class="form-group">
                            <label for="override-start">Start *</label>
                            <input type="datetime-local" id="override-start" name="start_time"
                                   class="form-input" required value="${nowLocal}">
                        </div>
                        <div class="form-group">
                            <label for="override-end">End *</label>
                            <input type="datetime-local" id="override-end" name="end_time"
                                   class="form-input" required value="${tomorrowLocal}">
                        </div>
                    </div>
                    <!-- Filled in when a local time falls in a daylight-saving
                         transition: it may not exist, or it may happen twice. -->
                    <div class="override-time-note" id="override-time-note"></div>
                    <div class="form-group">
                        <label for="override-reason">Reason (optional)</label>
                        <input type="text" id="override-reason" name="reason" class="form-input"
                               placeholder="e.g., Vacation coverage">
                    </div>
                </form>
            </div>
        </div>
    `;
}

/**
 * The schedule calendar over a range.
 *
 * Entries are shifts: adjacent grid slots with the same duty already
 * merged by the server, so an unchanged weekly rotation draws one band
 * rather than seven. A shift names both its layer (l1 or l2) and its
 * source (rotation or override) - "override" is not a third layer, it is
 * how a layer's duty came about, and treating it as a layer is what made
 * the old calendar unable to say which layer an override applied to.
 *
 * @param {Object} render - ScheduleRenderResponse
 * @param {Date} startDate
 * @param {string} timezone - the zone to display in
 * @param {Map} names - id -> display name
 */
export function monthlyScheduleCalendar(render, startDate, timezone = 'UTC', names = new Map()) {
    const entries = render?.entries || [];
    const banners = scheduleWarnings(render?.warnings, {
        historyCompleteFrom: render?.history_complete_from,
        deletedAt: render?.deleted_at,
    });

    if (entries.length === 0) {
        return `
            ${banners}
            <div class="schedule-empty">
                <i data-lucide="calendar-x"></i>
                <p>Nobody is scheduled in this range</p>
            </div>
        `;
    }

    const getDateKey = (date) => date.toLocaleDateString('en-US', { timeZone: timezone });

    // A shift can span days; it appears on each day it covers, so a weekly
    // rotation is visible on every day of the week rather than only on the
    // day it began.
    //
    // The days a shift covers are found by sampling it every 12 hours
    // rather than by walking calendar boundaries: the shortest possible
    // day is 23 hours, so a 12-hour step cannot skip one, and no local
    // midnight has to be computed to get it right. The interval is
    // half-open, so a shift ending exactly at midnight does not claim the
    // day it ends on.
    const HALF_DAY = 12 * 60 * 60 * 1000;
    const entriesByDate = new Map();
    const pushEntry = (key, entry) => {
        if (!entriesByDate.has(key)) entriesByDate.set(key, []);
        const bucket = entriesByDate.get(key);
        if (!bucket.includes(entry)) bucket.push(entry);
    };
    for (const entry of entries) {
        const startMs = new Date(entry.start).getTime();
        const endMs = new Date(entry.end).getTime();
        for (let t = startMs; t < endMs; t += HALF_DAY) {
            pushEntry(getDateKey(new Date(t)), entry);
        }
        // The last instant the shift actually covers, for the tail of a
        // shift whose final day is shorter than the sampling step.
        pushEntry(getDateKey(new Date(Math.max(startMs, endMs - 1))), entry);
    }

    const days = [];
    let current = new Date(startDate);
    for (let i = 0; i < 30; i++) {
        days.push(new Date(current));
        const currentKey = getDateKey(current);
        let next = new Date(current.getTime() + 24 * 60 * 60 * 1000);
        if (getDateKey(next) === currentKey) {
            next = new Date(next.getTime() + 12 * 60 * 60 * 1000);
        }
        current = next;
    }

    const formatDate = (date) => {
        const todayKey = getDateKey(new Date());
        return {
            dayName: date.toLocaleDateString('en-US', { weekday: 'short', timeZone: timezone }),
            dayNum: date.toLocaleDateString('en-US', { day: 'numeric', timeZone: timezone }),
            month: date.toLocaleDateString('en-US', { month: 'short', timeZone: timezone }),
            isToday: getDateKey(date) === todayKey,
        };
    };

    // An override is styled by its source; its layer is still shown,
    // because "who is covering L2" and "who is covering L1" are different
    // facts about an override.
    const entryClass = (entry) =>
        entry.source === 'override' ? 'layer-override' : (entry.layer === 'l2' ? 'layer-l2' : 'layer-l1');
    const entryLabel = (entry) =>
        entry.source === 'override'
            ? `${entry.layer === 'l2' ? 'L2' : 'L1'} override`
            : (entry.layer === 'l2' ? 'L2' : 'L1');

    const formatShiftTime = (entry) => {
        const start = new Date(entry.start);
        const end = new Date(entry.end);
        const formatTime = (d) => d.toLocaleTimeString('en-US', {
            hour: '2-digit', minute: '2-digit', hour12: false, timeZone: timezone,
        });
        const sameDay = getDateKey(start) === getDateKey(end);
        if (sameDay) return `${formatTime(start)}-${formatTime(end)}`;
        const durationHours = (end - start) / (1000 * 60 * 60);
        if (durationHours >= 23.5) return `${formatTime(start)}→`;
        return `${formatTime(start)}-${formatTime(end)}`;
    };

    return `
        <div class="monthly-calendar">
            ${banners}
            <div class="calendar-header">
                <div class="calendar-legend">
                    <span class="legend-item layer-l1">L1 Primary</span>
                    <span class="legend-item layer-l2">L2 Backup</span>
                    <span class="legend-item layer-override">Override</span>
                </div>
                <div class="calendar-timezone">
                <i data-lucide="globe"></i>
                <div id="calendar-timezone-select" class="tz-picker" data-name="timezone"></div>
            </div>
            </div>
            <div class="calendar-list">
                ${days.map(day => {
        const dayEntries = entriesByDate.get(getDateKey(day)) || [];
        const { dayName, dayNum, month, isToday } = formatDate(day);

        return `
                        <div class="calendar-day ${isToday ? 'is-today' : ''}">
                            <div class="calendar-day-header">
                                <span class="calendar-day-name">${dayName}</span>
                                <span class="calendar-day-date">${dayNum} ${month}</span>
                            </div>
                            <div class="calendar-day-entries">
                                ${dayEntries.length > 0 ? dayEntries.map(entry => {
            const showTime = dayEntries.length > 1;
            const timeStr = showTime ? formatShiftTime(entry) : '';
            const userNames = (entry.user_ids || [])
                .map(id => names.get(id) || `Unknown user (${id})`)
                .join(', ') || 'Nobody';
            const isOverride = entry.source === 'override';
            return `
                                    <div class="calendar-entry ${entryClass(entry)}"
                                        ${isOverride ? `
                                            data-override-id="${escapeAttr(entry.override_id || '')}"
                                            data-user-id="${escapeAttr((entry.user_ids || [])[0] || '')}"
                                            data-valid-from="${escapeAttr(entry.start)}"
                                            data-valid-to="${escapeAttr(entry.end)}"
                                        ` : ''}>
                                        <span class="entry-layer">${entryLabel(entry)}</span>
                                        <span class="entry-user">${escapeHtml(userNames)}</span>
                                        ${timeStr ? `<span class="entry-time">${timeStr}</span>` : ''}
                                    </div>
                                `;
        }).join('') : `
                                    <div class="calendar-no-entry">No on-call</div>
                                `}
                            </div>
                        </div>
                    `;
    }).join('')}
            </div>
        </div>
    `;
}

/**
 * What a save would do, before it does it.
 *
 * The answer is true as of `evaluated_at` and is not a promise: the save
 * recomputes under a lock, and a handoff or an override in between can
 * change it. Saying so plainly is the honest thing - the alternative is a
 * client that quietly implies a guarantee the server never made.
 *
 * @param {Object} preview - SchedulePreviewResponse
 * @param {Map} names - id -> display name
 * @param {string} timezone
 */
export function schedulePreview(preview, names = new Map(), timezone = 'UTC') {
    const before = onCallNames(preview.on_call_before?.l1, names) || 'nobody';
    const after = onCallNames(preview.on_call_after?.l1, names) || 'nobody';
    const evaluatedAt = new Date(preview.evaluated_at).toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
    });

    // The next few handoffs, so the effect on the rotation order is
    // visible rather than inferred from one name.
    const upcoming = (preview.entries || [])
        .filter(e => e.layer === 'l1')
        .slice(0, 6)
        .map(e => ({
            when: new Date(e.start).toLocaleString(undefined, {
                month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: timezone,
            }),
            who: (e.user_ids || []).map(id => names.get(id) || `Unknown user (${id})`).join(', ') || 'Nobody',
            override: e.source === 'override',
        }));

    return `
        <div class="schedule-preview">
            ${preview.on_call_changed ? `
            <div class="schedule-banner schedule-banner-warning">
                <i data-lucide="user-cog"></i>
                <div>
                    <strong>This changes who is on duty right now.</strong>
                    ${escapeHtml(before)} → ${escapeHtml(after)}
                </div>
            </div>
            ` : `
            <div class="schedule-banner schedule-banner-info">
                <i data-lucide="check-circle"></i>
                <div>Who is on duty right now does not change: ${escapeHtml(after)} stays on.</div>
            </div>
            `}

            ${scheduleWarnings(preview.warnings)}

            <div class="preview-shifts">
                <h4 class="preview-shifts-title">Next shifts after saving</h4>
                ${upcoming.length > 0 ? `
                <ul class="preview-shift-list">
                    ${upcoming.map(shift => `
                        <li class="preview-shift ${shift.override ? 'is-override' : ''}">
                            <span class="preview-shift-when">${escapeHtml(shift.when)}</span>
                            <span class="preview-shift-who">${escapeHtml(shift.who)}</span>
                            ${shift.override ? '<span class="on-call-override-badge">Override</span>' : ''}
                        </li>
                    `).join('')}
                </ul>
                ` : '<p class="preview-empty">No shifts in the previewed window.</p>'}
            </div>

            <p class="preview-caveat">
                <i data-lucide="info"></i>
                Calculated at ${escapeHtml(evaluatedAt)}. Saving recalculates it, so a handoff
                or an override in the meantime can change the result.
            </p>
        </div>
    `;
}
