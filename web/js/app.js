/**
 * TokayOps Application - Main Entry Point
 * Orchestrates all modules and handles initialization
 * 
 * Phase 2: Modular Architecture with ES Modules
 */

import { State } from '/js/core/state.js';
import { Elements, initElements, showToast, initTheme, toggleTheme, escapeHtml, initModalEscapeHandler } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';
import { Permissions } from '/js/modules/permissions.js';
import {
    loadAlertGroups,
    loadTeamAlertGroups,
    openAlertGroupModal,
    openManualAlertModal,
    closeModal,
    bindAlertsEvents
} from '/js/modules/alerts.js';
import {
    loadTeams,
    showTeamsView,
    selectTeam,
    clearTeamFilter,
    handleCreateTeam,
    bindTeamModalClose
} from '/js/modules/teams.js';
import { bindUsersEvents, showUsersView } from '/js/modules/users.js';
import { bindScheduleEvents, loadOnCallOverviewRow, loadOnCallOverviewRows } from '/js/modules/schedules.js';
import { bindPoliciesEvents, showPoliciesView } from '/js/modules/policies.js';
import { bindIntegrationsEvents, showIntegrationsView } from '/js/modules/integrations.js';

// ========================================
// Auto-Refresh
// ========================================

// ========================================
// Auto-Refresh
// ========================================

function startAutoRefresh() {
    State.refreshCountdown = 10;
    updateCountdownDisplay();

    if (State.refreshInterval) clearInterval(State.refreshInterval);

    State.refreshInterval = setInterval(() => {
        State.refreshCountdown--;
        updateCountdownDisplay();

        if (State.refreshCountdown <= 0) {
            // Only refresh alerts in Ops mode
            if (State.mode === 'ops' && State.currentView === 'alertGroups') {
                if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                    loadTeamAlertGroups(State.selectedTeamId);
                } else {
                    loadAlertGroups();
                }
            }
            State.refreshCountdown = 10;
        }
    }, 1000);
}

function updateCountdownDisplay() {
    if (Elements.refreshCountdown) Elements.refreshCountdown.textContent = State.refreshCountdown;
}

// ========================================
// Rendering & Navigation
// ========================================

/**
 * Render Sidebar Navigation based on current Mode
 */
function renderSidebar() {
    const userRole = Permissions.user?.role || 'user'; // 'admin' or 'user' (simplified)
    const teamId = State.selectedTeamId;

    // Render dynamic nav
    Elements.sidebarNav.innerHTML = Components.sidebarNav(State.mode, teamId);

    // Update active state based on hash
    const currentHash = window.location.hash;
    Elements.sidebarNav.querySelectorAll('.nav-item').forEach(item => {
        if (item.getAttribute('href') === currentHash) {
            item.classList.add('active');
        } else {
            // Handle partial matches for deeper routes if needed, simple equality for now
            item.classList.remove('active');
        }
    });

    if (window.lucide) lucide.createIcons();
}


/**
 * Render Mode Switcher in Header (global level)
 */
function renderModeSwitcher() {
    const headerModeSwitcher = document.getElementById('header-mode-switcher');
    if (headerModeSwitcher) {
        headerModeSwitcher.innerHTML = Components.modeSwitcher(State.mode);
        if (window.lucide) lucide.createIcons();
    }
}

/**
 * Render Sidebar Controls (Team Selector only, Mode is now in header)
 * Only shown in Operations mode - Configure mode shows all data globally
 */
function renderHeaderControls() {
    // Team Selector - only visible in Operations mode
    if (State.mode === 'ops') {
        Elements.sidebarHeaderControls.innerHTML = Components.teamContextSelector(State.teams, State.selectedTeamId);
        Elements.sidebarHeaderControls.style.display = '';
    } else {
        Elements.sidebarHeaderControls.innerHTML = '';
        Elements.sidebarHeaderControls.style.display = 'none';
    }

    if (window.lucide) lucide.createIcons();
}

/**
 * Switch Application Mode
 */
