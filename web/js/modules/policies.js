/**
 * TokayOps Policies Module
 * Escalation policy management and editor UI
 */

import { State } from '/js/core/state.js';
import { Elements, showToast, escapeHtml } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';
import { Permissions } from '/js/modules/permissions.js';

// Current policy scope tab: 'team' | 'global'
let currentPolicyScope = 'team';

/**
 * Load policies from API
 * @param {string|null} teamFilter - Optional team ID to filter by
 */
export async function loadPolicies(teamFilter = null) {
    try {
        const response = await API.policies.list();
        let policies = response || [];

        // Filter by team if specified
        if (teamFilter) {
            policies = policies.filter(p => p.team_id === teamFilter);
        }

        State.policies = policies;
        renderPolicies();
    } catch (error) {
        console.warn('Failed to load policies:', error);
        showToast('Failed to load policies', 'error');
    }
}

/**
 * Get filtered policies by current scope
 */
function getFilteredPolicies() {
    if (!State.policies) return [];

    if (currentPolicyScope === 'global') {
        return State.policies.filter(p => p.team_id === null || p.team_id === undefined);
    } else {
        return State.policies.filter(p => p.team_id !== null && p.team_id !== undefined);
    }
}

/**
 * Render policies grid
 */
function renderPolicies() {
    if (!Elements.policiesGrid) return;

    const filteredPolicies = getFilteredPolicies();
    const isGlobalScope = currentPolicyScope === 'global';
    const canEditGlobal = Permissions.isAdmin();

    // Show read-only banner for non-admins on global tab
    let bannerHtml = '';
    if (isGlobalScope && !canEditGlobal) {
        bannerHtml = `
            <div class="policy-readonly-banner">
                <i data-lucide="info"></i>
                <span>You have read-only access to global policies. Contact an administrator to make changes.</span>
            </div>
        `;
    }

    if (filteredPolicies.length === 0) {
        const emptyMessage = isGlobalScope
            ? 'No global policies defined.'
            : 'No team policies yet. Create your first escalation policy.';
        Elements.policiesGrid.innerHTML = `
            ${bannerHtml}
            <div class="empty-state">
                <i data-lucide="shield-off" class="empty-icon"></i>
                <p>${emptyMessage}</p>
            </div>
        `;
    } else {
        Elements.policiesGrid.innerHTML = bannerHtml + filteredPolicies.map(policy =>
            Components.policyCard(policy)
        ).join('');
        bindPolicyCardEvents();
    }

    if (window.lucide) lucide.createIcons();
}

/**
 * Bind events on policy cards
 */
function bindPolicyCardEvents() {
    if (!Elements.policiesGrid) return;

    Elements.policiesGrid.querySelectorAll('.edit-policy-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            openPolicyEditor(btn.dataset.policyId);
        });
    });

    Elements.policiesGrid.querySelectorAll('.delete-policy-btn').forEach(btn => {
        btn.addEventListener('click', async (e) => {
            e.stopPropagation();
            await handleDeletePolicy(btn.dataset.policyId);
        });
    });

    Elements.policiesGrid.querySelectorAll('.duplicate-policy-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            openDuplicateModal(btn.dataset.policyId);
        });
    });

    Elements.policiesGrid.querySelectorAll('.policy-card').forEach(card => {
        card.addEventListener('click', () => {
            const policyId = card.dataset.policyId;
            if (policyId) openPolicyEditor(policyId);
        });
    });
}

/**
 * Show policies list view
 */
export function showPoliciesView() {
    State.currentView = 'policies';
    State.currentTeam = null;

    // Update sidebar state
    document.querySelectorAll('.sidebar-nav .nav-item').forEach(nav => nav.classList.remove('active'));
    document.querySelectorAll('.nav-section-link').forEach(nav => nav.classList.remove('active'));
    const policiesLink = document.querySelector('.nav-section-link[data-view="policies"]');
    if (policiesLink) policiesLink.classList.add('active');

    ViewManager.show('policies', { showStats: false, showViewToggle: false });

    // Show loading, load policies
    if (Elements.policiesLoading) Elements.policiesLoading.style.display = 'flex';
    if (Elements.policiesGrid) Elements.policiesGrid.innerHTML = '';

    loadPolicies().finally(() => {
        if (Elements.policiesLoading) Elements.policiesLoading.style.display = 'none';
    });
}

