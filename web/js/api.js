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

    // Statuses that are an answer rather than a failure. A 404 from "does this
    // team have a schedule" means "no", and logging it as an error would fill
    // the console with noise on every page that asks - which is how real
    // errors stop being noticed.
    const silent = options.silentStatuses || [];

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

            const body = await response.json().catch(() => ({ error: 'Unknown error' }));

            // The status and the body travel with the error. A message alone
            // is not enough to react to: the schedule API answers 409 in
            // several different senses, each needing a different recovery, and
            // telling them apart by matching prose would break the moment the
            // wording changes. `code` is the server's machine-readable name for
            // what happened; `body` carries the details that go with it, such
            // as the conflicting intervals or the current version.
            const error = new Error(body.error || `HTTP ${response.status}`);
            error.status = response.status;
            error.code = body.code || '';
            error.body = body;
            throw error;
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
        if (!silent.includes(error?.status)) {
            console.error(`API Error [${options.method || 'GET'} ${endpoint}]:`, error);
        }
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
        members: (id, options = {}) => request(
            `/teams/${encodeURIComponent(id)}/members`, options),

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
         * Resolve user IDs to display names.
         *
         * The display read, and the only one that answers for erased users -
         * their row survives so that history naming their ID stays legible,
         * while `get` above is the active read and answers 404 for them.
         * Returns id and name only. IDs it cannot resolve are absent from the
         * answer rather than present as nulls.
         *
         * @param {string[]} ids
         * @returns {Promise<{users: Array<{id, name}>}>}
         */
        resolve: (ids) => {
            return request('/users/resolve', {
                method: 'POST',
                body: JSON.stringify({ user_ids: ids }),
            });
        },

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
    // Schedules API (revision model)
    // ========================================
    //
    // Every call here addresses the revision model. The configuration is saved
    // whole, in one request: the old chain of three (schedule, then L1 groups,
    // then L2 users) could leave a schedule half-written if any link failed,
    // and there was no version to check it against.
    //
    // The reads take an options bag, which `request` flattens into the fetch
    // config - so a caller can pass the `signal` of whatever it is reading on
    // behalf of, and a modal that closes takes its unfinished reads with it.
    // The writes deliberately do not: a change already on its way has already
    // happened somewhere, and aborting it would only lose the answer.
    schedules: {
        /**
         * Read the configuration in force.
         * Answers 404 when the team has no schedule; a deleted schedule answers
         * 200 with `deleted_at` set and the last valid configuration, so the
         * editor can offer to recreate it without a second request.
         * @param {string} teamId
         * @returns {Promise<{schedule_id, version, revision_id, effective_from, deleted_at?, config}>}
         */
        getConfig: (teamId, options = {}) => request(
            `/teams/${encodeURIComponent(teamId)}/schedule/config`,
            { silentStatuses: [404], ...options }),

        /**
         * Save the whole configuration.
         * `expectedVersion` is the version the editor loaded - 0 when there is
         * no schedule yet. A mismatch is refused with 409 rather than
         * overwriting someone else's save.
         * @param {string} teamId
         * @param {Object} config - {timezone, slack_usergroup_id, l1, l2}
         * @param {number} expectedVersion
         * @param {string} [reason] - free text recorded with the revision
         * @returns {Promise<{version, revision_id, noop, created, recreated}>}
         */
        saveConfig: (teamId, config, expectedVersion, reason) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/config`, {
                method: 'PUT',
                body: JSON.stringify({
                    ...config,
                    expected_version: expectedVersion,
                    ...(reason ? { reason } : {}),
                }),
            });
        },

        /**
         * Ask what a save would do, without doing it.
         * The answer is true as of `evaluated_at` and is not a promise about
         * the save: a handoff or an override in between can change it.
         * @param {string} teamId
         * @param {Object} config - the same payload the save would carry
         * @param {Date} [until] - end of the previewed window
         */
        preview: (teamId, config, until, options = {}) => {
            const query = buildQuery({ until: until ? until.toISOString() : null });
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/preview${query}`, {
                method: 'POST',
                body: JSON.stringify(config),
                ...options,
            });
        },

        /**
         * Deactivate the schedule. History is kept.
         * expected_version travels in the query: a DELETE body is not carried
         * reliably by every proxy, and losing it would silently skip the
         * conflict check it exists to perform.
         * @param {string} teamId
         * @param {number} expectedVersion
         */
        deleteSchedule: (teamId, expectedVersion) => {
            const query = buildQuery({ expected_version: expectedVersion });
            return request(`/teams/${encodeURIComponent(teamId)}/schedule${query}`, {
                method: 'DELETE',
            });
        },

        /**
         * Who was on duty across a range. Capped at 90 days by the server.
         * @param {string} teamId
         * @param {Date} from
         * @param {Date} until
         * @returns {Promise<{from, until, history_complete, history_complete_from, deleted_at?, entries, warnings}>}
         */
        render: (teamId, from, until, options = {}) => {
            const query = buildQuery({
                from: from.toISOString(),
                until: until.toISOString(),
            });
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/render${query}`, options);
        },

        /**
         * Who is on duty right now.
         * A team without a schedule and a team whose schedule is deleted both
         * answer 200 with null layers: the question is who is on duty, and
         * "nobody" is an answer. Use getConfig to tell "not configured" from
         * "nobody on duty". A schedule whose data cannot produce an answer
         * answers 500 instead of claiming nobody is on call.
         * @param {string} teamId
         * @returns {Promise<{schedule_id, on_call}>}
         */
        currentOnCall: (teamId, options = {}) => request(
            `/teams/${encodeURIComponent(teamId)}/schedule/on-call`, options),

        /**
         * The audit trail of configuration changes, newest first.
         * @param {string} teamId
         * @param {Object} [opts] - {limit, beforeVersion}
         */
        revisions: (teamId, opts = {}) => {
            const query = buildQuery({
                limit: opts.limit,
                before_version: opts.beforeVersion,
            });
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/revisions${query}`);
        },

        /**
         * Every override that currently exists.
         * This is the only source of `expected_revision` for an edit or a
         * delete - an override read from anywhere else carries no revision to
         * check against.
         * @param {string} teamId
         * @returns {Promise<{overrides: Array}>}
         */
        listOverrides: (teamId, options = {}) => request(
            `/teams/${encodeURIComponent(teamId)}/schedule/overrides`, options),

        /**
         * Record a stand-in.
         * valid_from/valid_to are absolute instants; converting a local time to
         * one is what core/zoned-time.js is for.
         * @param {string} teamId
         * @param {Object} data - {user_id, valid_from, valid_to, reason?}
         */
        createOverride: (teamId, data) => {
            return request(`/teams/${encodeURIComponent(teamId)}/schedule/overrides`, {
                method: 'POST',
                body: JSON.stringify(data),
            });
        },

        /**
         * Append the next revision of an override.
         * @param {string} scheduleId - from getConfig
         * @param {string} overrideId
         * @param {Object} data - {user_id, valid_from, valid_to, reason?}
         * @param {number} expectedRevision - from listOverrides
         */
        updateOverride: (scheduleId, overrideId, data, expectedRevision) => {
            return request(
                `/schedules/${encodeURIComponent(scheduleId)}/overrides/${encodeURIComponent(overrideId)}`, {
                    method: 'PUT',
                    body: JSON.stringify({ ...data, expected_revision: expectedRevision }),
                });
        },

        /**
         * End an override from this moment.
         *
         * One that has not started is removed; one that is in force keeps the
         * hours it has already covered and loses the rest; one that has
         * already ended is refused. History is append-only either way.
         *
         * @param {string} scheduleId
         * @param {string} overrideId
         * @param {number} expectedRevision - from listOverrides
         */
        deleteOverride: (scheduleId, overrideId, expectedRevision) => {
            const query = buildQuery({ expected_revision: expectedRevision });
            return request(
                `/schedules/${encodeURIComponent(scheduleId)}/overrides/${encodeURIComponent(overrideId)}${query}`, {
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
