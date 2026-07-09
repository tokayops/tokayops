/**
 * TokayOps API Service Layer
 * Extensible abstraction for all API endpoints
 * 
 * Phase 2: AlertGroup is the primary entity
 */

const API_BASE = '/api/v1';

/**
 * Get CSRF token from cookie
 * @returns {string|null} The CSRF token or null if not found
 */
function getCSRFToken() {
    const match = document.cookie.match(/(^|;)\s*_csrf=([^;]+)/);
    return match ? match[2] : null;
}

/**
 * Generic fetch wrapper with error handling
 */
async function request(endpoint, options = {}) {
    const baseURL = options.baseURL || API_BASE;
    const url = `${baseURL}${endpoint}`;

    const headers = {
        'Content-Type': 'application/json',
        ...options.headers,
    };

    // Add CSRF token for mutating requests
    const method = (options.method || 'GET').toUpperCase();
    if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(method)) {
        const csrfToken = getCSRFToken();
        if (csrfToken) {
            headers['X-CSRF-Token'] = csrfToken;
        }
    }

    const config = {
        headers,
        ...options,
    };

    try {
        const response = await fetch(url, config);

        if (!response.ok) {
            // Handle 401 Unauthorized globally
            if (response.status === 401) {
                // If we are not on the login page, redirect
                if (!window.location.pathname.includes('login.html')) {
                    window.location.href = '/login.html';
                    throw new Error('Unauthorized'); // Stop processing
                }
            }

            const error = await response.json().catch(() => ({ error: 'Unknown error' }));
            throw new Error(error.error || `HTTP ${response.status}`);
        }

        // Handle 204 No Content responses (e.g., from DELETE operations)
        if (response.status === 204) {
            return null;
        }

        if (options.rawText) {
            return await response.text();
        }

        return await response.json();
    } catch (error) {
        console.error(`API Error [${options.method || 'GET'} ${endpoint}]:`, error);
        throw error;
    }
}

/**
 * Build query string from params object
 */