function switchMode(newMode) {
    if (State.mode === newMode) return;

    // Guard: Config access - use canAccessConfigure which handles team_admin
    if (newMode === 'cfg' && !Permissions.canAccessConfigure()) {
        showToast('Access Denied: Configure mode is restricted', 'error');
        return;
    }

    State.mode = newMode;
    localStorage.setItem('tokay.mode', newMode);

    // Switch URL to last known route for this mode
    const targetRoute = State.lastRoutes[newMode] || (newMode === 'ops' ? '#/ops/alert-groups' : '#/cfg/integrations');
    window.location.hash = targetRoute;

    renderSidebar();
    renderModeSwitcher();
    renderHeaderControls();

    // ViewManager update handled by route change
}

// ========================================
// Strict Routing
// ========================================

/**
 * Handle hash-based URL routing (Strict Mode)
 * Routes: #/ops/..., #/cfg/...
 */
function handleHashRoute() {
    const hash = window.location.hash;

    // 1. Root / Empty -> Redirect to default
    if (!hash || hash === '#/') {
        const defaultMode = localStorage.getItem('tokay.mode') || 'ops';
        window.location.hash = defaultMode === 'cfg' && Permissions.canAccessConfigure() ? '#/cfg/integrations' : '#/ops/alert-groups';
        return;
    }

    // 2. Parse Mode
    const modeMatch = hash.match(/^#\/(ops|cfg)(\/.*)?$/);
    if (!modeMatch) {
        // Legacy or unknown -> Redirect to Ops
        console.warn('Unknown route, redirecting to Ops:', hash);
        window.location.hash = '#/ops/alert-groups';
        return;
    }

    const mode = modeMatch[1];
    const path = modeMatch[2] || ''; // e.g., /alert-groups/123

    // 3. RBAC Guard for Config - use canAccessConfigure which handles team_admin
    if (mode === 'cfg' && !Permissions.canAccessConfigure()) {
        showToast('No access to Configure', 'error');
        window.location.hash = '#/ops/alert-groups';
        return;
    }

    // 4. Update State
    State.mode = mode;
    State.lastRoutes[mode] = hash;
    localStorage.setItem('tokay.mode', mode);

    renderSidebar();
    renderModeSwitcher(); // Update header mode switcher
    renderHeaderControls(); // Update sidebar team selector

    // 5. Route Handling
    if (mode === 'ops') {
        if (path.startsWith('/alert-groups')) {
            // Check for ID: /alert-groups/:id
            const idMatch = path.match(/^\/alert-groups\/(.+)$/);

            // Show Alert Groups View
            ViewManager.show('alertGroups');
            renderAlertGroupsActions();
            const selectedTeamName = (State.selectedTeamId && State.selectedTeamId !== 'all')
                ? (State.teams.find(t => t.id === State.selectedTeamId)?.name || State.selectedTeamId)
                : '';
            const alertGroupsTitle = selectedTeamName
                ? `Alert Groups - ${selectedTeamName}`
                : 'Alert Groups';
            updatePageTitle(alertGroupsTitle);

            // If ID present, open modal
            if (idMatch) {
                const id = decodeURIComponent(idMatch[1]);
                setTimeout(() => openAlertGroupModal(id), 100);
            }
        } else if (path.startsWith('/oncall')) {
            // Ops On-Call View
            ViewManager.show('oncall', { showStats: false, showViewToggle: false });
            clearPageActions();
            updatePageTitle('On-Call');
            if (Elements.opsOnCallView) {
                // If a team is selected, show its schedule
                if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                    const selectedTeam = State.teams.find(t => t.id === State.selectedTeamId);
                    const teamName = selectedTeam?.name || State.selectedTeamId;
                    Elements.opsOnCallView.innerHTML = `
                         <div class="section-header">
                            <h2 class="section-title"><i data-lucide="calendar"></i> On-Call: ${escapeHtml(teamName)}</h2>
                        </div>
                        <div id="ops-on-call-content" class="on-call-overview"></div>
                    `;
                    const container = document.getElementById('ops-on-call-content');
                    if (container && selectedTeam) {
                        loadOnCallOverviewRow(selectedTeam, container);
                    }
                } else {
                    Elements.opsOnCallView.innerHTML = `
                        <div class="section-header">
                            <h2 class="section-title"><i data-lucide="calendar"></i> On-Call: All Teams</h2>
                        </div>
                        <div id="ops-on-call-content" class="on-call-overview"></div>
                    `;
                    const container = document.getElementById('ops-on-call-content');
                    if (container) {
                        if (!State.teams || State.teams.length === 0) {
                            container.innerHTML = `
                                <div class="empty-state">
                                    <i data-lucide="calendar-clock" class="empty-icon"></i>
                                    <p>No teams available</p>
                                </div>
                            `;
                        } else {
                            const teams = [...State.teams].sort((a, b) =>
                                (a.name || a.id || '').localeCompare(b.name || b.id || '')
                            );
                            container.innerHTML = `
                                ${Components.onCallListHeader()}
                                <div class="oncall-list-body">
                                    ${teams.map(team => `
                                        <div class="oncall-row-slot" data-team-id="${escapeHtml(team.id)}"></div>
                                    `).join('')}
                                </div>
                            `;

                            // All rows together, so the names they mention are
                            // looked up once for the page rather than once per
                            // row. A team whose state cannot be read still gets
                            // its own row saying so.
                            loadOnCallOverviewRows(teams, (teamId) => {
                                const safeId = typeof CSS !== 'undefined' && CSS.escape
                                    ? CSS.escape(teamId) : teamId;
                                return container.querySelector(`.oncall-row-slot[data-team-id="${safeId}"]`);
                            });
                        }
                    }
                    if (window.lucide) lucide.createIcons();
                }
            }
        } else if (path.startsWith('/activity')) {
            // Ops Activity View
            ViewManager.show('activity', { showStats: false, showViewToggle: false });
            clearPageActions();
            updatePageTitle('Activity Log');
            if (Elements.opsActivityView) {
                Elements.opsActivityView.innerHTML = `
                    <div class="section-header">
                        <h2 class="section-title"><i data-lucide="activity"></i> Activity Log</h2>
                    </div>
                    <div class="empty-state">
                        <i data-lucide="construction" class="empty-icon"></i>
                        <p>Global activity log coming soon</p>
                    </div>
                `;
                if (window.lucide) lucide.createIcons();
            }
        }
    } else if (mode === 'cfg') {
        clearPageActions();
        // Configure Dashboard (Tiles) handling
        if (path === '' || path === '/') {
            // Render stats for titles
            const stats = {
                teamsCount: State.teams.length,
                policiesCount: State.policies?.length || 0 // If loaded
            };
            Elements.configureDashboard.innerHTML = Components.configureDashboard(stats);
            ViewManager.show('configureDashboard');
            updatePageTitle('Configuration');
            if (window.lucide) lucide.createIcons();
        } else if (path.startsWith('/teams')) {
            showTeamsView(); // Modules/teams.js
            updatePageTitle('Teams');
        } else if (path.startsWith('/policies')) {
            showPoliciesView(); // Modules/policies.js
            updatePageTitle('Escalation Policies');
        } else if (path.startsWith('/users')) {
            if (Permissions.isAdmin()) {
                showUsersView();
                updatePageTitle('User Management');
            } else {
                window.location.hash = '#/cfg';
            }
        } else if (path.startsWith('/integrations')) {
            if (Permissions.canAccessConfigure()) {
                showIntegrationsView();
                updatePageTitle('Integrations');
            } else {
                window.location.hash = '#/cfg';
            }
        }
    }
}

