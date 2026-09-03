/**
 * TokayOps Utility Functions
 * Shared utilities used across modules
 */

/**
 * DOM Elements cache
 */
export const Elements = {
    alertGroupsGrid: null,
    loadingState: null,
    emptyState: null,
    alertGroupsFilters: null,
    stateTabs: null,
    periodTabs: null,
    severityChips: null,
    refreshBtn: null,
    refreshCountdown: null,
    pageActions: null,
    modalOverlay: null,
    modalTitle: null,
    modalBody: null,
    modalFooter: null,
    modalClose: null,
    pagination: null,
    prevPage: null,
    nextPage: null,
    pageInfo: null,
    toastContainer: null,
    teamsNav: null,
    teamsSection: null,
    teamTitle: null,
    teamDescription: null,
    onCallContainer: null,
    usersSection: null,
    usersGrid: null,
    addUserBtn: null,
    userModalOverlay: null,
    userModalTitle: null,
    userModalBody: null,
    usersLoading: null,
    allTeamsSection: null,
    allTeamsGrid: null,
    allTeamsLoading: null,
    teamModalOverlay: null,
    teamModalTitle: null,
    teamModalBody: null,
    teamModalClose: null,
    viewGridBtn: null,
    viewListBtn: null,
    addTeamBtn: null,
    teamFormModalOverlay: null,
    teamFormModalClose: null,
    teamForm: null,
    viewToggle: null,
    // Policies elements
    policiesSection: null,
    policiesGrid: null,
    policiesLoading: null,
    createPolicyBtn: null,
    addPolicyBtn: null,
    policyModalOverlay: null,
    policyModalBody: null,
    policyModalClose: null,
    manualAlertModalOverlay: null,
    manualAlertModalBody: null,
    manualAlertModalClose: null,
    // Integration elements
    integrationsSection: null,
    integrationsGrid: null,
    integrationsLoading: null,
    createIntegrationBtn: null,
    integrationModalOverlay: null,
    integrationModalTitle: null,
    integrationModalBody: null,
    integrationModalClose: null,
    // Modal footers
    teamModalFooter: null,
    userModalFooter: null,
    teamFormModalFooter: null,
    profileModalFooter: null,
    policyModalFooter: null,
    manualAlertModalFooter: null,
    integrationModalFooter: null,
};

/**
 * Initialize DOM element references
 */
export function initElements() {
    Elements.alertGroupsGrid = document.getElementById('alert-groups-grid');
    Elements.loadingState = document.getElementById('loading-state');
    Elements.emptyState = document.getElementById('empty-state');
    Elements.alertGroupsFilters = document.getElementById('alert-groups-filters');
    Elements.stateTabs = document.getElementById('state-tabs');
    Elements.periodTabs = document.getElementById('period-tabs');
    Elements.severityChips = document.getElementById('severity-chips');
    Elements.refreshBtn = document.getElementById('refresh-btn');
    Elements.refreshCountdown = document.getElementById('refresh-countdown');
    Elements.pageActions = document.getElementById('page-actions');
    Elements.modalOverlay = document.getElementById('modal-overlay');
    Elements.modalTitle = document.getElementById('modal-title');
    Elements.modalBody = document.getElementById('modal-body');
    Elements.modalFooter = document.getElementById('modal-footer');
    Elements.modalClose = document.getElementById('modal-close');
    Elements.deliveryModalOverlay = document.getElementById('delivery-modal-overlay');
    Elements.deliveryModalTitle = document.getElementById('delivery-modal-title');
    Elements.deliveryModalBody = document.getElementById('delivery-modal-body');
    Elements.deliveryModalFooter = document.getElementById('delivery-modal-footer');
    Elements.deliveryModalClose = document.getElementById('delivery-modal-close');
    Elements.pagination = document.getElementById('pagination');
    Elements.prevPage = document.getElementById('prev-page');
    Elements.nextPage = document.getElementById('next-page');
    Elements.pageInfo = document.getElementById('page-info');
    Elements.toastContainer = document.getElementById('toast-container');
    Elements.teamsNav = document.getElementById('teams-nav');
    Elements.teamsSection = document.getElementById('teams-section');
    Elements.teamTitle = document.getElementById('team-title');
    Elements.teamDescription = document.getElementById('team-description');
    Elements.onCallContainer = document.getElementById('on-call-container');
    Elements.usersSection = document.getElementById('users-section');
    Elements.usersGrid = document.getElementById('users-grid');
    Elements.addUserBtn = document.getElementById('add-user-btn');
    Elements.userModalOverlay = document.getElementById('user-modal-overlay');
    Elements.userModalTitle = document.getElementById('user-modal-title');
    Elements.userModalBody = document.getElementById('user-modal-body');
    Elements.usersLoading = document.getElementById('users-loading');
    Elements.allTeamsSection = document.getElementById('all-teams-section');
    Elements.allTeamsGrid = document.getElementById('all-teams-grid');
    Elements.allTeamsLoading = document.getElementById('all-teams-loading');
    Elements.teamModalOverlay = document.getElementById('team-modal-overlay');
    Elements.teamModalTitle = document.getElementById('team-modal-title');
    Elements.teamModalBody = document.getElementById('team-modal-body');
    Elements.teamModalClose = document.getElementById('team-modal-close');
    Elements.viewGridBtn = document.getElementById('view-grid');
    Elements.viewListBtn = document.getElementById('view-list');
    Elements.addTeamBtn = document.getElementById('add-team-btn');
    Elements.teamFormModalOverlay = document.getElementById('team-form-modal-overlay');
    Elements.teamFormModalClose = document.getElementById('team-form-modal-close');
    Elements.teamForm = document.getElementById('team-form');
    Elements.viewToggle = document.getElementById('view-toggle');
    // Policies elements
    Elements.policiesSection = document.getElementById('policies-section');
    Elements.policiesGrid = document.getElementById('policies-grid');
    Elements.policiesLoading = document.getElementById('policies-loading');
    Elements.createPolicyBtn = document.getElementById('create-policy-btn');
    Elements.addPolicyBtn = document.getElementById('add-policy-btn');
    Elements.policyModalOverlay = document.getElementById('policy-modal-overlay');
    Elements.policyModalBody = document.getElementById('policy-modal-body');
    Elements.policyModalClose = document.getElementById('policy-modal-close');
    Elements.manualAlertModalOverlay = document.getElementById('manual-alert-modal-overlay');
    Elements.manualAlertModalBody = document.getElementById('manual-alert-modal-body');
    Elements.manualAlertModalClose = document.getElementById('manual-alert-modal-close');
    // Integration elements
    Elements.integrationsSection = document.getElementById('integrations-section');
    Elements.integrationsGrid = document.getElementById('integrations-grid');
    Elements.integrationsLoading = document.getElementById('integrations-loading');
    Elements.createIntegrationBtn = document.getElementById('create-integration-btn');
    Elements.integrationModalOverlay = document.getElementById('integration-modal-overlay');
    Elements.integrationModalTitle = document.getElementById('integration-modal-title');
    Elements.integrationModalBody = document.getElementById('integration-modal-body');
    Elements.integrationModalClose = document.getElementById('integration-modal-close');
    // Modal footers
    Elements.teamModalFooter = document.getElementById('team-modal-footer');
    Elements.userModalFooter = document.getElementById('user-modal-footer');
    Elements.teamFormModalFooter = document.getElementById('team-form-modal-footer');
    Elements.profileModalFooter = document.getElementById('profile-modal-footer');
    Elements.policyModalFooter = document.getElementById('policy-modal-footer');
    Elements.manualAlertModalFooter = document.getElementById('manual-alert-modal-footer');
    Elements.integrationModalFooter = document.getElementById('integration-modal-footer');

    // UI Mode Elements
    Elements.sidebarHeaderControls = document.getElementById('sidebar-header-controls');
    Elements.sidebarNav = document.getElementById('sidebar-nav');
    Elements.configureDashboard = document.getElementById('configure-dashboard');
    Elements.opsOnCallView = document.getElementById('ops-on-call-view');
    Elements.opsActivityView = document.getElementById('ops-activity-view');
    Elements.cfgIntegrationsView = document.getElementById('cfg-integrations-view');

    // View Toggles
    Elements.viewGridBtn = document.getElementById('view-grid');
    Elements.viewListBtn = document.getElementById('view-list');
    Elements.alertGroupsContainer = document.querySelector('.alert-groups-container');
    Elements.viewToggle = document.getElementById('view-toggle');
}