/**
 * Open policy editor modal
 * @param {string|null} policyId - Policy ID for editing, null for create
 */
export async function openPolicyEditor(policyId = null) {
    try {
        let policy = null;
        if (policyId) {
            policy = await API.policies.get(policyId);
        }
        State.editingPolicy = policy;

        // Load required data. Sprint 4: also fetch provider capabilities so
        // the step-type dropdown is built from the dispatcher's registry
        // rather than the old hardcoded {slack_dm, slack_channel} pair.
        const [teamsResponse, usersResponse, providersResponse] = await Promise.all([
            API.teams.list(),
            API.users.list(),
            API.providers.list().catch(() => ({ providers: [] })),
        ]);

        const teams = teamsResponse.teams || [];
        const users = usersResponse.users || [];
        const providers = providersResponse.providers || [];

        // Store in state for dynamic step updates
        State.users = users;
        State.teams = teams;
        State.providers = providers;

        // Render modal
        const isEdit = policy !== null;
        Elements.policyModalBody.innerHTML = Components.policyBuilderModal(policy, teams, users);

        // Render footer
        Elements.policyModalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="cancel-policy-btn">Cancel</button>
            <button type="button" class="btn btn-primary" id="save-policy-btn">${isEdit ? 'Save Changes' : 'Create Policy'}</button>
        `;

        Elements.policyModalOverlay.classList.add('active');
        document.body.style.overflow = 'hidden';

        if (window.lucide) lucide.createIcons();

        // Load schedule for initial team if exists
        const initialTeamId = policy?.team_id || (teams.length > 0 ? teams[0].id : null);
        if (initialTeamId) {
            await loadTeamSchedule(initialTeamId);
        }

        // Initialize Sortable for steps
        initStepsSortable();
        // Use single binding call
        bindPolicyEditorEvents();

    } catch (error) {
        showToast('Failed to open policy editor: ' + error.message, 'error');
    }
}

/**
 * Load the schedule a team's escalation steps can target.
 *
 * A deleted schedule keeps its ID: recreating one goes through the same
 * record, so a step pointing at it starts working again the moment it comes
 * back. Clearing the target here would quietly break policies that a recreate
 * would have healed, and nobody would connect the two events.
 */
async function loadTeamSchedule(teamId) {
    let scheduleId = null;
    let label = null;

    try {
        const config = await API.schedules.getConfig(teamId);
        scheduleId = config?.schedule_id || null;
        if (config?.deleted_at) {
            label = 'Schedule inactive (deleted)';
        }
    } catch (e) {
        if (e?.status !== 404) {
            console.warn('Failed to load schedule for team', teamId, e);
        }
        scheduleId = null;
    }

    State.currentScheduleId = scheduleId;

    document.querySelectorAll('.target-id-input option[data-schedule-placeholder]').forEach(opt => {
        opt.value = scheduleId || '';
        if (!scheduleId) {
            opt.textContent = 'No schedule configured for this team';
            opt.parentElement.disabled = true;
            return;
        }
        if (label) {
            // Still selectable in the sense that an existing step keeps
            // pointing at it, but not offered as a new choice.
            opt.textContent = label;
            opt.parentElement.disabled = true;
            return;
        }
        const team = State.teams.find(t => t.id === teamId);
        opt.textContent = `Team Schedule (${team?.name || 'Unknown'})`;
        opt.parentElement.disabled = false;
    });
}

/**
 * Initialize drag-and-drop for steps
 */
function initStepsSortable() {
    const stepsList = document.getElementById('policy-steps-list');
    if (stepsList && window.Sortable) {
        new Sortable(stepsList, {
            animation: 150,
            handle: '.step-drag-handle',
            ghostClass: 'sortable-ghost',
            onEnd: () => {
                document.querySelectorAll('.policy-step-row').forEach((row, index) => {
                    row.dataset.stepIndex = index;
                    row.querySelector('.step-index-label').textContent = `Step ${index + 1}`;
                });
            }
        });
    }
}

/**
 * Reindex step indices after reorder
 */
function reindexSteps() {
    const stepsList = document.getElementById('policy-steps-list');
    if (!stepsList) return;

    stepsList.querySelectorAll('.policy-step-row').forEach((row, index) => {
        row.dataset.stepIndex = index;
        const indexLabel = row.querySelector('.step-index-label');
        if (indexLabel) indexLabel.textContent = `Step ${index + 1}`;
    });
}

/**
 * Handle team change in policy editor
 */
async function handleTeamChange(e) {
    const teamId = e.target.value;
    if (teamId) {
        await loadTeamSchedule(teamId);
    } else {
        State.currentScheduleId = null;
    }

    // Update all existing schedule selectors
    document.querySelectorAll('.target-type-select').forEach(select => {
        if (select.value === 'schedule') {
            const row = select.closest('.policy-step-row');
            const targetContainer = row.querySelector('.target-selector-container');
            updateTargetSelector(targetContainer, 'schedule');
        }
    });
}

/**
 * Bind policy editor events using delegation
 */
function bindPolicyEditorEvents() {
    const modal = Elements.policyModalBody;
    const stepsList = document.getElementById('policy-steps-list');

    // Team Change
    const teamSelect = modal.querySelector('#policy-team-select');
    if (teamSelect) {
        teamSelect.addEventListener('change', handleTeamChange);
    }

    // Scope Tab Buttons - show/hide team selector
    const scopeTabs = modal.querySelectorAll('.scope-tab-sm');
    scopeTabs.forEach(tab => {
        tab.addEventListener('click', (e) => {
            if (tab.disabled) return;

            const scope = tab.dataset.scope;
            const teamSelectorGroup = document.getElementById('team-selector-group');
            const scopeInput = document.getElementById('policy-scope-input');

            // Update active state on tabs
            scopeTabs.forEach(t => t.classList.remove('active'));
            tab.classList.add('active');

            // Update hidden input value
            if (scopeInput) {
                scopeInput.value = scope;
            }

            // Show/hide team selector based on scope
            if (teamSelectorGroup) {
                teamSelectorGroup.style.display = scope === 'team' ? '' : 'none';
            }
        });
    });

    // Add Step
    const addStepBtn = document.getElementById('add-step-btn');
    if (addStepBtn) {
        addStepBtn.onclick = addNewStep;
    }

    // Save button
    const saveBtn = document.getElementById('save-policy-btn');
    if (saveBtn) {
        saveBtn.addEventListener('click', handleSavePolicy);
    }

    // Cancel/Close buttons
    const cancelBtn = document.getElementById('cancel-policy-btn');
    if (cancelBtn) {
        cancelBtn.addEventListener('click', closePolicyEditor);
    }
    if (Elements.policyModalClose) {
        Elements.policyModalClose.addEventListener('click', closePolicyEditor);
    }

    // Event Delegation for Steps List
    if (stepsList) {
        stepsList.addEventListener('click', (e) => {
            // Remove Step
            const removeBtn = e.target.closest('.remove-step-btn');
            if (removeBtn) {
                const row = removeBtn.closest('.policy-step-row');
                if (row) {
                    row.remove();
                    reindexSteps();
                }
                return;
            }
        });

        stepsList.addEventListener('change', (e) => {
            const target = e.target;
            const row = target.closest('.policy-step-row');
            if (!row) return;

            // Step Type Change. The select value is encoded as
            // "<provider>:<target_kind>" (e.g. "slack:dm" / "slack:channel"),
            // so we split it to drive both the target-type select and the
            // target selector. Sprint 4 replaced the old flat enum.
            if (target.classList.contains('step-type-select')) {
                const [, targetKind] = target.value.split(':');
                const targetTypeSelect = row.querySelector('.target-type-select');
                const targetContainer = row.querySelector('.target-selector-container');

                if (targetKind === 'channel') {
                    targetTypeSelect.innerHTML = '<option value="channel">Channel</option>';
                    updateTargetSelector(targetContainer, 'channel');
                } else {
                    targetTypeSelect.innerHTML = `
                        <option value="user">User</option>
                        <option value="schedule">Schedule</option>
                    `;
                    updateTargetSelector(targetContainer, 'user');
                }
                return;
            }

            // Target Type Change
            if (target.classList.contains('target-type-select')) {
                const targetType = target.value;
                const targetContainer = row.querySelector('.target-selector-container');
                updateTargetSelector(targetContainer, targetType);
                return;
            }
        });

        // User Search Input Delegation
        stepsList.addEventListener('input', (e) => {
            const input = e.target;
            if (input.hasAttribute('list') && input.closest('.target-selector-container')) {
                const val = input.value;
                const container = input.closest('.target-selector-container');
                const hiddenInput = container.querySelector('input[type="hidden"]');
                const listId = input.getAttribute('list');
                const option = document.querySelector(`#${listId} option[value="${val}"]`);

                if (option) {
                    hiddenInput.value = option.dataset.userId;
                    input.value = option.value;
                } else if (!val) {
                    hiddenInput.value = '';
                }
            }
        });
    }
}