function buildQuery(params) {
    const filtered = Object.entries(params)
        .filter(([_, v]) => v !== null && v !== undefined && v !== '');

    if (filtered.length === 0) return '';

    return '?' + filtered
        .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`)
        .join('&');
}

/**
 * API endpoints organized by resource
 */
const API = {
    /**
     * Authentication API
     */
    auth: {
        login: (email, password) => request('/login', {
            baseURL: '/api/auth',
            method: 'POST',
            body: JSON.stringify({ email, password }),
        }),
        logout: () => request('/logout', {
            baseURL: '/api/auth',
            method: 'POST',
        }),
        me: () => request('/me', {
            baseURL: '/api/auth',
        }),
        /**
         * Update current user profile (name, slack_user_id)
         * @param {Object} data - Fields to update
         */
        updateMe: (data) => request('/me', {
            baseURL: '/api/auth',
            method: 'PATCH',
            body: JSON.stringify(data),
        }),

        /**
         * Slack OTP Binding
         */
        slack: {
            /**
             * Request OTP code for a Slack user
             * @param {string} slackUserId - Slack User ID
             */
            requestCode: (slackUserId) => request('/me/slack/request-code', {
                baseURL: '/api/auth',
                method: 'POST',
                body: JSON.stringify({ slack_user_id: slackUserId }),
            }),

            /**
             * Confirm OTP code
             * @param {string} code - 6-digit OTP code
             */
            confirmCode: (code) => request('/me/slack/confirm-code', {
                baseURL: '/api/auth',
                method: 'POST',
                body: JSON.stringify({ code }),
            }),

            /**
             * Unbind Slack account
             */
            unbind: () => request('/me/slack', {
                baseURL: '/api/auth',
                method: 'DELETE',
            }),
        },

        telegram: {
            /**
             * Request a Telegram deep link. Returns { link: "https://t.me/<bot>?start=<token>" }.
             * The user opens it and presses Start to link their account (async).
             */
            link: () => request('/me/telegram/link', {
                baseURL: '/api/auth',
                method: 'POST',
            }),

            /**
             * Unbind Telegram account
             */
            unbind: () => request('/me/telegram', {
                baseURL: '/api/auth',
                method: 'DELETE',
            }),
        },
    },

    /**
     * API Tokens
     */
    tokens: {
        /**
         * List current user's tokens
         */
        list: () => request('/tokens'),

        /**
         * Create a new token
         * @param {string} name - Token name
         * @param {number} expiresIn - Expiration in days (optional)
         */
        create: (name, expiresIn = null) => {
            const body = { name };
            if (expiresIn) body.expires_in = expiresIn;
            return request('/tokens', {
                method: 'POST',
                body: JSON.stringify(body),
            });
        },

        /**
         * Delete/revoke a token
         * @param {string} id - Token ID
         */
        delete: (id) => request(`/tokens/${encodeURIComponent(id)}`, {
            method: 'DELETE',
        }),
    },

    /**
     * Alert Groups API (Primary Entity)
     */
    alertGroups: {
        /**
         * List alert groups with optional filters
         * @param {Object} params - Query parameters
         * @param {string} [params.status] - Filter by status
         * @param {number} [params.limit=50] - Items per page
         * @param {number} [params.offset=0] - Pagination offset
         */
        list: (params = {}) => {
            const query = buildQuery({
                statuses: params.statuses,
                severity: params.severity,
                limit: params.limit || 50,
                page: params.page || 1,
                days: params.days,
                view: params.view,
                sort: params.sort,
                sort_dir: params.sort_dir,
            });
            return request(`/alert-groups${query}`);
        },

        /**
         * Get single alert group by ID
         * @param {string} id - Alert Group ID
         */
        get: (id) => {
            return request(`/alert-groups/${encodeURIComponent(id)}`);
        },

        /**
         * Acknowledge an alert group
         * @param {string} id - Alert Group ID
         */
        ack: (id) => {
            return request(`/alert-groups/${encodeURIComponent(id)}/ack`, {
                method: 'PATCH',
            });
        },

        /**
         * Resolve an alert group
         * @param {string} id - Alert Group ID
         */
        resolve: (id) => {
            return request(`/alert-groups/${encodeURIComponent(id)}/resolve`, {
                method: 'PATCH',
            });
        },

        /**
         * Get timeline events for an alert group
         * @param {string} id - Alert Group ID
         */
        timeline: (id) => {
            return request(`/alert-groups/${encodeURIComponent(id)}/timeline`);
        },

        /**
         * Add a note to an alert group
         * @param {string} id - Alert Group ID
         * @param {string} message - Note message
         * @param {string} actor - Optional actor name
         */
        addNote: (id, message, actor = 'user') => {
            return request(`/alert-groups/${encodeURIComponent(id)}/notes`, {
                method: 'POST',
                body: JSON.stringify({ message, actor }),
            });
        },

        /**
         * Create a manual alert group
         * @param {Object} data - Manual alert group data
         * @param {string} data.team_id - Team ID
         * @param {string} data.severity - Severity (critical, warning, info)
         * @param {string} [data.title] - Optional title
         */
        create: (data) => {
            return request('/alert-groups', {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },
    },

    // ========================================
    // Teams API
    // ========================================
    teams: {
        /**
         * List all teams
         */
        list: () => request('/teams'),

        /**
         * Create a new team
         * @param {Object} data - Team data
         * @param {string} data.id - Team ID (required)
         * @param {string} data.name - Team name (required)
         * @param {string} [data.slack_channel] - Slack channel ID
         */
        create: (data) => {
            return request('/teams', {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Get single team by ID
         * @param {string} id - Team ID
         */
        get: (id) => request(`/teams/${encodeURIComponent(id)}`),

        /**
         * Get alert groups for a team
         * @param {string} id - Team ID
         * @param {Object} params - Query parameters
         */
        alertGroups: (id, params = {}) => {
            const query = buildQuery({
                statuses: params.statuses,
                severity: params.severity,
                limit: params.limit || 50,
                page: params.page || 1,
                days: params.days,
                view: params.view,
                sort: params.sort,
                sort_dir: params.sort_dir,
            });
            return request(`/teams/${encodeURIComponent(id)}/alert-groups${query}`);
        },

        /**
         * Get members of a team
         * @param {string} id - Team ID
         */
        members: (id) => request(`/teams/${encodeURIComponent(id)}/members`),

        /**
         * Add a member to a team
         * @param {string} teamId - Team ID
         * @param {string} userId - User ID
         * @param {string} role - Role (team_admin or team_member)
         */
        addMember: (teamId, userId, role = 'team_member') => {
            return request(`/teams/${encodeURIComponent(teamId)}/members`, {
                method: 'POST',
                body: JSON.stringify({ user_id: userId, role }),
            });
        },

        /**
         * Remove a member from a team
         * @param {string} teamId - Team ID
         * @param {string} userId - User ID
         */
        removeMember: (teamId, userId) => {
            return request(`/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}`, {
                method: 'DELETE',
            });
        },

        /**
         * Update a team
         * @param {string} id - Team ID
         * @param {Object} data - Team data to update
         */
        update: (id, data) => {
            return request(`/teams/${encodeURIComponent(id)}`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        /**
         * Delete a team
         * @param {string} id - Team ID
         */
        delete: (id) => {
            return request(`/teams/${encodeURIComponent(id)}`, {
                method: 'DELETE',
            });
        },
    },

    // ========================================
    // Users API
    // ========================================
    users: {
        /**
         * List all users
         */
        list: () => request('/users'),

        /**
         * Get single user by ID
         * @param {string} id - User ID
         */
        get: (id) => request(`/users/${encodeURIComponent(id)}`),

        /**
         * Create a new user
         * @param {Object} data - User data
         */
        create: (data) => {
            return request('/users', {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Update a user
         * @param {string} id - User ID
         * @param {Object} data - Fields to update
         */
        update: (id, data) => {
            return request(`/users/${encodeURIComponent(id)}`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        /**
         * Delete a user
         * @param {string} id - User ID
         */
        delete: (id) => {
            return request(`/users/${encodeURIComponent(id)}`, {
                method: 'DELETE',
            });
        },

        /**
         * Update a user's password
         * @param {string} id - User ID
         * @param {string} password - New password
         */
        updatePassword: (id, password) => {
            return request(`/users/${encodeURIComponent(id)}/password`, {
                method: 'PUT',
                body: JSON.stringify({ password }),
            });
        },
    },

    // ========================================
    // Schedules API (Phase 3)
    // ========================================
    schedules: {
        /**
         * Get schedule for a team
         * @param {string} teamId - Team ID
         */
        get: (teamId) => request(`/teams/${encodeURIComponent(teamId)}/schedule`),

        /**
         * Create or update schedule for a team
         * @param {string} teamId - Team ID
         * @param {Object} data - Schedule configuration
         */
        upsert: (teamId, data) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        /**
         * Set L1 rotation groups
         * @param {string} teamId - Team ID
         * @param {string[][]} groups - Ordered groups of user IDs, e.g. [["a","b"],["c"]]
         */
        setL1Groups: (teamId, groups) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/l1-groups`, {
                method: 'PUT',
                body: JSON.stringify({ groups }),
            });
        },

        /**
         * Set L2 rotation users
         * @param {string} teamId - Team ID
         * @param {string[]} userIds - Ordered user IDs
         */
        setL2Users: (teamId, userIds) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/l2-users`, {
                method: 'PUT',
                body: JSON.stringify({ user_ids: userIds }),
            });
        },

        /**
         * Get current on-call for a team
         * @param {string} teamId - Team ID
         */
        getOnCall: (teamId) => request(`/teams/${encodeURIComponent(teamId)}/oncall`),

        /**
         * Render schedule entries for a time range
         * @param {string} teamId - Team ID
         * @param {Date} from - Start time
         * @param {Date} until - End time
         */
        render: (teamId, from, until, timezone) => {
            const query = buildQuery({
                from: from.toISOString(),
                until: until.toISOString(),
                timezone: timezone,
            });
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/render${query}`);
        },

        /**
         * Create an override
         * @param {string} teamId - Team ID
         * @param {Object} data - Override data
         */
        createOverride: (teamId, data) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/overrides`, {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Delete an override
         * @param {string} scheduleId - Schedule ID
         * @param {string} overrideId - Override ID
         */
        /**
         * Delete schedule for a team
         * @param {string} teamId - Team ID
         */
        delete: (teamId) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule`, {
                method: 'DELETE',
            });
        },

        updateOverride: (scheduleId, overrideId, data) => {
            return request(`/schedules/${encodeURIComponent(scheduleId)}/overrides/${encodeURIComponent(overrideId)}`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        deleteOverride: (scheduleId, overrideId) => {
            return request(`/schedules/${encodeURIComponent(scheduleId)}/overrides/${encodeURIComponent(overrideId)}`, {
                method: 'DELETE',
            });
        },
    },

    // ========================================
    // Policies API (Phase 4)
    // ========================================
    // ========================================
    // Providers API (Sprint 4 / Epic 7 L6)
    // ========================================
    providers: {
        /**
         * List notification providers and their capabilities.
         * Returns { providers: [{name, integration_type, supported_target_kinds}] }.
         * The policy editor uses this to build the (provider, target_kind) dropdown.
         */
        list: () => request('/providers'),
    },

    policies: {
        /**
         * List all escalation policies
         */
        list: () => request('/policies'),

        /**
         * Get a specific policy by ID
         * @param {string} id - Policy ID
         */
        get: (id) => request(`/policies/${encodeURIComponent(id)}`),

        /**
         * Create a new escalation policy
         * @param {Object} data - Policy data
         * @param {string} data.name - Policy name
         * @param {string} [data.description] - Description
         * @param {string} data.team_id - Team ID
         * @param {Array} data.steps - Array of step objects
         */
        create: (data) => {
            return request('/policies', {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Update an escalation policy
         * @param {string} id - Policy ID
         * @param {Object} data - Policy data
         */
        update: (id, data) => {
            return request(`/policies/${encodeURIComponent(id)}`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        /**
         * Delete an escalation policy
         * @param {string} id - Policy ID
         */
        delete: (id) => {
            return request(`/policies/${encodeURIComponent(id)}`, {
                method: 'DELETE',
            });
        },
    },

    // ========================================
    // Integrations API
    // ========================================
    integrations: {
        /**
         * List all integrations
         */
        list: () => request('/integrations'),

        /**
         * Get a specific integration by ID
         * @param {string} id - Integration ID
         */
        get: (id) => request(`/integrations/${encodeURIComponent(id)}`),

        /**
         * Create a new integration
         * @param {Object} data - Integration data
         * @param {string} data.type - Integration type (slack, alertmanager_webhook)
         * @param {string} data.name - Integration name
         * @param {boolean} [data.enabled] - Whether integration is enabled
         * @param {Object} data.config - Type-specific configuration
         */
        create: (data) => {
            return request('/integrations', {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Update an integration
         * @param {string} id - Integration ID
         * @param {Object} data - Integration data
         */
        update: (id, data) => {
            return request(`/integrations/${encodeURIComponent(id)}`, {
                method: 'PUT',
                body: JSON.stringify(data),
            });
        },

        /**
         * Delete an integration
         * @param {string} id - Integration ID
         */
        delete: (id) => {
            return request(`/integrations/${encodeURIComponent(id)}`, {
                method: 'DELETE',
            });
        },

        /**
         * Test an integration (send test DM)
         * @param {string} id - Integration ID
         * @param {Object} data - Test options (e.g., { mode: 'dm' })
         */
        test: (id, data) => {
            return request(`/integrations/${encodeURIComponent(id)}/test`, {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Get Slack App Manifest YAML with pre-filled callback URLs.
         * Returns raw text (not JSON).
         */
        slackManifest: () => request('/integrations/slack/manifest', { rawText: true }),

        /**
         * List deliveries for an integration
         * @param {string} id - Integration ID
         * @param {Object} params - Query parameters (page, limit)
         */
        deliveries: (id, params = {}) => request(`/integrations/${encodeURIComponent(id)}/deliveries${buildQuery(params)}`),

        /**
         * Get a specific delivery detail
         * @param {string} id - Integration ID
         * @param {string} deliveryId - Delivery ID
         */
        deliveryDetail: (id, deliveryId) => request(`/integrations/${encodeURIComponent(id)}/deliveries/${encodeURIComponent(deliveryId)}`),

        /**
         * Replay a delivery
         * @param {string} id - Integration ID
         * @param {string} deliveryId - Delivery ID
         */
        replayDelivery: (id, deliveryId) => request(`/integrations/${encodeURIComponent(id)}/deliveries/${encodeURIComponent(deliveryId)}/replay`, { method: 'POST' }),
    },
};

// Export for use in other modules
window.API = API;

/**
 * Fetch build metadata from the public /api/version endpoint and render it into
 * the element with the given id. Compact form "branch@commit · date" on screen,
 * full detail in the title (hover tooltip). No-ops quietly if the element is
 * missing or the request fails — version display is non-critical.
 * @param {string} elId - id of the target element
 */
async function renderAppVersion(elId) {
    const el = document.getElementById(elId);
    if (!el) return;
    try {
        const res = await fetch('/api/version');
        if (!res.ok) return;
        const v = await res.json();
        const branch = v.branch || 'unknown';
        const commit = v.commit || 'unknown';
        const shortCommit = commit.slice(0, 7);
        const date = (v.date || '').slice(0, 10);
        el.textContent = date ? `${branch}@${shortCommit} · ${date}` : `${branch}@${shortCommit}`;
        el.title = `branch: ${branch}\ncommit: ${commit}\nbuilt: ${v.date || 'unknown'}`;
    } catch (e) {
        // Version display is non-critical; leave the element empty on failure.
    }
}

window.renderAppVersion = renderAppVersion;
