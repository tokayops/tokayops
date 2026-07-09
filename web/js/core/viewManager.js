import { Elements } from '/js/core/utils.js';
import { State } from '/js/core/state.js';

export const ViewManager = {
    views: {
        // Ops Views
        alertGroups: () => document.querySelector('.alert-groups-container'),
        oncall: () => Elements.opsOnCallView, // Ops on-call view
        activity: () => Elements.opsActivityView, // Ops activity view

        // Config Views
        configureDashboard: () => document.getElementById('configure-dashboard'),
        teams: () => Elements.teamsSection, // Can be reused or specific config view
        allTeams: () => document.getElementById('all-teams-section'),
        users: () => Elements.usersSection,
        policies: () => Elements.policiesSection,
        integrations: () => Elements.integrationsSection,
    },

    globalElements: {
        filters: () => Elements.alertGroupsFilters,
        viewToggle: () => Elements.viewToggle,
        configureDashboard: () => document.getElementById('configure-dashboard'), // Tile grid
        teamsNav: () => document.getElementById('teams-nav'), // Maybe hide in Cfg? depends on design
    },

    currentView: null,

    /**
     * Show a specific view, hiding all others
     * @param {string} viewName - Key from this.views
     * @param {object} options - { showStats: bool, showViewToggle: bool }
     */
    show(viewName, options = {}) {
        const isOps = State.mode === 'ops';

        // Defaults based on Mode and View
        const defaults = {
            showStats: isOps && viewName === 'alertGroups',
            showViewToggle: isOps && viewName === 'alertGroups',
        };
        const opts = { ...defaults, ...options };

        // 1. Hide ALL views
        Object.values(this.views).forEach(getter => {
            const el = getter();
            if (el) el.style.display = 'none';
        });

        // 2. Show TARGET view
        const targetGetter = this.views[viewName];
        if (targetGetter) {
            const target = targetGetter();
            if (target) {
                target.style.display = 'block';
                // Special handling for views that need flex/grid
                if (viewName === 'configureDashboard') target.style.display = 'grid';
            }
        }

        // 3. Manage global elements
        const filtersEl = this.globalElements.filters();
        const viewToggleEl = this.globalElements.viewToggle();

        if (filtersEl) filtersEl.style.display = opts.showStats ? 'block' : 'none';
        if (viewToggleEl) viewToggleEl.style.display = opts.showViewToggle ? 'flex' : 'none';

        this.currentView = viewName;
        console.log(`📺 ViewManager: switched to '${viewName}' (Mode: ${State.mode})`);
    },

    /**
     * Get current view name
     */
    getCurrent() {
        return this.currentView;
    }
};
