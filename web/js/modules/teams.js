/**
 * TokayOps Teams Module
 * Team management and navigation functionality
 */

import { State } from '/js/core/state.js';
import { Elements, showToast } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';
import { loadTeamAlertGroups } from '/js/modules/alerts.js';

/**
 * Load teams from API
 */
export async function loadTeams() {
    try {
        const response = await API.teams.list();
        State.teams = response.teams || [];
        renderTeamsNav();
    } catch (error) {
        console.warn('Failed to load teams:', error);
    }
}

/**
 * Render teams navigation in sidebar - NOW HANDLED BY APP.JS (Dynamic Sidebar)
 * Only updates event listeners if needed, or can be removed if handled globally
 */
export function renderTeamsNav() {
    document.dispatchEvent(new CustomEvent('tokay:teams-updated'));
}

/**
 * Show all teams view (CONFIGURE MODE)
 */
export async function showTeamsView() {
    // Only allowed in Config mode
    if (State.mode !== 'cfg') return;

    // Ensure All Teams Section exists
    let allTeamsSection = document.getElementById('all-teams-section');
    if (!allTeamsSection) {
        allTeamsSection = document.createElement('div');
        allTeamsSection.id = 'all-teams-section';
        allTeamsSection.className = 'users-section';
        allTeamsSection.innerHTML = Components.allTeamsSection();
        document.querySelector('.main-content').appendChild(allTeamsSection);

        allTeamsSection.querySelector('#create-team-view-btn').addEventListener('click', () => {
            Elements.teamFormModalOverlay.classList.add('active');
            document.body.style.overflow = 'hidden';
        });
    }

    ViewManager.show('allTeams', { showStats: false, showViewToggle: false });

    // Load and render teams
    const loading = document.getElementById('all-teams-loading');
    const grid = document.getElementById('all-teams-grid');

    if (loading) loading.style.display = 'flex';
    if (grid) grid.innerHTML = '';

    try {
        await loadTeams();
        if (grid) {
            if (State.teams.length === 0) {
                grid.innerHTML = Components.emptyTeamsState();
            } else {
                grid.innerHTML = Components.teamsList(State.teams);
            }

            bindTeamCardEvents(grid);
            if (window.lucide) lucide.createIcons();
        }
    } catch (e) {
        showToast('Failed to load teams view', 'error');
    } finally {
        if (loading) loading.style.display = 'none';
    }
}

/**
 * Bind events on team cards
 */
function bindTeamCardEvents(grid) {
    grid.querySelectorAll('.manage-team-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            openTeamModal(btn.dataset.teamId);
        });
    });

    grid.querySelectorAll('.delete-team-btn').forEach(btn => {
        btn.addEventListener('click', async (e) => {
            e.stopPropagation();
            if (confirm('Delete this team? This cannot be undone.')) {
                try {
                    await API.teams.delete(btn.dataset.teamId);
                    showToast('Team deleted', 'success');
                    loadTeams();
                    showTeamsView();
                } catch (err) {
                    showToast('Failed to delete team: ' + err.message, 'error');
                }
            }
        });
    });

    // Clicking card in Config mode opens View/Manage Modal
    grid.querySelectorAll('.team-card').forEach(card => {
        card.addEventListener('click', (e) => {
            if (e.target.closest('button')) return;
            const teamId = card.dataset.teamId;
            if (teamId) openTeamModal(teamId);
        });
    });
}

/**
 * Select a specific team (OPERATIONS MODE context switch)
 */
export function selectTeam(teamId) {
    State.selectedTeamId = teamId;
    State.currentPage = 1;

    // Persist choice
    localStorage.setItem('tokay.selectedTeamId', teamId);

    const teamName = teamId === 'all' ? 'All Teams' : (State.teams.find(t => t.id === teamId)?.name || teamId);
    showToast(`Switched context to: ${teamName}`);

    // Dispatch context change event for all modes
    document.dispatchEvent(new CustomEvent('tokay:context-changed'));
}

/**
 * Clear team filter and show all alert groups
 */
export function clearTeamFilter() {
    State.currentTeam = null;
    State.currentPage = 1;
    State.currentView = 'alertGroups';

    ViewManager.show('alerts');
}

/**
 * Load team members
 */
export async function loadTeamMembers(teamId, force = false) {
    try {
        if (!force && State.teamMembers[teamId]) {
            renderOnCall(teamId, State.teamMembers[teamId]);
            return;
        }

        const response = await API.teams.members(teamId);
        const members = response.users || [];
        State.teamMembers[teamId] = members;
        renderOnCall(teamId, members);
    } catch (e) {
        console.warn('Failed to load team members', e);
    }
}

