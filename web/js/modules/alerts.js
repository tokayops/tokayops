/**
 * TokayOps Alerts Module
 * Alert group management and display
 */

import { State, STATE_STATUS_MAP } from '/js/core/state.js';
import { Elements, showToast, escapeHtml } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';

const STATUS_STATE_MAP = {
    'new': 'triggered',
    'processing': 'triggered',
    'triggered': 'triggered',
    'acknowledged': 'acknowledged',
    'resolved': 'resolved',
    'closed': 'resolved',
};

const STATE_PRIORITY = {
    'triggered': 3,
    'acknowledged': 2,
    'resolved': 1,
};

const SEVERITY_PRIORITY = {
    'critical': 3,
    'warning': 2,
    'info': 1,
};

const SORT_PAUSE_MS = 8000;
const HIGHLIGHT_MS = 8000;

let sortResumeTimer = null;
let highlightTimer = null;

function normalizeSeverity(severity) {
    const normalized = severity?.toLowerCase();
    return ['critical', 'warning', 'info'].includes(normalized) ? normalized : 'info';
}

function normalizeState(status) {
    return STATUS_STATE_MAP[status] || 'triggered';
}

function getDurationMs(alertGroup) {
    const start = alertGroup.created_at ? new Date(alertGroup.created_at).getTime() : 0;
    if (!start) return 0;
    const end = alertGroup.resolved_at ? new Date(alertGroup.resolved_at).getTime() : Date.now();
    return Math.max(0, end - start);
}

function getStateStatuses() {
    return STATE_STATUS_MAP[State.currentState] || STATE_STATUS_MAP.active;
}

function clearSortPause() {
    State.sortPauseUntil = 0;
    State.sortSnapshot = [];
    if (sortResumeTimer) {
        clearTimeout(sortResumeTimer);
        sortResumeTimer = null;
    }
}

function startSortPause() {
    const now = Date.now();
    State.sortPauseUntil = now + SORT_PAUSE_MS;

    if (State.lastSortedOrder && State.lastSortedOrder.length > 0) {
        State.sortSnapshot = [...State.lastSortedOrder];
    } else {
        State.sortSnapshot = State.alertGroupsRaw.map(ag => ag.id);
    }

    if (sortResumeTimer) {
        clearTimeout(sortResumeTimer);
    }
    sortResumeTimer = setTimeout(() => {
        if (Date.now() >= State.sortPauseUntil) {
            clearSortPause();
            applyAlertGroupFilters();
        }
    }, SORT_PAUSE_MS + 50);
}

function startHighlight(alertGroupId) {
    State.highlightedAlertGroupId = alertGroupId;
    State.highlightUntil = Date.now() + HIGHLIGHT_MS;
    State.highlightScrollPending = true;

    if (highlightTimer) {
        clearTimeout(highlightTimer);
    }
    highlightTimer = setTimeout(() => {
        if (State.highlightedAlertGroupId === alertGroupId && Date.now() >= State.highlightUntil) {
            State.highlightedAlertGroupId = null;
            State.highlightUntil = 0;
            State.highlightScrollPending = false;
            renderAlertGroups();
        }
    }, HIGHLIGHT_MS + 50);
}

function isHighlightActive(alertGroupId) {
    if (!State.highlightedAlertGroupId) return false;
    if (Date.now() >= State.highlightUntil) return false;
    return State.highlightedAlertGroupId === alertGroupId;
}

function setLingerAlertGroup(alertGroup) {
    if (!alertGroup || !alertGroup.id) return;
    State.lingerAlertGroup = alertGroup;
    State.lingerUntil = Date.now() + HIGHLIGHT_MS;
}

function isLingerActive() {
    if (!State.lingerAlertGroup) return false;
    return Date.now() < State.lingerUntil;
}

function mergeLingerGroup(groups) {
    if (!isLingerActive()) {
        State.lingerAlertGroup = null;
        State.lingerUntil = 0;
        return groups;
    }

    const linger = State.lingerAlertGroup;
    const exists = groups.some(ag => ag.id === linger.id);
    if (!exists) {
        return groups.concat(linger);
    }
    return groups;
}

/**
 * Show loading spinner
 */
