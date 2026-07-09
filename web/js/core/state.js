/**
 * TokayOps Application State
 * Centralized state management
 */

export const State = {
    // Mode State
    mode: 'ops',
    modeSource: 'user',
    lastRoutes: { ops: '#/ops/alert-groups', cfg: '#/cfg/teams' },

    // Data
    alertGroups: [],
    teams: [],
    users: [],
    teamMembers: {}, // Cache: teamId -> members[]

    // UI/View State
    currentState: 'active',
    periodDays: 7,
    severityFilter: {
        critical: true,
        warning: true,
        info: false,
    },
    selectedTeamId: 'all',
    currentView: 'alertGroups',
    currentPage: 1,
    pageSize: 20,
    selectedAlertGroup: null,
    editingUser: null,
    managingTeam: null,
    refreshInterval: null,
    refreshCountdown: 10,
    isLoading: false,
    viewMode: 'grid',
    sortField: 'default',
    sortDirection: 'desc',
    sortPauseUntil: 0,
    sortSnapshot: [],
    lastSortedOrder: [],
    highlightedAlertGroupId: null,
    highlightUntil: 0,
    highlightScrollPending: false,
    lingerAlertGroup: null,
    lingerUntil: 0,
    policies: [],
    editingPolicy: null,
    alertGroupsRaw: [],
    paginationMeta: null,
    onCallByTeam: {},
    integrations: [],
    editingIntegration: null,
};

window.State = State;

// State mapping: user-facing -> internal statuses to query
export const STATE_STATUS_MAP = {
    'active': ['new', 'processing', 'triggered', 'acknowledged'],
    'triggered': ['new', 'processing', 'triggered'],
    'acknowledged': ['acknowledged'],
    'resolved': ['resolved', 'closed'],
    'all': ['new', 'processing', 'triggered', 'acknowledged', 'resolved', 'closed'],
};