/**
 * Update Page Title
 */
function updatePageTitle(title) {
    const titleEl = document.querySelector('.page-title');
    if (titleEl) {
        titleEl.textContent = title;
    }
}

function clearPageActions() {
    if (Elements.pageActions) {
        Elements.pageActions.innerHTML = '';
    }
}

function renderAlertGroupsActions() {
    if (!Elements.pageActions) return;
    Elements.pageActions.innerHTML = `
        <button class="btn btn-primary btn-sm" id="create-manual-alert-btn">
            <i data-lucide="plus"></i>
            <span>Create Manual Alert</span>
        </button>
    `;
    if (window.lucide) lucide.createIcons();
    const btn = document.getElementById('create-manual-alert-btn');
    if (btn) {
        btn.addEventListener('click', () => openManualAlertModal());
    }
}


// ========================================
// Event Delegation & Listeners
// ========================================

function bindEvents() {
    // 1. Sidebar Navigation Delegation
    if (Elements.sidebarNav) {
        Elements.sidebarNav.addEventListener('click', (e) => {
            const link = e.target.closest('a.nav-item');
            if (!link) return;

            // Allow default hash navigation to happen (href="#/ops/...")
            // Active class updated in renderSidebar called by hashchange
        });
    }

    // 2. Header Mode Switcher (now in header, not sidebar)
    const headerModeSwitcher = document.getElementById('header-mode-switcher');
    if (headerModeSwitcher) {
        headerModeSwitcher.addEventListener('click', (e) => {
            const modeBtn = e.target.closest('.mode-btn');
            if (modeBtn && !modeBtn.disabled) {
                switchMode(modeBtn.dataset.mode);
            }
        });
    }

    // 2b. Logo Home Link - reset to Ops + All Teams
    const logoHomeLink = document.getElementById('logo-home-link');
    if (logoHomeLink) {
        logoHomeLink.addEventListener('click', () => {
            // Always reset to All Teams when clicking logo
            selectTeam('all');
        });
    }

    // 3. Sidebar Controls Delegation (Team Selector only now)
    if (Elements.sidebarHeaderControls) {
        Elements.sidebarHeaderControls.addEventListener('click', (e) => {
            // Team Selector Trigger
            const trigger = e.target.closest('.dropdown-trigger');
            if (trigger) {
                trigger.parentElement.classList.toggle('active'); // toggle dropdown visibility wrapper
                e.stopPropagation(); // prevent immediate close
                return;
            }

            // Team Item Click
            const teamItem = e.target.closest('.team-dropdown-item');
            if (teamItem) {
                // Close dropdown logic
                const wrapper = document.querySelector('.team-context-wrapper');
                if (wrapper) {
                    wrapper.classList.remove('active');
                }

                const teamId = teamItem.dataset.teamId;
                selectTeam(teamId);
            }
        });
    }

    // Close dropdowns when clicking outside
    document.addEventListener('click', () => {
        // Remove active class from the CONTROLLER (sidebar header controls)
        if (Elements.sidebarHeaderControls) {
            Elements.sidebarHeaderControls.classList.remove('active');
        }
        // Close team selector dropdown
        const wrapper = document.querySelector('.team-context-wrapper');
        if (wrapper) {
            wrapper.classList.remove('active');
        }
    });

    // Manual Toggle implementation for Dropdown (since component structure is sibling)
    // We'll handle this in the click handler above better if we wrap them.
    // Component update: wrap trigger and dropdown? Or simple JS toggle.
    // Let's refine the component Click handler:
    // The component output is siblings. We can toggle class on the dropdown ID directly.

    // Mobile Menu Toggle
    const mobileMenuBtn = document.getElementById('mobile-menu-btn');
    const sidebar = document.getElementById('sidebar');
    const sidebarOverlay = document.getElementById('sidebar-overlay');

    if (mobileMenuBtn && sidebar) {
        mobileMenuBtn.addEventListener('click', () => {
            sidebar.classList.toggle('open');
            if (sidebarOverlay) {
                sidebarOverlay.classList.toggle('visible');
            }
        });
    }

    if (sidebarOverlay) {
        sidebarOverlay.addEventListener('click', () => {
            sidebar.classList.remove('open');
            sidebarOverlay.classList.remove('visible');
        });
    }

    // Close sidebar on nav item click (mobile)
    if (Elements.sidebarNav) {
        Elements.sidebarNav.addEventListener('click', (e) => {
            const link = e.target.closest('a.nav-item');
            if (link && window.innerWidth <= 768) {
                sidebar.classList.remove('open');
                if (sidebarOverlay) {
                    sidebarOverlay.classList.remove('visible');
                }
            }
        });
    }

    // ... (rest of standard events: Refresh, View Toggles, etc.)

    Elements.refreshBtn.addEventListener('click', () => {
        State.refreshCountdown = 10;
        if (State.selectedTeamId && State.selectedTeamId !== 'all') {
            loadTeamAlertGroups(State.selectedTeamId);
        } else {
            loadAlertGroups();
        }
    });

    bindAlertsEvents();

    // View Switcher Events
    if (Elements.viewGridBtn && Elements.viewListBtn) {
        // Restore viewMode from localStorage
        const savedViewMode = localStorage.getItem('tokay.viewMode');
        if (savedViewMode === 'list') {
            State.viewMode = 'list';
            Elements.viewListBtn.classList.add('active');
            Elements.viewGridBtn.classList.remove('active');
            if (Elements.alertGroupsGrid) {
                Elements.alertGroupsGrid.classList.add('view-list');
            }
        }

        Elements.viewGridBtn.addEventListener('click', () => {
            if (State.viewMode === 'grid') return;
            State.viewMode = 'grid';
            Elements.viewGridBtn.classList.add('active');
            Elements.viewListBtn.classList.remove('active');
            if (Elements.alertGroupsGrid) {
                Elements.alertGroupsGrid.classList.remove('view-list');
            }
            localStorage.setItem('tokay.viewMode', 'grid');
        });

        Elements.viewListBtn.addEventListener('click', () => {
            if (State.viewMode === 'list') return;
            State.viewMode = 'list';
            Elements.viewListBtn.classList.add('active');
            Elements.viewGridBtn.classList.remove('active');
            if (Elements.alertGroupsGrid) {
                Elements.alertGroupsGrid.classList.add('view-list');
            }
            localStorage.setItem('tokay.viewMode', 'list');
        });
    }

    // Alert Group Click Delegation
    if (Elements.alertGroupsGrid) {
        Elements.alertGroupsGrid.addEventListener('click', (e) => {
            const card = e.target.closest('.alert-group-card');
            if (card) {
                const id = card.dataset.alertGroupId;
                // Deep link update
                window.location.hash = `#/ops/alert-groups/${id}`;
            }
        });
    }

    // Teams list updated (e.g. after team creation)
    document.addEventListener('tokay:teams-updated', () => {
        renderSidebar();
        renderHeaderControls();
    });

    // Context Change Listener (triggered by selectTeam)
    document.addEventListener('tokay:context-changed', () => {
        console.log('Context changed, reloading view...');
        renderSidebar(); // Update team label in sidebar
        renderHeaderControls(); // Update selector label

        // Reload data based on current view
        handleHashRoute();

        if (State.mode === 'ops') {
            // Explicitly reload alerts if we are in alert view (redundant if handleHashRoute does it, but safer)
            if (ViewManager.getCurrent() === 'alertGroups') {
                if (State.selectedTeamId && State.selectedTeamId !== 'all') {
                    loadTeamAlertGroups(State.selectedTeamId);
                } else {
                    loadAlertGroups();
                }
            }
        }
    });

    // Close Modals

    Elements.modalClose.addEventListener('click', closeModal);
    Elements.modalOverlay.addEventListener('click', (e) => {
        if (e.target === Elements.modalOverlay) closeModal();
    });
}