function showLoading(show) {
    Elements.loadingState.style.display = show ? 'flex' : 'none';
    if (show) {
        Elements.alertGroupsGrid.innerHTML = '';
        Elements.emptyState.style.display = 'none';
    }
}

/**
 * Load alert groups from API
 */
export async function loadAlertGroups() {
    if (State.isLoading) return;

    State.isLoading = true;

    const isInitialLoad = State.alertGroupsRaw.length === 0;
    if (isInitialLoad) {
        showLoading(true);
    }

    try {
        const statuses = getStateStatuses();
        const severities = Object.entries(State.severityFilter)
            .filter(([_, v]) => v).map(([k]) => k);
        const sort = State.sortField !== 'default' ? State.sortField : undefined;
        const sort_dir = sort ? State.sortDirection : undefined;

        const response = await API.alertGroups.list({
            statuses: statuses.join(','),
            severity: severities.join(','),
            days: State.periodDays,
            view: 'summary',
            limit: State.pageSize,
            page: State.currentPage,
            sort,
            sort_dir,
        });

        State.alertGroupsRaw = mergeLingerGroup(response.alert_groups || []);
        State.currentPage = response.page;
        State.paginationMeta = {
            page: response.page,
            total_pages: response.total_pages,
            has_next: response.has_next,
            has_prev: response.has_prev,
            total: response.total,
        };

        applyAlertGroupFilters();
    } catch (error) {
        showToast(`Failed to load alert groups: ${error.message}`, 'error');
    } finally {
        State.isLoading = false;
        if (isInitialLoad) {
            showLoading(false);
        }
    }
}

/**
 * Load alert groups for a specific team
 */
export async function loadTeamAlertGroups(teamId) {
    if (State.isLoading) return;

    State.isLoading = true;
    const isInitialLoad = State.alertGroupsRaw.length === 0;
    if (isInitialLoad) showLoading(true);

    try {
        const statuses = getStateStatuses();
        const severities = Object.entries(State.severityFilter)
            .filter(([_, v]) => v).map(([k]) => k);
        const sort = State.sortField !== 'default' ? State.sortField : undefined;
        const sort_dir = sort ? State.sortDirection : undefined;

        const response = await API.teams.alertGroups(teamId, {
            statuses: statuses.join(','),
            severity: severities.join(','),
            days: State.periodDays,
            view: 'summary',
            limit: State.pageSize,
            page: State.currentPage,
            sort,
            sort_dir,
        });

        State.alertGroupsRaw = mergeLingerGroup(response.alert_groups || []);
        State.currentPage = response.page;
        State.paginationMeta = {
            page: response.page,
            total_pages: response.total_pages,
            has_next: response.has_next,
            has_prev: response.has_prev,
            total: response.total,
        };

        applyAlertGroupFilters();
    } catch (error) {
        showToast(`Failed to load team alert groups: ${error.message}`, 'error');
    } finally {
        State.isLoading = false;
        if (isInitialLoad) showLoading(false);
    }
}

function applyAlertGroupFilters() {
    State.alertGroups = State.alertGroupsRaw;
    updateSortHeaders();
    renderAlertGroups();
    updatePagination();
}

/**
 * Sort alert groups based on current state
 */