/**
 * Update target selector based on target type
 */
function updateTargetSelector(container, targetType, currentValue = '') {
    container.innerHTML = '';
    const labelHtml = '<label>Target</label>';

    if (targetType === 'schedule') {
        const scheduleId = State.currentScheduleId;
        const teamId = document.getElementById('policy-team-select')?.value;
        const team = State.teams.find(t => t.id === teamId);

        let label = 'Select team first';
        if (team) {
            label = scheduleId ? `Team Schedule (${team.name})` : 'No schedule configured';
        }

        const isDisabled = !scheduleId;
        const value = scheduleId || '';

        container.innerHTML = `
            ${labelHtml}
            <select class="form-select target-id-input">
                <option value="${value}" data-schedule-placeholder="true">${label}</option>
            </select>
        `;
        container.querySelector('select').disabled = isDisabled;
        return;
    }

    if (targetType === 'channel') {
        container.innerHTML = `
            ${labelHtml}
            <input type="text" class="form-input target-id-input" placeholder="C01234567" value="${escapeHtml(currentValue)}">
        `;
    } else {
        // User selector
        const users = State.users || [];
        // Generate unique datalist ID
        const uniqueId = `user-datalist-${Math.random().toString(36).substr(2, 9)}`;
        container.innerHTML = `
            ${labelHtml}
            ${Components.searchableUserSelect(users, currentValue, uniqueId)}
        `;

        // Initial display name fix for existing value
        if (currentValue) {
            const input = container.querySelector('input[list]');
            const user = users.find(u => u.id === currentValue);
            if (input && user) input.value = user.name;
        }
    }
}