// ========================================
// Authentication
// ========================================

async function checkAuth() {
    try {
        const user = await API.auth.me();
        console.log('Logged in as:', user.name);
        Permissions.init(user);

        // Populate user menu
        const userName = document.getElementById('dropdown-user-name');
        const userEmail = document.getElementById('dropdown-user-email');
        if (userName) userName.textContent = user.name || 'User';
        if (userEmail) userEmail.textContent = user.email || '';

        return user;
    } catch (error) {
        console.warn('Not authenticated, redirecting...', error);
        window.location.href = '/login.html';
        throw error;
    }
}

/**
 * Setup user menu dropdown and profile events
 */
function bindUserMenuEvents() {
    const userMenu = document.getElementById('user-menu');
    const userAvatarBtn = document.getElementById('user-avatar-btn');
    const openProfileBtn = document.getElementById('open-profile-btn');
    const logoutBtn = document.getElementById('logout-btn');

    // Toggle dropdown on avatar click
    if (userAvatarBtn) {
        userAvatarBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            userMenu.classList.toggle('active');
        });
    }

    // Close dropdown when clicking outside
    document.addEventListener('click', () => {
        if (userMenu) userMenu.classList.remove('active');
    });

    // Open profile modal
    if (openProfileBtn) {
        openProfileBtn.addEventListener('click', (e) => {
            e.preventDefault();
            userMenu.classList.remove('active');
            if (window.ProfileModule) {
                ProfileModule.openModal();
            }
        });
    }

    // Logout
    if (logoutBtn) {
        logoutBtn.addEventListener('click', async (e) => {
            e.preventDefault();
            try {
                await API.auth.logout();
                window.location.href = '/login.html';
            } catch (error) {
                console.error('Logout error:', error);
                window.location.href = '/login.html';
            }
        });
    }

    // Initialize profile module
    if (window.ProfileModule) {
        ProfileModule.init();
    }
}