export function sortAlertGroups(groups) {
    const now = Date.now();
    const pauseActive = State.sortPauseUntil && now < State.sortPauseUntil;

    if (pauseActive && State.sortSnapshot && State.sortSnapshot.length > 0) {
        const order = new Map(State.sortSnapshot.map((id, index) => [id, index]));
        const sorted = [...groups].sort((a, b) => {
            const idxA = order.has(a.id) ? order.get(a.id) : Number.MAX_SAFE_INTEGER;
            const idxB = order.has(b.id) ? order.get(b.id) : Number.MAX_SAFE_INTEGER;
            return idxA - idxB;
        });
        updateSortHeaders();
        return sorted;
    }

    const field = State.sortField;
    const direction = State.sortDirection === 'asc' ? 1 : -1;
    const items = [...groups];

    items.sort((a, b) => {
        if (field === 'default') {
            const severityDiff = (SEVERITY_PRIORITY[normalizeSeverity(b.severity)] || 0)
                - (SEVERITY_PRIORITY[normalizeSeverity(a.severity)] || 0);
            if (severityDiff !== 0) return severityDiff;

            const stateDiff = (STATE_PRIORITY[normalizeState(b.status)] || 0)
                - (STATE_PRIORITY[normalizeState(a.status)] || 0);
            if (stateDiff !== 0) return stateDiff;

            // For resolved filter: sort by resolved_at (newest first)
            // For active alerts: sort by duration (shortest first)
            const aResolved = normalizeState(a.status) === 'resolved';
            const bResolved = normalizeState(b.status) === 'resolved';
            if (State.currentState === 'resolved' || (aResolved && bResolved)) {
                const timeA = a.resolved_at ? new Date(a.resolved_at).getTime() : 0;
                const timeB = b.resolved_at ? new Date(b.resolved_at).getTime() : 0;
                return timeB - timeA; // newest resolved first
            }
            return getDurationMs(a) - getDurationMs(b);
        }

        let valA = a[field];
        let valB = b[field];

        if (field === 'severity') {
            valA = SEVERITY_PRIORITY[normalizeSeverity(valA)] || 0;
            valB = SEVERITY_PRIORITY[normalizeSeverity(valB)] || 0;
        } else if (field === 'status') {
            valA = STATE_PRIORITY[normalizeState(valA)] || 0;
            valB = STATE_PRIORITY[normalizeState(valB)] || 0;
        } else if (field === 'created_at' || field === 'resolved_at') {
            valA = valA ? new Date(valA).getTime() : 0;
            valB = valB ? new Date(valB).getTime() : 0;
        } else {
            if (valA === undefined || valA === null) valA = '';
            if (valB === undefined || valB === null) valB = '';

            if (typeof valA === 'string') valA = valA.toLowerCase();
            if (typeof valB === 'string') valB = valB.toLowerCase();
        }

        if (valA < valB) return -1 * direction;
        if (valA > valB) return 1 * direction;
        return 0;
    });

    updateSortHeaders();
    State.lastSortedOrder = items.map(item => item.id);
    return items;
}

/**
 * Update sort header UI indicators
 */
function updateSortHeaders() {
    document.querySelectorAll('.list-header-col[data-sort]').forEach(col => {
        col.classList.remove('asc', 'desc');
        if (State.sortField !== 'default' && col.dataset.sort === State.sortField) {
            col.classList.add(State.sortDirection);
        }
    });
}

/**
 * Handle sort header click
 */
export function handleSort(field) {
    clearSortPause();
    if (State.sortField === field) {
        State.sortDirection = State.sortDirection === 'asc' ? 'desc' : 'asc';
    } else {
        if (field === 'created_at' || field === 'resolved_at' || field === 'severity' || field === 'status') {
            State.sortDirection = 'desc';
        } else {
            State.sortDirection = 'asc';
        }
        State.sortField = field;
    }

    State.currentPage = 1;
    if (State.selectedTeamId && State.selectedTeamId !== 'all') {
        loadTeamAlertGroups(State.selectedTeamId);
    } else {
        loadAlertGroups();
    }
}

/**
 * Render alert groups grid
 */
export function renderAlertGroups() {
    if (State.alertGroups.length === 0) {
        Elements.alertGroupsGrid.innerHTML = '';
        Elements.emptyState.style.display = 'flex';
        if (window.lucide) lucide.createIcons();
        return;
    }

    Elements.emptyState.style.display = 'none';
    Elements.alertGroupsGrid.innerHTML = State.alertGroups
        .map(ag => {
            return Components.alertGroupCard(ag, {
                highlight: isHighlightActive(ag.id),
                forceAlertsCount: State.viewMode === 'list',
                onCall: ag.oncall_snapshot
            });
        })
        .join('');

    if (window.lucide) lucide.createIcons();

    if (State.highlightScrollPending && State.highlightedAlertGroupId) {
        const target = Elements.alertGroupsGrid.querySelector(
            `[data-alert-group-id="${State.highlightedAlertGroupId}"]`
        );
        if (target) {
            State.highlightScrollPending = false;
            requestAnimationFrame(() => {
                target.scrollIntoView({ behavior: 'smooth', block: 'center', inline: 'nearest' });
            });
        }
    }
}

/**
 * Update pagination controls
 */