/**
 * Add a new empty step
 */
function addNewStep() {
    const stepsList = document.getElementById('policy-steps-list');
    if (!stepsList) return;

    const newIndex = stepsList.children.length;
    const currentTeamId = document.getElementById('policy-team-select')?.value || '';

    // Pass currentScheduleId for schedule target display. Default provider
    // is the first registered one (alphabetical) — Sprint 4 makes the editor
    // discover providers via /providers instead of hardcoding "slack_dm".
    const defaultProvider = (State.providers || [])[0]?.name || '';
    const stepHtml = Components.policyStepRow({
        provider: defaultProvider,
        target_kind: 'dm',
        target_type: 'user',
        target_id: '',
        delay_seconds: 0,
        timeout_seconds: 30,
        max_attempts: 5,
        message: '',
        continue_on_failure: true
    }, newIndex, State.users || [], State.teams || [], currentTeamId, State.currentScheduleId, State.providers || []);

    stepsList.insertAdjacentHTML('beforeend', stepHtml);

    if (window.lucide) lucide.createIcons();
}

/**
 * Handle save policy
 */
export async function handleSavePolicy() {
    const name = document.getElementById('policy-name-input')?.value?.trim();
    const description = document.getElementById('policy-description-input')?.value?.trim();

    // Get scope from hidden input (new tabs) or checked radio (edit mode)
    const scopeInput = document.getElementById('policy-scope-input');
    const scopeRadio = document.querySelector('input[name="policy-scope"]');
    const scope = scopeInput?.value || scopeRadio?.value || 'team';
    const isGlobalScope = scope === 'global';

    // Get team ID (only relevant for team scope)
    const teamId = document.getElementById('policy-team-select')?.value || null;

    // Validation
    if (!name) {
        showToast('Policy name is required', 'error');
        return;
    }

    // Team is required only for team-scoped policies
    if (!isGlobalScope && !teamId) {
        showToast('Team is required for team-scoped policies', 'error');
        return;
    }

    const steps = collectStepsData();
    if (steps.length === 0) {
        showToast('At least one step is required', 'error');
        return;
    }

    // Validate each step has target_id when required
    for (let i = 0; i < steps.length; i++) {
        const step = steps[i];
        if ((step.target_type === 'user' || step.target_type === 'channel' || step.target_type === 'schedule') && !step.target_id) {
            showToast(`Step ${i + 1}: Target is required`, 'error');
            return;
        }
    }

    const policyData = {
        name,
        description,
        team_id: isGlobalScope ? null : teamId,
        steps
    };

    const saveBtn = document.getElementById('save-policy-btn');
    if (saveBtn) {
        saveBtn.disabled = true;
        saveBtn.textContent = 'Saving...';
    }

    try {
        if (State.editingPolicy) {
            await API.policies.update(State.editingPolicy.id, policyData);
            showToast('Policy updated', 'success');
        } else {
            await API.policies.create(policyData);
            showToast('Policy created', 'success');
        }
        closePolicyEditor();
        loadPolicies();
    } catch (error) {
        showToast('Failed to save policy: ' + error.message, 'error');
    } finally {
        if (saveBtn) {
            saveBtn.disabled = false;
            saveBtn.textContent = 'Save Policy';
        }
    }
}