/**
 * Render on-call widget with schedule data
 */
async function renderOnCall(teamId, members) {
    if (!Elements.onCallContainer) return;

    try {
        // Fetch schedule and on-call data (both may 404 if not configured)
        const [schedule, onCallResult] = await Promise.all([
            API.schedules.get(teamId).catch(() => null),
            API.schedules.getOnCall(teamId).catch(() => null)
        ]);

        // Always use new widget - it handles null gracefully and shows setup prompt
        Elements.onCallContainer.innerHTML = Components.onCallWidget(schedule, onCallResult, teamId);
        if (window.lucide) lucide.createIcons();
    } catch (e) {
        console.warn('Failed to load on-call data', e);
        // Show widget with null data (will display "Not configured" prompt)
        Elements.onCallContainer.innerHTML = Components.onCallWidget(null, null, teamId);
        if (window.lucide) lucide.createIcons();
    }
}

/**
 * Open team management modal
 */
export async function openTeamModal(teamId) {
    try {
        const team = State.teams.find(t => t.id === teamId);

        // Ensure latest members are loaded
        await loadTeamMembers(teamId, true);
        const members = State.teamMembers[teamId] || [];

        // Load users if needed
        if (State.users.length === 0) {
            const resp = await API.users.list();
            State.users = resp.users || [];
        }

        // Load policies for this team
        const policiesResp = await API.policies.list();
        const allPolicies = policiesResp || [];
        // Filter to team's policies only
        console.log('Team ID:', teamId, 'All policies:', allPolicies.map(p => ({ id: p.id, name: p.name, team_id: p.team_id })));
        const teamPolicies = allPolicies.filter(p => p.team_id === teamId);

        Elements.teamModalBody.innerHTML = Components.teamManagementModal(team, members, State.users, teamPolicies);

        // Render footer
        Elements.teamModalFooter.innerHTML = `
            <button type="button" class="btn btn-secondary" id="team-modal-close-btn">Close</button>
        `;

        Elements.teamModalOverlay.classList.add('active');
        document.body.style.overflow = 'hidden';

        if (window.lucide) lucide.createIcons();

        bindTeamModalEvents(teamId);
    } catch (e) {
        showToast('Failed to open team management', 'error');
    }
}

/**
 * Bind events in team modal
 */