export function updatePagination() {
    const meta = State.paginationMeta;
    if (!meta || meta.total_pages <= 1 || ViewManager.getCurrent() !== 'alertGroups') {
        Elements.pagination.style.display = 'none';
        return;
    }

    Elements.pagination.style.display = 'flex';
    Elements.pageInfo.textContent = `Page ${meta.page} of ${meta.total_pages}`;
    Elements.prevPage.disabled = !meta.has_prev;
    Elements.nextPage.disabled = !meta.has_next;
}

// ========================================
// Alert Group Modal Functions
// ========================================

/**
 * Open alert group detail modal
 */
export async function openAlertGroupModal(alertGroupId) {
    // 1. Find summary in already-loaded data for instant display
    const summary = State.alertGroupsRaw.find(ag => ag.id === alertGroupId);

    // 2. Show modal immediately with summary content
    Elements.modalTitle.textContent = summary?.title || 'Loading...';
    if (summary) {
        summary.onCall = summary.oncall_snapshot;
        Elements.modalBody.innerHTML = Components.alertGroupDetail(summary);
        Elements.modalFooter.innerHTML = '';
    } else {
        Elements.modalBody.innerHTML = '<div class="loading-spinner">Loading...</div>';
        Elements.modalFooter.innerHTML = '';
    }
    Elements.modalOverlay.classList.add('active');
    document.body.style.overflow = 'hidden';

    // 3. Load full detail asynchronously
    try {
        const alertGroup = await API.alertGroups.get(alertGroupId);
        alertGroup.onCall = alertGroup.oncall_snapshot;
        State.selectedAlertGroup = alertGroup;
        Elements.modalTitle.textContent = alertGroup.title || 'Alert Group Details';
        Elements.modalBody.innerHTML = Components.alertGroupDetail(alertGroup);
        Elements.modalFooter.innerHTML = Components.alertGroupActions(alertGroup);
        bindModalActions();
        loadAlertGroupTimeline(alertGroupId);
    } catch (error) {
        Elements.modalBody.innerHTML = `<div class="empty-state"><p>Failed to load: ${escapeHtml(error.message)}</p></div>`;
    }
}

// ========================================
// Manual Alert Group Modal Functions
// ========================================

export function openManualAlertModal(options = {}) {
    const teams = State.teams || [];
    if (teams.length === 0) {
        showToast('No teams available', 'error');
        return;
    }

    const preferredTeamId = options.teamId
        || (State.selectedTeamId && State.selectedTeamId !== 'all' ? State.selectedTeamId : '')
        || teams[0].id;
    const severity = options.severity || 'critical';
    const title = options.title || 'Manual Alert';
    const teamLocked = !!options.teamLocked;

    Elements.manualAlertModalBody.innerHTML = Components.manualAlertGroupModal({
        teams,
        teamId: preferredTeamId,
        severity,
        title,
        teamLocked,
    });

    // Render footer
    Elements.manualAlertModalFooter.innerHTML = `
        <button type="button" class="btn btn-secondary" id="manual-alert-cancel">Cancel</button>
        <button type="submit" form="manual-alert-form" class="btn btn-primary" id="manual-alert-submit">Create Alert Group</button>
    `;

    Elements.manualAlertModalOverlay.classList.add('active');
    document.body.style.overflow = 'hidden';
    if (window.lucide) lucide.createIcons();

    bindManualAlertModalEvents();
}

function closeManualAlertModal() {
    if (Elements.manualAlertModalOverlay) {
        Elements.manualAlertModalOverlay.classList.remove('active');
        document.body.style.overflow = '';
    }
}

function bindManualAlertModalEvents() {
    const form = document.getElementById('manual-alert-form');
    if (form) {
        form.onsubmit = handleManualAlertCreate;
    }

    const cancelBtn = document.getElementById('manual-alert-cancel');
    if (cancelBtn) {
        cancelBtn.onclick = closeManualAlertModal;
    }

    if (Elements.manualAlertModalClose) {
        Elements.manualAlertModalClose.onclick = closeManualAlertModal;
    }

    if (Elements.manualAlertModalOverlay) {
        Elements.manualAlertModalOverlay.onclick = (e) => {
            if (e.target === Elements.manualAlertModalOverlay) {
                closeManualAlertModal();
            }
        };
    }
}