/**
 * Show toast notification
 */
export function showToast(message, type = 'info') {
    const toast = document.createElement('div');
    toast.innerHTML = Components.toast(message, type);
    const toastElement = toast.firstElementChild;

    Elements.toastContainer.appendChild(toastElement);

    setTimeout(() => {
        toastElement.style.opacity = '0';
        toastElement.style.transform = 'translateX(100%)';
        setTimeout(() => toastElement.remove(), 300);
    }, 4000);
}

// Export to window for non-module scripts
window.showToast = showToast;

/**
 * Escape HTML to prevent XSS
 */
export function escapeHtml(str) {
    if (!str) return '';
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

/**
 * Escape a string for use inside a double-quoted HTML attribute.
 *
 * Escapes the quotes as well as the angle brackets, which is what separates it
 * from escapeHtml: a value that only has to be safe as text is not safe as an
 * attribute. The classic-script side of the app carries its own copy, because
 * a script that is not a module cannot import this one.
 */
export function escapeAttr(str) {
    if (!str) return '';
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;');
}

/**
 * Open a modal with standardized behavior
 * @param {string} modalId - Modal overlay ID
 */
export function openModal(modalId) {
    const overlay = document.getElementById(modalId);
    if (!overlay) return;
    overlay.classList.add('active');
    document.body.style.overflow = 'hidden';
}

/**
 * Close a modal with cleanup
 * @param {string} modalId - Modal overlay ID
 */
export function closeModalById(modalId) {
    const overlay = document.getElementById(modalId);
    if (!overlay) return;
    overlay.classList.remove('active');
    document.body.style.overflow = '';
}

/**
 * Initialize global modal escape handler
 */
export function initModalEscapeHandler() {
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
            const activeModal = document.querySelector('.modal-overlay.active');
            if (activeModal) {
                e.preventDefault();
                const closeBtn = activeModal.querySelector('.modal-close');
                if (closeBtn) closeBtn.click();
            }
        }
    });
}

/**
 * Initialize Theme
 */
export function initTheme() {
    const savedTheme = localStorage.getItem('tokay.theme') || 'light';
    document.documentElement.setAttribute('data-theme', savedTheme);
    updateThemeIcon(savedTheme);
}

/**
 * Toggle Theme
 */
export function toggleTheme() {
    const currentTheme = document.documentElement.getAttribute('data-theme') || 'light';
    const newTheme = currentTheme === 'light' ? 'dark' : 'light';

    document.documentElement.setAttribute('data-theme', newTheme);
    localStorage.setItem('tokay.theme', newTheme);

    updateThemeIcon(newTheme);
}

function updateThemeIcon(theme) {
    // CSS handles visibility based on [data-theme] attribute
    // We don't need to manually verify classes if using .theme-icon-light/.theme-icon-dark
    if (window.lucide) {
        lucide.createIcons();
    }
}