function bindTeamModalEvents(teamId) {
    const modal = Elements.teamModalBody;

    // Add Member
    const addBtn = modal.querySelector('#add-member-btn');
    if (addBtn) {
        addBtn.addEventListener('click', async () => {
            const userId = modal.querySelector('#add-member-select').value;
            const role = modal.querySelector('#add-member-role').value;
            if (!userId) return;

            try {
                await API.teams.addMember(teamId, userId, role);
                showToast('Member added', 'success');
                delete State.teamMembers[teamId];
                await loadTeamMembers(teamId);
                openTeamModal(teamId);
            } catch (error) {
                showToast(error.message, 'error');
            }
        });
    }

    // Remove Member
    modal.querySelectorAll('.remove-member-btn').forEach(btn => {
        btn.addEventListener('click', async () => {
            const userId = btn.dataset.userId;
            if (!confirm('Remove this user from the team?')) return;

            try {
                await API.teams.removeMember(teamId, userId);
                showToast('Member removed', 'success');
                delete State.teamMembers[teamId];
                await loadTeamMembers(teamId);
                openTeamModal(teamId);
            } catch (error) {
                showToast(error.message, 'error');
            }
        });
    });

    // Update Member Role
    modal.querySelectorAll('.role-select').forEach(select => {
        select.addEventListener('change', async (e) => {
            const userId = select.dataset.userId;
            const newRole = e.target.value;

            try {
                // Re-add member acts as upsert/update for role
                await API.teams.addMember(teamId, userId, newRole);
                showToast('Role updated', 'success');
                delete State.teamMembers[teamId];
                // No need to reload modal as the select value is already changed
            } catch (error) {
                showToast(error.message, 'error');
                // Revert selection on error
                // We'd need to know the old value to revert accurately, but reloading is safer
                await loadTeamMembers(teamId);
                openTeamModal(teamId);
            }
        });
    });

    // Save Routing
    const saveRoutingBtn = modal.querySelector('#save-routing-btn');
    if (saveRoutingBtn) {
        saveRoutingBtn.addEventListener('click', async () => {
            const defaultPolicyId = modal.querySelector('#routing-default-policy')?.value || null;
            const criticalPolicy = modal.querySelector('#routing-critical')?.value || null;
            const warningPolicy = modal.querySelector('#routing-warning')?.value || null;
            const infoPolicy = modal.querySelector('#routing-info')?.value || null;

            const severityRoutes = {};
            if (criticalPolicy) severityRoutes.critical = criticalPolicy;
            if (warningPolicy) severityRoutes.warning = warningPolicy;
            if (infoPolicy) severityRoutes.info = infoPolicy;

            try {
                await API.teams.update(teamId, {
                    default_policy_id: defaultPolicyId || null,
                    // Always send the map (even empty) so the backend replaces it —
                    // setting all severities to "Use default" must CLEAR existing
                    // routes, otherwise a stale route shadows the Default Policy.
                    severity_routes: severityRoutes
                });
                showToast('Routing saved', 'success');
                // Refresh teams in state
                await loadTeams();
            } catch (error) {
                showToast('Failed to save routing: ' + error.message, 'error');
            }
        });
    }

    // Delete Team (in modal)
    const deleteTeamBtn = modal.querySelector('.delete-team-modal-btn');
    if (deleteTeamBtn) {
        deleteTeamBtn.addEventListener('click', async () => {
            if (confirm('Delete this team? This cannot be undone.')) {
                try {
                    await API.teams.delete(teamId);
                    showToast('Team deleted', 'success');
                    Elements.teamModalOverlay.classList.remove('active');
                    document.body.style.overflow = '';
                    await loadTeams();
                    showTeamsView();
                } catch (err) {
                    showToast('Failed to delete team: ' + err.message, 'error');
                }
            }
        });
    }

    // Close button in footer
    const closeBtn = document.getElementById('team-modal-close-btn');
    if (closeBtn) {
        closeBtn.addEventListener('click', () => {
            Elements.teamModalOverlay.classList.remove('active');
            document.body.style.overflow = '';
        });
    }
}

/**
 * Handle create team form submission
 */
export async function handleCreateTeam(e) {
    e.preventDefault();
    const formData = new FormData(e.target);
    const data = Object.fromEntries(formData.entries());

    if (!data.id || !data.name) {
        showToast('Team ID and Name are required', 'error');
        return;
    }

    if (!/^[a-z0-9-]+$/.test(data.id)) {
        showToast('Team ID must be lowercase, alphanumeric, and can contain hyphens', 'error');
        return;
    }

    const btn = document.getElementById('team-form-submit');
    const originalText = btn.textContent;
    btn.disabled = true;
    btn.textContent = 'Creating...';

    try {
        await API.teams.create(data);
        showToast('Team created successfully', 'success');
        Elements.teamFormModalOverlay.classList.remove('active');
        document.body.style.overflow = '';
        Elements.teamForm.reset();
        await loadTeams();

        // Refresh All Teams view if visible
        const grid = document.getElementById('all-teams-grid');
        if (grid && State.currentView === 'allTeams') {
            grid.innerHTML = Components.teamsList(State.teams);
            bindTeamCardEvents(grid);
            if (window.lucide) lucide.createIcons();
        }
    } catch (error) {
        showToast(`Failed to create team: ${error.message}`, 'error');
    } finally {
        btn.disabled = false;
        btn.textContent = originalText;
    }
}

/**
 * Bind team modal close button and form submit
 */
export function bindTeamModalClose() {
    if (Elements.teamModalClose) {
        Elements.teamModalClose.addEventListener('click', () => {
            Elements.teamModalOverlay.classList.remove('active');
            document.body.style.overflow = '';
        });
    }
    // Also bind the team FORM modal close (Create Team modal)
    if (Elements.teamFormModalClose) {
        Elements.teamFormModalClose.addEventListener('click', () => {
            Elements.teamFormModalOverlay.classList.remove('active');
            document.body.style.overflow = '';
        });
    }
    // Bind Cancel button in Team Form modal footer
    const teamFormCancel = document.getElementById('team-form-cancel');
    if (teamFormCancel) {
        teamFormCancel.addEventListener('click', () => {
            Elements.teamFormModalOverlay.classList.remove('active');
            document.body.style.overflow = '';
        });
    }
    // Bind create team form submit
    if (Elements.teamForm) {
        Elements.teamForm.addEventListener('submit', handleCreateTeam);
    }
}