/**
 * Collect steps data from form
 */
function collectStepsData() {
    const steps = [];
    const stepRows = document.querySelectorAll('.policy-step-row');

    stepRows.forEach((row, index) => {
        // Sprint 4: the select encodes "<provider>:<target_kind>". Split and
        // send the two parts separately — the API now expects them as
        // distinct fields, not a combined step_type string.
        const raw = row.querySelector('.step-type-select')?.value || '';
        const [provider, targetKind] = raw.split(':');
        const targetType = row.querySelector('.target-type-select')?.value || 'user';
        const targetId = row.querySelector('.target-id-input')?.value || '';
        const delaySeconds = parseInt(row.querySelector('.delay-input')?.value || '0', 10);
        const timeoutSeconds = parseInt(row.querySelector('.timeout-input')?.value || '30', 10);
        const maxAttempts = parseInt(row.querySelector('.max-attempts-input')?.value || '5', 10);
        const message = row.querySelector('.message-input')?.value || '';
        const continueOnFailure = row.querySelector('.continue-on-failure-input')?.checked ?? true;

        steps.push({
            provider,
            target_kind: targetKind,
            target_type: targetType,
            target_id: targetId,
            delay_seconds: delaySeconds,
            timeout_seconds: timeoutSeconds,
            max_attempts: maxAttempts,
            message,
            continue_on_failure: continueOnFailure
        });
    });

    return steps;
}

/**
 * Handle delete policy
 */
export async function handleDeletePolicy(policyId) {
    if (!confirm('Delete this policy? This cannot be undone.')) return;

    try {
        // Check if policy is used in team routing
        const teamsResponse = await API.teams.list();
        const teams = teamsResponse.teams || [];

        for (const team of teams) {
            if (team.default_policy_id === policyId) {
                showToast(`Cannot delete: policy is default for team "${team.name}"`, 'error');
                return;
            }
            if (team.severity_routes) {
                for (const [severity, routePolicyId] of Object.entries(team.severity_routes)) {
                    if (routePolicyId === policyId) {
                        showToast(`Cannot delete: policy is used for ${severity} severity in team "${team.name}"`, 'error');
                        return;
                    }
                }
            }
        }

        await API.policies.delete(policyId);
        showToast('Policy deleted', 'success');
        loadPolicies();
    } catch (error) {
        showToast('Failed to delete policy: ' + error.message, 'error');
    }
}

/**
 * Open duplicate policy modal with team selector
 */