// ========================================
// Initialization
// ========================================

async function init() {
    console.log('🔥 TokayOps Dashboard initialized (Modes Update)');

    // Init lucide icons early for loading screen
    if (window.lucide) lucide.createIcons();

    try {
        await checkAuth();
    } catch (error) {
        // checkAuth handles redirect, just stop here
        return;
    }

    const authLoading = document.getElementById('auth-loading');
    const mainApp = document.getElementById('main-app');

    // 1. Show App to ensure dimensions/visibility
    if (authLoading) authLoading.style.display = 'none';
    if (mainApp) mainApp.style.display = 'flex';

    // 2. Initialize DOM element references
    initElements();
    initTheme();
    initModalEscapeHandler();

    // Show the running build version in the sidebar footer
    window.renderAppVersion('app-version');

    const themeToggle = document.getElementById('theme-toggle');
    if (themeToggle) {
        themeToggle.addEventListener('click', toggleTheme);
    }

    // 3. Bind Events (Delegation)
    bindEvents();
    bindUsersEvents();
    bindTeamModalClose();
    bindScheduleEvents();
    bindUserMenuEvents();
    bindPoliciesEvents();
    bindIntegrationsEvents();

    // 4. Load Data
    const savedTeamId = localStorage.getItem('tokay.selectedTeamId');
    if (savedTeamId) State.selectedTeamId = savedTeamId;

    await loadTeams();

    // 5. Initial Routing & Rendering
    window.addEventListener('hashchange', handleHashRoute);

    // Explicitly handle initial hash
    handleHashRoute();

    // Fallback: Force render if hash route didn't trigger
    if (!Elements.sidebarNav || Elements.sidebarNav.children.length === 0) {
        console.log('⚠️ Sidebar empty after routing, forcing render...');
        renderSidebar();
        renderHeaderControls();
    }

    // Initial Data Load
    if (State.mode === 'ops') {
        if (State.selectedTeamId && State.selectedTeamId !== 'all') {
            loadTeamAlertGroups(State.selectedTeamId);
        } else {
            loadAlertGroups();
        }
    } else {
        loadAlertGroups(); // Background load or stats
    }

    // Initial Data Load handled by mode check above
    // Removed duplicate loadAlertGroups call
    startAutoRefresh();
}

// Start the application
if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
} else {
    init();
}