async function handleManualAlertCreate(e) {
    if (e) e.preventDefault();

    const teamId = document.getElementById('manual-alert-team')?.value || '';
    const severity = document.getElementById('manual-alert-severity')?.value || 'info';
    const title = document.getElementById('manual-alert-title')?.value?.trim() || '';

    if (!teamId) {
        showToast('Team is required', 'error');
        return;
    }

    const submitBtn = document.getElementById('manual-alert-submit');
    const originalText = submitBtn?.textContent;
    if (submitBtn) {
        submitBtn.disabled = true;
        submitBtn.textContent = 'Creating...';
    }

    try {
        const created = await API.alertGroups.create({
            team_id: teamId,
            severity: severity,
            title: title,
        });

        showToast('Manual alert group created', 'success');
        closeManualAlertModal();

        if (State.selectedTeamId && State.selectedTeamId !== 'all') {
            loadTeamAlertGroups(State.selectedTeamId);
        } else {
            loadAlertGroups();
        }

        if (created?.id && State.mode === 'ops') {
            openAlertGroupModal(created.id);
        }
    } catch (error) {
        showToast(`Failed to create manual alert: ${error.message}`, 'error');
    } finally {
        if (submitBtn) {
            submitBtn.disabled = false;
            submitBtn.textContent = originalText || 'Create Alert Group';
        }
    }
}

/**
 * Load and render timeline
 */
async function loadAlertGroupTimeline(alertGroupId) {
    const timelineContainer = document.getElementById('alert-group-timeline');
    if (!timelineContainer) return;

    try {
        const response = await API.alertGroups.timeline(alertGroupId);
        const events = response.events || [];
        timelineContainer.innerHTML = Components.timeline(events);
        if (window.lucide) lucide.createIcons();
    } catch (error) {
        timelineContainer.innerHTML = '<div class="timeline-empty">Failed to load timeline</div>';
        console.warn('Failed to load timeline:', error);
    }
}

/**
 * Close modal
 */
export function closeModal() {
    Elements.modalOverlay.classList.remove('active');
    document.body.style.overflow = '';
    State.selectedAlertGroup = null;

    const hash = window.location.hash || '';
    if (hash.startsWith('#/ops/alert-groups/')) {
        const baseHash = '#/ops/alert-groups';
        if (hash !== baseHash) {
            history.replaceState(null, '', baseHash);
        }
        State.lastRoutes.ops = baseHash;
    }
}

/**
 * Bind modal action buttons
 */
function bindModalActions() {
    const ackBtn = document.getElementById('action-ack');
    const resolveBtn = document.getElementById('action-resolve');

    if (ackBtn) {
        ackBtn.addEventListener('click', handleAcknowledge);
    }

    if (resolveBtn) {
        resolveBtn.addEventListener('click', handleResolve);
    }
}

/**
 * Acknowledge alert group
 */
async function handleAcknowledge(e) {
    const btn = e.currentTarget;
    const alertGroupId = btn.dataset.alertGroupId;

    btn.disabled = true;
    btn.textContent = 'Acknowledging...';

    try {
        const acknowledgedGroup = await API.alertGroups.ack(alertGroupId);
        showToast('Alert group acknowledged', 'success');
        if (acknowledgedGroup && acknowledgedGroup.id) {
            setLingerAlertGroup(acknowledgedGroup);
        }
        startHighlight(alertGroupId);
        startSortPause();
        closeModal();
        if (State.selectedTeamId && State.selectedTeamId !== 'all') {
            loadTeamAlertGroups(State.selectedTeamId);
        } else {
            loadAlertGroups();
        }
    } catch (error) {
        showToast(`Failed to acknowledge: ${error.message}`, 'error');
        btn.disabled = false;
        btn.textContent = '✓ Acknowledge';
    }
}

/**
 * Resolve alert group
 */
async function handleResolve(e) {
    const btn = e.currentTarget;
    const alertGroupId = btn.dataset.alertGroupId;
    const currentStatus = btn.dataset.alertGroupStatus;

    if (currentStatus === 'triggered') {
        const confirmed = window.confirm('Resolve this alert group without acknowledging?');
        if (!confirmed) {
            return;
        }
    }

    btn.disabled = true;
    btn.textContent = 'Resolving...';

    try {
        await API.alertGroups.resolve(alertGroupId);
        showToast('Alert group resolved', 'success');
        closeModal();
        if (State.selectedTeamId && State.selectedTeamId !== 'all') {
            loadTeamAlertGroups(State.selectedTeamId);
        } else {
            loadAlertGroups();
        }
    } catch (error) {
        showToast(`Failed to resolve: ${error.message}`, 'error');
        btn.disabled = false;
        btn.textContent = '✅ Resolve';
    }
}