async function openDuplicateModal(policyId) {
    try {
        const policy = await API.policies.get(policyId);
        const teamsResponse = await API.teams.list();
        const teams = teamsResponse.teams || [];

        const modalHtml = `
            <div class="duplicate-policy-form">
                <h3>Duplicate Policy: ${policy.name}</h3>
                <div class="form-group">
                    <label>New Policy Name</label>
                    <input type="text" id="duplicate-policy-name" class="form-input" value="${policy.name} (Copy)">
                </div>
                <div class="form-group">
                    <label>Target Team</label>
                    <select id="duplicate-policy-team" class="form-select">
                        ${teams.map(t => `<option value="${t.id}" ${t.id === policy.team_id ? 'selected' : ''}>${t.name}</option>`).join('')}
                    </select>
                </div>
                <div class="form-actions">
                    <button class="btn btn-secondary" id="cancel-duplicate-btn">Cancel</button>
                    <button class="btn btn-primary" id="confirm-duplicate-btn">Duplicate</button>
                </div>
            </div>
        `;

        Elements.policyModalBody.innerHTML = modalHtml;
        Elements.policyModalOverlay.classList.add('active');

        document.getElementById('cancel-duplicate-btn').addEventListener('click', closePolicyEditor);
        document.getElementById('confirm-duplicate-btn').addEventListener('click', async () => {
            const newName = document.getElementById('duplicate-policy-name').value.trim();
            const targetTeamId = document.getElementById('duplicate-policy-team').value;

            if (!newName) {
                showToast('Name is required', 'error');
                return;
            }

            try {
                const newPolicy = {
                    name: newName,
                    description: policy.description,
                    team_id: targetTeamId,
                    // Sprint 4: policy steps carry (provider, target_kind)
                    // instead of the old combined step_type.
                    steps: policy.steps.map(s => ({
                        provider: s.provider,
                        target_kind: s.target_kind,
                        target_type: s.target_type,
                        target_id: s.target_id,
                        delay_seconds: s.delay_seconds,
                        timeout_seconds: s.timeout_seconds,
                        max_attempts: s.max_attempts,
                        message: s.message,
                        continue_on_failure: s.continue_on_failure ?? true
                    }))
                };

                await API.policies.create(newPolicy);
                showToast('Policy duplicated', 'success');
                closePolicyEditor();
                loadPolicies();
            } catch (error) {
                showToast('Failed to duplicate: ' + error.message, 'error');
            }
        });
    } catch (error) {
        showToast('Failed to load policy: ' + error.message, 'error');
    }
}

/**
 * Close policy editor modal
 */
function closePolicyEditor() {
    if (Elements.policyModalOverlay) {
        Elements.policyModalOverlay.classList.remove('active');
        document.body.style.overflow = '';
    }
    State.editingPolicy = null;
}

/**
 * Handle scope tab click
 */
function handleScopeTabClick(scope) {
    if (scope === currentPolicyScope) return;

    currentPolicyScope = scope;

    // Update tab UI
    const tabs = document.querySelectorAll('#policy-scope-tabs .scope-tab');
    tabs.forEach(tab => {
        if (tab.dataset.scope === scope) {
            tab.classList.add('active');
        } else {
            tab.classList.remove('active');
        }
    });

    // Re-render policies with new filter
    renderPolicies();
}

/**
 * Bind policy events (called from app.js)
 */
export function bindPoliciesEvents() {
    // Create policy button in section header
    if (Elements.createPolicyBtn) {
        Elements.createPolicyBtn.addEventListener('click', () => openPolicyEditor());
    }

    // Add policy button in sidebar
    if (Elements.addPolicyBtn) {
        Elements.addPolicyBtn.addEventListener('click', () => openPolicyEditor());
    }

    // Modal close on overlay click
    if (Elements.policyModalOverlay) {
        Elements.policyModalOverlay.addEventListener('click', (e) => {
            if (e.target === Elements.policyModalOverlay) {
                closePolicyEditor();
            }
        });
    }

    // Scope tabs - using delegation since tabs are in the HTML
    document.addEventListener('click', (e) => {
        const tab = e.target.closest('.scope-tab');
        if (tab && tab.dataset.scope) {
            handleScopeTabClick(tab.dataset.scope);
        }
    });
}