/**
 * Bind Alert Module Events
 */
export function bindAlertsEvents() {
    // State Tabs (single-select)
    if (Elements.stateTabs) {
        Elements.stateTabs.addEventListener('click', (e) => {
            const tab = e.target.closest('.state-tab');
            if (!tab) return;

            const nextState = tab.dataset.state;
            if (!nextState || nextState === State.currentState) return;

            State.currentState = nextState;
            State.currentPage = 1;
            updateFilterActiveState();
            clearSortPause();

            if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                loadTeamAlertGroups(State.selectedTeamId);
            } else {
                loadAlertGroups();
            }
        });
    }

    // Period Tabs
    if (Elements.periodTabs) {
        Elements.periodTabs.addEventListener('click', (e) => {
            const tab = e.target.closest('.scope-tab-sm');
            if (!tab) return;

            const days = parseInt(tab.dataset.days, 10);
            if (!days || days === State.periodDays) return;

            State.periodDays = days;
            State.currentPage = 1;
            clearSortPause();
            updateFilterActiveState();

            if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                loadTeamAlertGroups(State.selectedTeamId);
            } else {
                loadAlertGroups();
            }
        });
    }

    // Severity Chips (multi-select) - reload from server
    if (Elements.severityChips) {
        Elements.severityChips.addEventListener('click', (e) => {
            const chip = e.target.closest('.severity-chip');
            if (!chip) return;

            const severity = chip.dataset.severity;
            if (!severity) return;

            const nextValue = !State.severityFilter[severity];
            const activeCount = Object.values(State.severityFilter).filter(Boolean).length;

            if (!nextValue && activeCount === 1) return;

            State.severityFilter[severity] = nextValue;
            State.currentPage = 1;
            updateFilterActiveState();
            clearSortPause();

            if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                loadTeamAlertGroups(State.selectedTeamId);
            } else {
                loadAlertGroups();
            }
        });
    }

    // List Header Sort Delegation
    const listHeader = document.getElementById('list-header');
    if (listHeader) {
        listHeader.addEventListener('click', (e) => {
            const col = e.target.closest('.list-header-col[data-sort]');
            if (col) {
                handleSort(col.dataset.sort);
            }
        });
    }

    // Pagination - reload from server
    if (Elements.prevPage) {
        Elements.prevPage.addEventListener('click', () => {
            if (State.isLoading || Elements.prevPage.disabled) return;
            State.currentPage--;
            if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                loadTeamAlertGroups(State.selectedTeamId);
            } else {
                loadAlertGroups();
            }
        });
    }
    if (Elements.nextPage) {
        Elements.nextPage.addEventListener('click', () => {
            if (State.isLoading || Elements.nextPage.disabled) return;
            State.currentPage++;
            if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                loadTeamAlertGroups(State.selectedTeamId);
            } else {
                loadAlertGroups();
            }
        });
    }

    updateFilterActiveState();
}

/**
 * Update visual state of filters
 */
function updateFilterActiveState() {
    if (Elements.stateTabs) {
        Elements.stateTabs.querySelectorAll('.state-tab').forEach(tab => {
            const isActive = tab.dataset.state === State.currentState;
            tab.classList.toggle('active', isActive);
            tab.setAttribute('aria-selected', isActive ? 'true' : 'false');
        });
    }

    if (Elements.severityChips) {
        Elements.severityChips.querySelectorAll('.severity-chip').forEach(chip => {
            const severity = chip.dataset.severity;
            const isActive = !!State.severityFilter[severity];
            chip.classList.toggle('active', isActive);
            chip.setAttribute('aria-pressed', isActive ? 'true' : 'false');
        });
    }

    if (Elements.periodTabs) {
        Elements.periodTabs.querySelectorAll('.scope-tab-sm').forEach(tab => {
            const isActive = parseInt(tab.dataset.days, 10) === State.periodDays;
            tab.classList.toggle('active', isActive);
            tab.setAttribute('aria-selected', isActive ? 'true' : 'false');
        });
    }
}
