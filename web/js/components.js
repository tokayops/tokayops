/**
 * TokayOps UI Components
 * Reusable component functions for rendering UI elements
 * 
 * Phase 2: AlertGroup is the primary entity
 */

/**
 * Status Mapping: Internal → User-facing
 * Backend uses: new, processing, triggered, acknowledged, resolved, closed
 * UI shows: Triggered, Acknowledged, Resolved
 */
const STATUS_MAP = {
    'new': 'triggered',
    'processing': 'triggered',
    'triggered': 'triggered',
    'acknowledged': 'acknowledged',
    'resolved': 'resolved',
    'closed': 'resolved',
};

const STATUS_LABELS = {
    'triggered': 'Triggered',
    'acknowledged': 'Acknowledged',
    'resolved': 'Resolved',
};

function getDisplayStatus(internalStatus) {
    return STATUS_MAP[internalStatus] || internalStatus;
}

function truncateText(value, maxLength = 24) {
    if (!value) return '—';
    const text = String(value);
    if (text.length <= maxLength) return text;
    return `${text.slice(0, Math.max(0, maxLength - 3))}...`;
}

const Components = {
    /**
     * Render Configure Dashboard (Tile Grid)
     * Shows all tiles, disables based on role with tooltips
     */
    configureDashboard: (stats = {}) => {
        const canConfig = Permissions.canAccessConfigure();
        const isAdmin = Permissions.isAdmin();

        // Determine disabled states (same as sidebar)
        const teamsDisabled = !canConfig;
        const policiesDisabled = !canConfig;
        const integrationsDisabled = !canConfig;
        const usersDisabled = !isAdmin;

        const tileClass = (disabled) => disabled ? 'config-tile disabled' : 'config-tile';
        const tileClick = (disabled, route) => disabled ? '' : `onclick="window.location.hash='${route}'"`;
        const tileTooltip = (disabled, isAdminOnly) => {
            if (!disabled) return '';
            return isAdminOnly ? 'title="Admin only"' : 'title="Insufficient permissions"';
        };

        return `
            <div class="configure-grid">
                <div class="${tileClass(teamsDisabled)}" ${tileClick(teamsDisabled, '#/cfg/teams')} 
                     ${tileTooltip(teamsDisabled, false)}>
                    <div class="tile-icon"><i data-lucide="users"></i></div>
                    <h3>Teams</h3>
                    <p>${stats.teamsCount || 0} teams configured</p>
                </div>
                <div class="${tileClass(policiesDisabled)}" ${tileClick(policiesDisabled, '#/cfg/policies')}
                     ${tileTooltip(policiesDisabled, false)}>
                    <div class="tile-icon"><i data-lucide="shield"></i></div>
                    <h3>Escalation Policies</h3>
                    <p>${stats.policiesCount || 0} policies active</p>
                </div>
                <div class="${tileClass(integrationsDisabled)}" ${tileClick(integrationsDisabled, '#/cfg/integrations')}
                     ${tileTooltip(integrationsDisabled, false)}>
                    <div class="tile-icon"><i data-lucide="webhook"></i></div>
                    <h3>Integrations</h3>
                    <p>Manage integrations</p>
                </div>
                <div class="${tileClass(usersDisabled)}" ${tileClick(usersDisabled, '#/cfg/users')}
                     ${tileTooltip(usersDisabled, true)}>
                    <div class="tile-icon"><i data-lucide="user-cog"></i></div>
                    <h3>Users</h3>
                    <p>Manage system access</p>
                </div>
            </div>
        `;
    },

    /**
     * Render Mode Switcher (for Header placement)
     * Shows both modes, disables Configure if user doesn't have access
     * Compact horizontal layout for header bar
     */
    modeSwitcher: (currentMode) => {
        const canConfig = Permissions.canAccessConfigure();
        const disabledClass = canConfig ? '' : 'disabled';
        const disabledAttr = canConfig ? '' : 'disabled';
        const tooltip = canConfig ? '' : 'title="Insufficient permissions"';

        return `
            <div class="mode-switcher">
                <button class="mode-btn ${currentMode === 'ops' ? 'active' : ''}" data-mode="ops">
                    <i data-lucide="zap" class="mode-icon"></i>
                    <span>Operations</span>
                </button>
                <button class="mode-btn ${currentMode === 'cfg' ? 'active' : ''} ${disabledClass}" 
                        data-mode="cfg" ${disabledAttr} ${tooltip}>
                    <i data-lucide="settings" class="mode-icon"></i>
                    <span>Configure</span>
                </button>
            </div>
        `;
    },

    /**
     * Render Team Context Selector (for Sidebar)
     * Clean team selector without mode-related labels
     */
    teamContextSelector: (teams, selectedTeamId) => {
        const selectedTeam = teams.find(t => t.id === selectedTeamId);
        const label = selectedTeam ? selectedTeam.name : 'All Teams';

        return `
            <div class="team-context-wrapper">
                <div class="team-selector dropdown-trigger" id="team-context-trigger">
                    <div class="team-selector-label">
                        <span class="team-label-prefix">Team:</span>
                        <span class="team-name">${escapeHtml(label)}</span>
                        <i data-lucide="chevron-down" style="width:14px;height:14px;opacity:0.6;"></i>
                    </div>
                </div>
                <div class="team-dropdown" id="team-context-dropdown">
                    <div class="team-dropdown-item ${selectedTeamId === 'all' ? 'active' : ''}" data-team-id="all">
                        <span class="team-icon-small"><i data-lucide="layout-grid"></i></span>
                        All Teams
                    </div>
                    <div class="dropdown-divider"></div>
                    ${teams.map(t => `
                        <div class="team-dropdown-item ${selectedTeamId === t.id ? 'active' : ''}" data-team-id="${t.id}">
                            <span class="team-icon-small"><i data-lucide="users"></i></span>
                            ${escapeHtml(t.name)}
                        </div>
                    `).join('')}
                </div>
            </div>
        `;
    },

    /**
     * Render Sidebar Navigation based on Mode
     */
    sidebarNav: (mode, selectedTeamId) => {
        // Operations Mode
        if (mode === 'ops') {
            return `
                <a href="#/ops/alert-groups" class="nav-item ${selectedTeamId ? '' : 'active'}" data-route="alert-groups">
                    <i data-lucide="bell" class="nav-icon"></i>
                    <span class="nav-text">Alert Groups</span>
                </a>
                <a href="#/ops/oncall" class="nav-item" data-route="oncall">
                    <i data-lucide="calendar" class="nav-icon"></i>
                    <span class="nav-text">On-Call</span>
                </a>
                <a href="#/ops/activity" class="nav-item disabled" title="Coming soon">
                    <i data-lucide="activity" class="nav-icon"></i>
                    <span class="nav-text">Activity</span>
                </a>
            `;
        }

        // Configure Mode - show all items, disable based on role
        if (mode === 'cfg') {
            const canConfig = Permissions.canAccessConfigure();
            const isAdmin = Permissions.isAdmin();

            // Determine disabled states
            const teamsDisabled = !canConfig;
            const policiesDisabled = !canConfig;
            const integrationsDisabled = !canConfig;
            const usersDisabled = !isAdmin;

            return `
                <div class="nav-section-header">
                    <span class="nav-section-title">General</span>
                </div>
                <a href="${teamsDisabled ? 'javascript:void(0)' : '#/cfg/teams'}" 
                   class="nav-item ${teamsDisabled ? 'disabled' : ''}" 
                   ${teamsDisabled ? 'title="Insufficient permissions"' : ''} data-route="teams">
                    <i data-lucide="users" class="nav-icon"></i>
                    <span class="nav-text">Teams</span>
                </a>
                <a href="${policiesDisabled ? 'javascript:void(0)' : '#/cfg/policies'}" 
                   class="nav-item ${policiesDisabled ? 'disabled' : ''}"
                   ${policiesDisabled ? 'title="Insufficient permissions"' : ''} data-route="policies">
                    <i data-lucide="shield" class="nav-icon"></i>
                    <span class="nav-text">Escalation Policies</span>
                </a>
                <a href="${integrationsDisabled ? 'javascript:void(0)' : '#/cfg/integrations'}" 
                   class="nav-item ${integrationsDisabled ? 'disabled' : ''}"
                   ${integrationsDisabled ? 'title="Insufficient permissions"' : ''} data-route="integrations">
                    <i data-lucide="webhook" class="nav-icon"></i>
                    <span class="nav-text">Integrations</span>
                </a>
                <div class="nav-divider"></div>
                <div class="nav-section-header">
                    <span class="nav-section-title">Admin</span>
                </div>
                <a href="${usersDisabled ? 'javascript:void(0)' : '#/cfg/users'}" 
                   class="nav-item ${usersDisabled ? 'disabled' : ''}"
                   ${usersDisabled ? 'title="Admin only"' : ''} data-route="users">
                    <i data-lucide="user-cog" class="nav-icon"></i>
                    <span class="nav-text">Users</span>
                </a>
            `;
        }
    },
    /**
     * Render status badge (maps internal status to user-facing)
     * @param {string} status - Internal status
     */
    statusBadge: (status) => {
        const displayStatus = getDisplayStatus(status);
        const label = STATUS_LABELS[displayStatus] || displayStatus;
        return `<span class="badge badge-status status-${displayStatus}">${label}</span>`;
    },

    /**
     * Render alert status badge (for alert-level statuses: firing/resolved)
     * @param {string} status - Alert status (firing or resolved)
     */
    alertStatusBadge: (status) => {
        const normalized = status?.toLowerCase() || 'firing';
        const label = normalized === 'resolved' ? 'Resolved' : 'Firing';
        const cssClass = normalized === 'resolved' ? 'resolved' : 'firing';
        return `<span class="alert-status-tag status-${cssClass}">${label}</span>`;
    },

    /**
     * Render severity badge
     * @param {string} severity - Severity level
     */
    severityBadge: (severity) => {
        const normalized = severity?.toLowerCase() || 'info';
        const severityClass = ['critical', 'warning', 'info'].includes(normalized)
            ? normalized
            : 'info';
        return `<span class="badge badge-severity severity-${severityClass}">${severity || 'N/A'}</span>`;
    },

    /**
     * Format timestamp to relative time
     * @param {string} timestamp - ISO timestamp
     */
    timeAgo: (timestamp) => {
        if (!timestamp) return 'N/A';

        const date = new Date(timestamp);
        const now = new Date();
        const seconds = Math.floor((now - date) / 1000);

        if (seconds < 60) return 'just now';
        if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
        if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
        if (seconds < 604800) return `${Math.floor(seconds / 86400)}d ago`;

        return date.toLocaleDateString();
    },

    /**
     * Format elapsed time since timestamp as a compact duration
     * @param {string} timestamp - ISO timestamp
     * @param {Object} options
     * @param {boolean} [options.withAgo=false] - Append "ago"
     */
    timeSince: (timestamp, options = {}) => {
        if (!timestamp) return 'N/A';

        const date = new Date(timestamp);
        const now = new Date();
        const seconds = Math.max(0, Math.floor((now - date) / 1000));

        let value = '1m';
        if (seconds < 60) {
            value = '1m';
        } else {
            const minutes = Math.floor(seconds / 60);
            if (minutes < 60) {
                value = `${minutes}m`;
            } else {
                const hours = Math.floor(minutes / 60);
                const remMinutes = minutes % 60;
                if (hours < 24) {
                    value = remMinutes > 0 ? `${hours}h ${remMinutes}m` : `${hours}h`;
                } else {
                    const days = Math.floor(hours / 24);
                    const remHours = hours % 24;
                    value = remHours > 0 ? `${days}d ${remHours}h` : `${days}d`;
                }
            }
        }

        return options.withAgo ? `${value} ago` : value;
    },

    /**
     * Format a timestamp as a human-readable date/time
     * @param {string} timestamp - ISO timestamp
     * @returns {string} Formatted date string (e.g., "Today, 15:30" or "Jan 22, 15:30")
     */
    formatDateTime: (timestamp) => {
        if (!timestamp) return 'N/A';
        const date = new Date(timestamp);
        const now = new Date();
        const isToday = date.toDateString() === now.toDateString();
        const timeStr = date.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false });
        if (isToday) return `Today, ${timeStr}`;
        const dateStr = date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
        return `${dateStr}, ${timeStr}`;
    },

    /**
     * Format a duration in milliseconds as a compact string
     * @param {number} ms
     */
    formatDuration: (ms) => {
        if (!ms || ms < 0) return '0m';
        const totalSeconds = Math.floor(ms / 1000);
        if (totalSeconds < 60) return '1m';
        const totalMinutes = Math.floor(totalSeconds / 60);
        if (totalMinutes < 60) return `${totalMinutes}m`;
        const hours = Math.floor(totalMinutes / 60);
        const remMinutes = totalMinutes % 60;
        if (hours < 24) {
            return remMinutes > 0 ? `${hours}h ${remMinutes}m` : `${hours}h`;
        }
        const days = Math.floor(hours / 24);
        const remHours = hours % 24;
        return remHours > 0 ? `${days}d ${remHours}h` : `${days}d`;
    },

    /**
     * Format timestamp to readable date/time with timezone
     * @param {string} timestamp - ISO timestamp
     */
    formatTime: (timestamp) => {
        if (!timestamp) return 'N/A';
        return new Date(timestamp).toLocaleString(undefined, {
            year: 'numeric',
            month: '2-digit',
            day: '2-digit',
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
            timeZoneName: 'short'
        });
    },

    /**
     * Render alert group card
     * @param {Object} alertGroup - AlertGroup data
     */
    alertGroupCard: (alertGroup, options = {}) => {
        const alertCount = alertGroup.alerts_count ?? alertGroup.alerts?.length ?? 0;
        const firingCount = alertGroup.firing_count ?? alertGroup.alerts?.filter(a => a.status === 'firing').length ?? 0;
        const resolvedCount = alertCount - firingCount;

        const displayStatus = getDisplayStatus(alertGroup.status);
        const ackName = displayStatus === 'acknowledged' ? (alertGroup.acknowledged_by || '-') : '-';
        const ackDisplay = truncateText(ackName, 24);
        const ackTitle = alertGroup.acknowledged_by ? ` title="${escapeHtml(alertGroup.acknowledged_by)}"` : '';
        const normalizedSeverity = alertGroup.severity?.toLowerCase() || 'info';
        const severityClass = ['critical', 'warning', 'info'].includes(normalizedSeverity)
            ? normalizedSeverity
            : 'info';
        const highlightClass = options.highlight ? ' is-highlighted' : '';
        const onCallData = options.onCall;
        const onCallName = (onCallData?.l1_users || []).map(u => u.name).join(', ');
        const onCallDisplay = onCallName
            ? truncateText(onCallName, 24)
            : (onCallData ? 'Not configured' : '—');
        const onCallTitle = onCallName ? ` title="${escapeHtml(onCallName)}"` : '';

        const showAlertsCount = alertCount > 0 || options.forceAlertsCount;
        const startTime = alertGroup.created_at ? new Date(alertGroup.created_at).getTime() : 0;
        const endTime = alertGroup.resolved_at ? new Date(alertGroup.resolved_at).getTime() : Date.now();
        const duration = Components.formatDuration(endTime - startTime);
        const alertsParts = [];
        if (firingCount > 0) alertsParts.push(`${firingCount} firing`);
        if (resolvedCount > 0) alertsParts.push(`${resolvedCount} resolved`);
        const alertsSummary = alertsParts.length > 0
            ? `<span class="alerts-count-main">${alertsParts.join(' · ')}</span><span class="alerts-duration">for ${duration}</span>`
            : '0';

        return `
            <div class="alert-group-card status-${displayStatus} severity-${severityClass}${highlightClass}" data-alert-group-id="${alertGroup.id}">
                <div class="alert-group-card-header">
                    <h3 class="alert-group-title">${escapeHtml(alertGroup.title || 'Untitled Alert')}</h3>
                </div>
                <div class="alert-group-badges">
                    <div class="alert-group-severity-badge">
                        ${Components.severityBadge(alertGroup.severity)}
                    </div>
                    <div class="alert-group-status-badge">
                        ${Components.statusBadge(alertGroup.status)}
                    </div>
                </div>
                <div class="alert-group-meta">
                    <div class="alert-group-meta-item">
                        <span class="alert-group-meta-label">Team:</span>
                        <span class="alert-group-meta-value">${escapeHtml(alertGroup.team_id || 'N/A')}</span>
                    </div>
                    <div class="alert-group-meta-item">
                        <span class="alert-group-meta-label">On-call:</span>
                        <span class="alert-group-meta-value meta-truncate"${onCallTitle}>${escapeHtml(onCallDisplay)}</span>
                    </div>
                    <div class="alert-group-meta-item">
                        <span class="alert-group-meta-label">Ack by:</span>
                        <span class="alert-group-meta-value meta-truncate"${ackTitle}>${escapeHtml(ackDisplay)}</span>
                    </div>
                    <div class="alert-group-meta-item">
                        <span class="alert-group-meta-label">Fired:</span>
                        <span class="alert-group-meta-value">${Components.formatDateTime(alertGroup.created_at)}</span>
                    </div>
                    <div class="alert-group-meta-item">
                        <span class="alert-group-meta-label">Resolved:</span>
                        <span class="alert-group-meta-value">${alertGroup.resolved_at ? Components.formatDateTime(alertGroup.resolved_at) : '-'}</span>
                    </div>
                </div>
                ${showAlertsCount ? `
                    <div class="alert-group-alerts-count">
                        ${alertsSummary}
                    </div>
                ` : ''}
            </div>
        `;
    },

    /**
     * Render alert group detail modal content
     * @param {Object} alertGroup - AlertGroup data
     */
    alertGroupDetail: (alertGroup) => {
        const displayStatus = getDisplayStatus(alertGroup.status);
        const normalizedSeverity = alertGroup.severity?.toLowerCase() || 'info';
        const severityClass = ['critical', 'warning', 'info'].includes(normalizedSeverity)
            ? normalizedSeverity
            : 'info';
        const severityLabel = (alertGroup.severity || 'info').toUpperCase();
        const stateLabel = STATUS_LABELS[displayStatus] || displayStatus;
        const startedAt = alertGroup.created_at ? new Date(alertGroup.created_at).getTime() : 0;
        const endedAt = alertGroup.resolved_at ? new Date(alertGroup.resolved_at).getTime() : Date.now();
        const activeDuration = Components.formatDuration(Math.max(0, endedAt - startedAt));
        const statusTime = activeDuration;
        const updatedRelative = Components.timeSince(alertGroup.updated_at, { withAgo: true });
        const ackName = alertGroup.acknowledged_by || '';
        const ackDisplay = ackName ? truncateText(ackName, 28) : '';
        const ackTitle = ackName ? ` title="${escapeHtml(ackName)}"` : '';
        const onCallName = (alertGroup.onCall?.l1_users || []).map(u => u.name).join(', ');
        const onCallDisplay = onCallName ? truncateText(onCallName, 28) : (alertGroup.onCall ? 'Not configured' : '—');
        const onCallTitle = onCallName ? ` title="${escapeHtml(onCallName)}"` : '';
        const firingCount = alertGroup.firing_count ?? alertGroup.alerts?.filter(a => a.status === 'firing').length ?? 0;
        const totalCount = alertGroup.alerts_count ?? alertGroup.alerts?.length ?? 0;
        const resolvedCount = totalCount - firingCount;
        const alertsSummary = `Alerts: ${firingCount} firing${resolvedCount > 0 ? ` · ${resolvedCount} resolved` : ''}`;
        const teamLabel = alertGroup.team_id ? `Team ${alertGroup.team_id}` : 'Team N/A';

        return `
            <div class="detail-hero">
                <div class="detail-status-line">
                    <span class="status-severity severity-${severityClass}">[${severityLabel}]</span>
                    <span class="status-sep">·</span>
                    <span class="status-state state-${displayStatus}">${stateLabel}</span>
                    <span class="status-sep">·</span>
                    <span class="status-time">${statusTime}</span>
                </div>
                <div class="detail-meta-row">
                    <span class="detail-meta-chip">${escapeHtml(teamLabel)}</span>
                    <span class="detail-meta-chip"${onCallTitle}>On-call ${escapeHtml(onCallDisplay)}</span>
                    <span class="detail-meta-chip">Last update ${updatedRelative}</span>
                    ${ackName ? `<span class="detail-meta-chip"${ackTitle}>Ack by ${escapeHtml(ackDisplay)}</span>` : ''}
                </div>
            </div>

            <details class="detail-section detail-technical">
                <summary class="detail-section-title detail-summary">
                    <span class="detail-summary-title">Technical details</span>
                </summary>
                <div class="detail-grid">
                    <div class="detail-item">
                        <div class="detail-label">ID</div>
                        <div class="detail-value" style="font-family: monospace; font-size: 0.8rem;">${alertGroup.id}</div>
                    </div>
                    <div class="detail-item">
                        <div class="detail-label">Dedup Key</div>
                        <div class="detail-value" style="font-family: monospace; font-size: 0.8rem;">${escapeHtml(alertGroup.dedup_key || 'N/A')}</div>
                    </div>
                    <div class="detail-item">
                        <div class="detail-label">Policy</div>
                        <div class="detail-value">${escapeHtml(alertGroup.policy_id || 'N/A')}</div>
                    </div>
                    ${alertGroup.external_url ? `
                        <div class="detail-item">
                            <div class="detail-label">Source</div>
                            <div class="detail-value">
                                <a href="${escapeHtml(alertGroup.external_url)}" target="_blank" rel="noopener noreferrer" 
                                   style="color: var(--accent-primary); text-decoration: none; display: flex; align-items: center; gap: 4px;">
                                    <i data-lucide="external-link" style="width:14px;height:14px;"></i>
                                    Alertmanager
                                </a>
                            </div>
                        </div>
                    ` : ''}
                </div>
                <div class="detail-subsection">
                    <div class="detail-subtitle">Timestamps</div>
                    <div class="detail-grid">
                        <div class="detail-item">
                            <div class="detail-label">Created</div>
                            <div class="detail-value">${Components.formatTime(alertGroup.created_at)}</div>
                        </div>
                        <div class="detail-item">
                            <div class="detail-label">Updated</div>
                            <div class="detail-value">${Components.formatTime(alertGroup.updated_at)}</div>
                        </div>
                        ${alertGroup.resolved_at ? `
                            <div class="detail-item">
                                <div class="detail-label">Resolved</div>
                                <div class="detail-value">${Components.formatTime(alertGroup.resolved_at)}</div>
                            </div>
                        ` : ''}
                    </div>
                </div>
            </details>

            ${alertGroup.alerts?.length > 0 ? `
                <div class="detail-section">
                    <h3 class="detail-section-title">${alertsSummary}</h3>
                    <div class="alerts-list">
                        ${alertGroup.alerts.map(alert => Components.alertItem(alert)).join('')}
                    </div>
                </div>
            ` : (!alertGroup.alerts && totalCount > 0 ? `
                <div class="detail-section">
                    <h3 class="detail-section-title">${alertsSummary}</h3>
                    <div class="loading-spinner">Loading alerts...</div>
                </div>
            ` : '')}

            <div class="detail-section timeline-section">
                <h3 class="detail-section-title">Timeline</h3>
                <div id="alert-group-timeline" class="timeline-container">
                    <div class="loading-spinner">Loading timeline...</div>
                </div>
            </div>
        `;
    },

    /**
     * Render alert item
     * @param {Object} alert - Alert data
     */
    alertItem: (alert) => {
        const labels = alert.labels || {};
        const summary = alert.annotations?.description || alert.annotations?.summary || '';

        const instanceKeys = ['instance', 'pod', 'node', 'host', 'ip', 'container'];
        let instanceLabel = '';
        let instanceKeyUsed = '';
        for (const key of instanceKeys) {
            if (labels[key]) {
                instanceLabel = `${key} ${labels[key]}`;
                instanceKeyUsed = key;
                break;
            }
        }
        const alertName = labels.alertname || 'Unknown Alert';
        const headerTitle = instanceLabel || alertName;

        // Filter out common labels for display
        const allLabels = Object.entries(labels)
            .filter(([key]) => !['alertname', '__name__', instanceKeyUsed].includes(key));
        const visibleLabels = allLabels.slice(0, 3);
        const hiddenLabels = allLabels.slice(3);
        const labelSpan = ([k, v]) => `<span class="alert-label-nowrap">${escapeHtml(k)}=${escapeHtml(v)}</span>`;
        const visibleHtml = visibleLabels.map(labelSpan).join(' · ');
        const hiddenHtml = hiddenLabels.map(labelSpan).join(' · ');

        return `
            <div class="alert-item status-${alert.status}">
                <div class="alert-header">
                    <span class="alert-name">${escapeHtml(headerTitle)}</span>
                    ${Components.alertStatusBadge(alert.status)}
                </div>
                ${summary ? `
                    <div class="alert-annotation">${escapeHtml(summary)}</div>
                ` : ''}
                ${visibleLabels.length > 0 ? `
                    <div class="alert-labels-inline">
                        ${hiddenLabels.length > 0 ? `<span class="alert-labels-toggle" onclick="const h=this.parentElement.querySelector('.alert-labels-hidden');if(h.style.display==='none'){h.style.display='inline';this.textContent='hide';}else{h.style.display='none';this.textContent='+${hiddenLabels.length} more';}">+${hiddenLabels.length} more</span>` : ''}
                        <span class="alert-labels-prefix">labels:</span>
                        <span>${visibleHtml}</span>${hiddenLabels.length > 0 ? `<span class="alert-labels-hidden" style="display:none"> · ${hiddenHtml}</span>` : ''}
                    </div>
                ` : ''}
            </div>
        `;
    },

    /**
     * Render modal footer actions based on alert group status
     * @param {Object} alertGroup - AlertGroup data
     */
    alertGroupActions: (alertGroup) => {
        const actions = [];

        const displayStatus = getDisplayStatus(alertGroup.status);
        const canAck = Permissions.can('ack_alert', { teamId: alertGroup.team_id });
        const canResolve = Permissions.can('resolve_alert', { teamId: alertGroup.team_id });

        if (displayStatus === 'triggered') {
            if (canAck) {
                actions.push(`
                    <button class="btn btn-primary" id="action-ack" data-alert-group-id="${alertGroup.id}">
                        <i data-lucide="check"></i> Acknowledge
                    </button>
                `);
            }
            if (canResolve) {
                actions.push(`
                    <button class="btn btn-secondary" id="action-resolve" data-alert-group-id="${alertGroup.id}" data-alert-group-status="${displayStatus}">
                        <i data-lucide="check-circle"></i> Resolve
                    </button>
                `);
            }
        } else if (displayStatus === 'acknowledged') {
            if (canResolve) {
                actions.push(`
                    <button class="btn btn-primary" id="action-resolve" data-alert-group-id="${alertGroup.id}" data-alert-group-status="${displayStatus}">
                        <i data-lucide="check-circle"></i> Resolve
                    </button>
                `);
            }
        }

        if (actions.length === 0) {
            const reason = displayStatus === 'resolved' ? 'Resolved' : 'No permission';
            actions.push(`<span class="text-muted" style="color: var(--text-muted);">No actions available (${reason})</span>`);
        }

        return actions.join('');
    },

    /**
     * Render toast notification
     * @param {string} message - Toast message
     * @param {string} type - Toast type (success, error, info)
     */
    toast: (message, type = 'info') => {
        return `
            <div class="toast ${type}">
                <span>${escapeHtml(message)}</span>
            </div>
        `;
    },

    /**
     * Render timeline events list
     * @param {Array} events - Timeline events
     */
    timeline: (events) => {
        if (!events || events.length === 0) {
            return '<div class="timeline-empty">No timeline events yet</div>';
        }

        return `
            <div class="timeline">
                ${events.map(e => Components.timelineEvent(e)).join('')}
            </div>
        `;
    },

    /**
     * Render single timeline event
     * @param {Object} event - Timeline event data
     */
    timelineEvent: (event) => {
        const icon = Components.getTimelineIcon(event.type);
        const time = new Date(event.created_at).toLocaleString(undefined, {
            month: 'short',
            day: 'numeric',
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
            timeZoneName: 'short'
        });
        const relative = Components.timeSince(event.created_at, { withAgo: true });
        const keyTypes = new Set(['created', 'acknowledged', 'resolved']);
        const emphasisClass = keyTypes.has(event.type) ? ' is-key' : ' is-minor';

        // Build notification details from metadata. Sprint 4 renamed
        // step_type values (slack_dm → dm, slack_channel → channel) and
        // introduced recipient_id in place of slack_user_id; firehose stays.
        let notificationDetails = '';
        if (event.type === 'notification_sent' && event.metadata) {
            const meta = event.metadata;
            if (meta.step_type === 'dm' && (meta.user_name || meta.recipient_id)) {
                const userName = meta.user_name || meta.recipient_id;
                notificationDetails = `
                    <div class="timeline-notification-details">
                        <i data-lucide="user"></i>
                        <span>${escapeHtml(userName)}</span>
                    </div>`;
            } else if ((meta.step_type === 'channel' || meta.step_type === 'firehose') && (meta.channel_name || meta.channel_id)) {
                const channelName = meta.channel_name || meta.channel_id;
                notificationDetails = `
                    <div class="timeline-notification-details">
                        <i data-lucide="hash"></i>
                        <span>${escapeHtml(channelName)}</span>
                    </div>`;
            }
        }

        return `
            <div class="timeline-event type-${event.type}${emphasisClass}">
                <div class="timeline-icon">${icon}</div>
                <div class="timeline-content">
                    <div class="timeline-time">${time} · ${relative}</div>
                    <div class="timeline-message">${escapeHtml(event.message)}</div>
                    ${notificationDetails}
                    ${event.actor && event.actor !== 'system' ?
                `<div class="timeline-actor">by ${escapeHtml(event.actor)}</div>` : ''}
                </div>
            </div>
        `;
    },

    /**
     * Get icon for timeline event type
     * @param {string} type - Event type
     */
    getTimelineIcon: (type) => {
        const icons = {
            'created': '<i data-lucide="bell-ring"></i>',
            'alert_added': '<i data-lucide="plus"></i>',
            'alert_resolved': '<i data-lucide="check"></i>',
            'acknowledged': '<i data-lucide="user-check"></i>',
            'resolved': '<i data-lucide="check-circle"></i>',
            'notification_sent': '<i data-lucide="send"></i>',
            'notification_failed': '<i data-lucide="x-circle"></i>',
            'note': '<i data-lucide="file-text"></i>',
            'status_change': '<i data-lucide="refresh-cw"></i>',
        };
        return icons[type] || '<i data-lucide="circle"></i>';
    },

    // ========================================
    // User & Team Components
    // ========================================

    /**
     * Render team navigation item for sidebar
     * @param {Object} team - Team data
     * @param {boolean} isActive - Whether this team is selected
     */
    teamNavItem: (team, isActive) => {
        return `
            <a href="#" class="nav-item nav-team ${isActive ? 'active' : ''}" data-team="${team.id}">
                <i data-lucide="users" class="nav-icon"></i>
                <span class="nav-text">${escapeHtml(team.name)}</span>
            </a>
        `;
    },

    /**
     * Render All Teams section container
     * Shows Create Team button for all, disabled for non-admins
     */
    allTeamsSection: () => {
        const canCreate = Permissions.can('create_team');
        const btnDisabled = canCreate ? '' : 'disabled';
        const btnTooltip = canCreate ? '' : 'title="Admin only"';

        return `
            <div class="section-header">
                <h2 class="section-title">
                    <i data-lucide="users" class="section-icon"></i>
                    <span>All Teams</span>
                </h2>
                <button class="btn btn-primary" id="create-team-view-btn" ${btnDisabled} ${btnTooltip}>
                    <i data-lucide="plus"></i>
                    <span>Create Team</span>
                </button>
            </div>
            <div class="teams-list" id="all-teams-grid"></div>
            <div class="users-loading" id="all-teams-loading" style="display: none;">
                <div class="spinner"></div>
                <span>Loading teams...</span>
            </div>
        `;
    },

    /**
     * Render empty teams state
     */
    emptyTeamsState: () => {
        return `
            <div class="empty-state">
                <i data-lucide="users" class="empty-icon"></i>
                <p>No teams found</p>
                <button class="btn btn-primary" style="margin-top:16px;" onclick="document.getElementById('create-team-view-btn').click()">
                    Create your first team
                </button>
            </div>
        `;
    },

    /**
     * Render team card for All Teams grid (Configuration Page - Variant A)
     * Shows configuration status: Users count, On-call status
     * @param {Object} team - Team data with member_count and on_call_configured
     */
    teamCard: (team) => {
        const memberCount = team.member_count ?? 0;
        const onCallConfigured = team.on_call_configured ?? false;
        const onCallLabel = onCallConfigured ? 'Configured' : 'Not configured';
        const onCallClass = onCallConfigured ? 'configured' : 'unconfigured';
        const teamInitial = team.name ? team.name.charAt(0).toUpperCase() : '?';
        const hasSchedule = onCallConfigured ? 'true' : 'false';

        return `
            <div class="team-row team-card" data-team-id="${team.id}" data-has-schedule="${hasSchedule}">
                <div class="team-cell team-cell-primary">
                    <div class="team-avatar">
                        <span>${escapeHtml(teamInitial)}</span>
                    </div>
                    <div class="team-primary-text">
                        <div class="team-name">${escapeHtml(team.name)}</div>
                        <div class="team-id">${escapeHtml(team.id)}</div>
                    </div>
                </div>
                <div class="team-cell" data-label="Members">
                    <span class="team-meta">${memberCount} ${memberCount === 1 ? 'user' : 'users'}</span>
                </div>
                <div class="team-cell" data-label="On-call">
                    <span class="badge badge-status team-oncall-badge ${onCallClass}">${onCallLabel}</span>
                </div>
            </div>
        `;
    },

    /**
     * Render teams list
     * @param {Array} teams - Array of team objects
     */
    teamsList: (teams) => {
        if (!teams || teams.length === 0) {
            return Components.emptyTeamsState();
        }

        return `
            <div class="teams-list-header">
                <div>Team</div>
                <div>Members</div>
                <div>On-call</div>
            </div>
            <div class="teams-list-body">
                ${teams.map(team => Components.teamCard(team)).join('')}
            </div>
        `;
    },

    /**
     * Render team title with icon
     * @param {string} teamName - Team name
     */
    teamTitle: (teamName) => {
        return `<i data-lucide="users" class="section-icon"></i> ${escapeHtml(teamName)}`;
    },



    /**
     * Render user row for users list
     * @param {Object} user - User data
     */
    userCard: (user) => {
        const hasSlack = Array.isArray(user.identities) && user.identities.some(i => i.provider === 'slack' && i.external_id);
        const authProvider = (user.auth_provider || '').toLowerCase();
        const isOidc = authProvider === 'oidc';
        const authLabel = isOidc ? 'SSO' : 'Local';
        const authClass = isOidc ? 'auth-oidc' : 'auth-local';
        const slackStatus = hasSlack ? 'linked' : 'not linked';
        const slackClass = hasSlack ? 'status-linked' : 'status-unlinked';
        const roleLabel = user.role === 'admin' ? 'Admin' : 'User';
        const roleClass = user.role === 'admin' ? 'severity-critical' : 'severity-info';
        const nameInitial = user.name ? user.name.charAt(0).toUpperCase() : '?';

        return `
            <div class="user-row" data-user-id="${user.id}">
                <div class="user-cell user-cell-primary">
                    <div class="user-avatar">
                        <span>${escapeHtml(nameInitial)}</span>
                    </div>
                    <div class="user-primary-text">
                        <div class="user-name">${escapeHtml(user.name)}</div>
                        <div class="user-email">${escapeHtml(user.email)}</div>
                    </div>
                </div>
                <div class="user-cell" data-label="Role">
                    <span class="badge badge-severity ${roleClass}">${roleLabel}</span>
                </div>
                <div class="user-cell" data-label="Auth">
                    <span class="badge badge-status user-auth-badge ${authClass}">${authLabel}</span>
                </div>
                <div class="user-cell" data-label="Slack">
                    <span class="badge badge-status user-slack-badge ${slackClass}">Slack: ${slackStatus}</span>
                </div>
            </div>
        `;
    },

    /**
     * Render users list/grid
     * @param {Array} users - Array of user objects
     */
    usersList: (users) => {
        if (!users || users.length === 0) {
            return `
                <div class="empty-state">
                    <i data-lucide="users" class="empty-icon"></i>
                    <p>No users found</p>
                    <p class="empty-hint">Create your first user to get started</p>
                </div>
            `;
        }

        return `
            <div class="users-list-header">
                <div>User</div>
                <div>Role</div>
                <div>Auth</div>
                <div>Slack</div>
            </div>
            <div class="users-list-body">
                ${users.map(user => Components.userCard(user)).join('')}
            </div>
        `;
    },

    /**
     * Render team management modal content
     * @param {Object} team - Team data
     * @param {Array} members - Team members with user details
     * @param {Array} allUsers - All users for adding new members
     * @param {Array} policies - Team's escalation policies for routing
     */
    teamManagementModal: (team, members, allUsers, policies = []) => {
        const memberIds = new Set(members.map(m => m.id));
        const availableUsers = allUsers.filter(u => !memberIds.has(u.id));

        // Check if user can edit this team
        const canEdit = Permissions.can('edit_team', { teamId: team?.id });
        const disabledAttr = canEdit ? '' : 'disabled';

        // Severity routes from team
        const severityRoutes = team?.severity_routes || {};

        return `
            <div class="team-modal-content">
                ${!canEdit ? `
                <div class="read-only-notice" style="background: var(--bg-tertiary); padding: 8px 12px; border-radius: var(--radius-md); margin-bottom: 16px; font-size: 0.85rem; color: var(--text-secondary);">
                    <i data-lucide="eye" style="width:14px;height:14px;display:inline-block;vertical-align:middle;margin-right:4px;"></i>
                    Read-only view. You don't have permission to edit this team.
                </div>
                ` : ''}
                
                <!-- Routing Section -->
                <div class="team-modal-section">
                    <h4 class="team-modal-section-title">
                        <i data-lucide="git-branch-plus"></i>
                        Escalation Policy Routing
                    </h4>
                    ${policies.length === 0 ? `
                        <div class="empty-hint">
                            <i data-lucide="info"></i>
                            No policies for this team. Create a policy first.
                        </div>
                    ` : `
                        <div class="routing-form">
                            <div class="form-group">
                                <label>Default Policy</label>
                                <select id="routing-default-policy" class="form-select" ${disabledAttr}>
                                    <option value="">No default policy</option>
                                    ${policies.map(p => `<option value="${p.id}" ${team?.default_policy_id === p.id ? 'selected' : ''}>${escapeHtml(p.name)}</option>`).join('')}
                                </select>
                                <small class="form-hint">Used when no severity-specific route matches</small>
                            </div>
                            
                            <div class="severity-routes-grid">
                                <div class="form-group">
                                    <label><span class="severity-badge critical">Critical</span></label>
                                    <select id="routing-critical" class="form-select" ${disabledAttr}>
                                        <option value="">Use default</option>
                                        ${policies.map(p => `<option value="${p.id}" ${severityRoutes.critical === p.id ? 'selected' : ''}>${escapeHtml(p.name)}</option>`).join('')}
                                    </select>
                                </div>
                                <div class="form-group">
                                    <label><span class="severity-badge warning">Warning</span></label>
                                    <select id="routing-warning" class="form-select" ${disabledAttr}>
                                        <option value="">Use default</option>
                                        ${policies.map(p => `<option value="${p.id}" ${severityRoutes.warning === p.id ? 'selected' : ''}>${escapeHtml(p.name)}</option>`).join('')}
                                    </select>
                                </div>
                                <div class="form-group">
                                    <label><span class="severity-badge info">Info</span></label>
                                    <select id="routing-info" class="form-select" ${disabledAttr}>
                                        <option value="">Use default</option>
                                        ${policies.map(p => `<option value="${p.id}" ${severityRoutes.info === p.id ? 'selected' : ''}>${escapeHtml(p.name)}</option>`).join('')}
                                    </select>
                                </div>
                            </div>
                            
                            ${canEdit ? `
                            <button class="btn btn-secondary" id="save-routing-btn" style="margin-top:12px;">
                                <i data-lucide="save" style="width:14px;height:14px;"></i>
                                Save Routing
                            </button>
                            ` : ''}
                        </div>
                    `}
                </div>

                <div class="team-modal-section">
                    <h4 class="team-modal-section-title">
                        <i data-lucide="users"></i>
                        Members (${members.length})
                    </h4>
                    <div class="team-members-list">
                        ${members.length === 0 ? `
                            <div class="empty-hint">No members in this team</div>
                        ` : members.map(member => `
                            <div class="team-member-row" data-user-id="${member.id}">
                                <div class="team-member-info">
                                    <span class="team-member-name">${escapeHtml(member.name)}</span>
                                    <span class="team-member-email">${escapeHtml(member.email)}</span>
                                </div>
                                <div class="team-member-role">
                                    ${canEdit ? `
                                    <select class="role-select" data-user-id="${member.id}">
                                        <option value="team_member" ${member.team_role === 'team_member' ? 'selected' : ''}>Member</option>
                                        <option value="team_admin" ${member.team_role === 'team_admin' ? 'selected' : ''}>Admin</option>
                                    </select>
                                    ` : `
                                    <span class="role-badge" style="font-size:0.85rem;color:var(--text-secondary);">
                                        ${member.team_role === 'team_admin' ? 'Admin' : 'Member'}
                                    </span>
                                    `}
                                </div>
                                ${canEdit ? `
                                <button class="btn btn-sm btn-icon remove-member-btn" data-user-id="${member.id}" title="Remove from team">
                                    <i data-lucide="x"></i>
                                </button>
                                ` : ''}
                            </div>
                        `).join('')}
                    </div>
                </div>

                ${canEdit && availableUsers.length > 0 ? `
                    <div class="team-modal-section">
                        <h4 class="team-modal-section-title">
                            <i data-lucide="user-plus"></i>
                            Add Member
                        </h4>
                        <div class="add-member-form">
                            <select id="add-member-select" class="form-select">
                                <option value="">Select user...</option>
                                ${availableUsers.map(u => `
                                    <option value="${u.id}">${escapeHtml(u.name)} (${escapeHtml(u.email)})</option>
                                `).join('')}
                            </select>
                            <select id="add-member-role" class="form-select">
                                <option value="team_member">Member</option>
                                <option value="team_admin">Admin</option>
                            </select>
                            <button class="btn btn-primary" id="add-member-btn">
                                <i data-lucide="plus" style="width:16px;height:16px;"></i>
                                Add
                            </button>
                        </div>
                    </div>
                ` : ''}
                
                ${Permissions.can('delete_team', { teamId: team?.id }) ? `
                    <div class="team-modal-section" style="margin-top: 24px; padding-top: 16px; border-top: 1px solid var(--border-color);">
                        <h4 class="team-modal-section-title" style="color: var(--severity-critical);">
                            <i data-lucide="alert-triangle"></i>
                            Danger Zone
                        </h4>
                        <p style="font-size: 0.85rem; color: var(--text-muted); margin-bottom: 12px;">
                            Deleting this team cannot be undone. Members will not be deleted.
                        </p>
                        <button class="btn btn-sm btn-danger delete-team-modal-btn" data-team-id="${team?.id}">
                            <i data-lucide="trash-2" style="width:14px;height:14px;"></i>
                            Delete Team
                        </button>
                    </div>
                ` : ''}
            </div>
        `;
    },

    /**
     * Render user form modal content (create/edit)
     * @param {Object|null} user - User data for editing, null for create
     */
    userFormModal: (user = null) => {
        const isEdit = user !== null;
        const authProvider = (user?.auth_provider || '').toLowerCase();
        const isOidc = authProvider === 'oidc';
        const roleValue = user?.role === 'admin' ? 'admin' : 'user';

        return `
            <form id="user-form" class="user-form">
                <div class="user-form-section">
                    <div class="user-form-section-title">Identity</div>
                    <div class="user-form-section-body">
                        <div class="user-form-grid user-form-grid-2">
                            <div class="form-group">
                                <label for="user-name">Name *</label>
                                <input type="text" id="user-name" name="name" required
                                       class="form-control"
                                       value="${isEdit ? escapeHtml(user.name) : ''}"
                                       placeholder="John Doe">
                            </div>
                            <div class="form-group">
                                <label for="user-email">Email *</label>
                                <input type="email" id="user-email" name="email" required
                                       class="form-control"
                                       value="${isEdit ? escapeHtml(user.email) : ''}"
                                       placeholder="john@example.com">
                            </div>
                        </div>
                    </div>
                </div>

                <div class="user-form-section-row">
                    <div class="user-form-section user-form-section-compact">
                        <div class="user-form-section-title">Role</div>
                        <div class="user-form-section-body">
                            <div class="form-group">
                                <label for="user-role">Role</label>
                                <select id="user-role" name="role" class="form-select">
                                    <option value="user" ${roleValue === 'user' ? 'selected' : ''}>User</option>
                                    <option value="admin" ${roleValue === 'admin' ? 'selected' : ''}>Admin</option>
                                </select>
                                <small class="form-hint">Admins have full access to all teams and settings.</small>
                            </div>
                        </div>
                    </div>

                    <!-- Slack linking moved to /profile (Settings → Integrations).
                         Admin user-edit no longer manages external identities directly. -->
                </div>

                <div class="user-form-section">
                    <div class="user-form-section-title">Security</div>
                    <div class="user-form-section-body">
                        ${!isEdit ? `
                        <div class="form-group">
                            <label for="user-password">Password <span class="optional">(optional for SSO)</span></label>
                            <input type="password" id="user-password" name="password"
                                   class="form-control"
                                   placeholder="Enter password"
                                   autocomplete="new-password">
                            <small class="form-text text-muted" style="display:block; margin-top:4px; font-size:0.75rem;">
                                Min 8 chars, 1 uppercase, 1 lowercase, 1 number, 1 special char.
                            </small>
                        </div>
                        ` : `
                        ${isOidc ? `
                            <div class="empty-hint">
                                <i data-lucide="shield"></i>
                                SSO user. Password reset is not available.
                            </div>
                        ` : `
                            <div class="user-security-card">
                                <div class="user-security-header">
                                    <i data-lucide="alert-triangle"></i>
                                    <span>Reset password</span>
                                </div>
                                <p class="user-security-note">This action immediately replaces the current password.</p>
                                <div class="form-group">
                                    <label for="user-password-reset">New password</label>
                                    <input type="password" id="user-password-reset" class="form-control"
                                           placeholder="Enter new password" autocomplete="new-password">
                                    <small class="form-text text-muted" style="display:block; margin-top:4px; font-size:0.75rem;">
                                        Min 8 chars, 1 uppercase, 1 lowercase, 1 number, 1 special char.
                                    </small>
                                </div>
                                <button type="button" class="btn btn-danger user-security-action" id="user-reset-password-btn">
                                    Reset password
                                </button>
                            </div>
                        `}
                        `}
                    </div>
                </div>

                ${isEdit ? `
                <details class="user-form-details">
                    <summary>Technical details</summary>
                    <div class="user-form-details-body">
                        <div class="form-group">
                            <label>User ID</label>
                            <div class="readonly-field">${escapeHtml(user.id)}</div>
                        </div>
                    </div>
                </details>
                ` : ''}

                ${isEdit && Permissions.can('manage_users') ? `
                <div class="team-modal-section" style="margin-top: 24px; padding-top: 16px; border-top: 1px solid var(--border-color);">
                    <h4 class="team-modal-section-title" style="color: var(--severity-critical);">
                        <i data-lucide="alert-triangle"></i>
                        Danger Zone
                    </h4>
                    <p style="font-size: 0.85rem; color: var(--text-muted); margin-bottom: 12px;">
                        Deleting this user will remove access and cannot be undone.
                    </p>
                    <button type="button" class="btn btn-sm btn-danger delete-user-modal-btn">
                        <i data-lucide="trash-2" style="width:14px;height:14px;"></i>
                        Delete User
                    </button>
                </div>
                ` : ''}
            </form>
        `;
    },

    // ========================================
    // Policy Components (Phase 4)
    // ========================================

    /**
     * Render searchable user select with typeahead
     * @param {Array} users - Available users
     * @param {string} selectedId - Currently selected user ID
     * @param {string} datalistId - Unique ID for datalist
     */
    searchableUserSelect: (users, selectedId, datalistId) => {
        const selectedUser = users.find(u => u.id === selectedId);
        const displayValue = selectedUser ? `${selectedUser.name} (${selectedUser.email})` : '';

        return `
            <div class="searchable-select">
                <input type="text" 
                    class="form-input user-search-input" 
                    list="${datalistId}" 
                    placeholder="Search users..." 
                    value="${escapeHtml(displayValue)}"
                    autocomplete="off">
                <input type="hidden" class="target-id-input" value="${escapeHtml(selectedId || '')}">
                <datalist id="${datalistId}">
                    ${users.map(u => `<option value="${escapeAttr(u.name)} (${escapeAttr(u.email)})" data-user-id="${u.id}" data-user-name="${escapeAttr(u.name)}"></option>`).join('')}
                </datalist>
            </div>
        `;
    },


    /**
     * Render policy card for policies grid
     * @param {Object} policy - Policy data
     */
    policyCard: (policy) => {
        const teamId = policy.team_id || null;
        const isGlobal = !teamId;
        const canEdit = Permissions.can('edit_policy', { teamId });
        const canDelete = Permissions.can('delete_policy', { teamId });

        // Build scope badge
        const scopeBadge = isGlobal
            ? `<span class="badge-global">🌐 Global</span>`
            : `<span class="badge-team">Team: ${escapeHtml(policy.team_id)}</span>`;

        return `
            <div class="alert-group-card policy-card" data-policy-id="${policy.id}">
                <div class="alert-group-card-header">
                    <div style="display:flex;align-items:center;gap:12px;">
                        <div class="team-icon" style="width:40px;height:40px;border-radius:50%;background:${isGlobal ? 'linear-gradient(135deg, #eab308, #ca8a04)' : 'var(--accent-primary)'};display:flex;align-items:center;justify-content:center;color:white;">
                            <i data-lucide="${isGlobal ? 'globe' : 'shield'}" style="width:20px;height:20px;"></i>
                        </div>
                        <div>
                            <h3 class="alert-group-title" style="margin:0;font-size:1.1rem;">${escapeHtml(policy.name)}</h3>
                            ${scopeBadge}
                        </div>
                    </div>
                </div>
                
                <div class="policy-steps-preview" style="margin:16px 0;">
                    ${Components.policyStepChips(policy.steps || [])}
                </div>

                <div class="team-meta" style="margin-bottom:16px;font-size:0.85rem;color:var(--text-muted);">
                    ${policy.description ? `<div style="margin-bottom:8px;">${escapeHtml(policy.description)}</div>` : ''}
                    <div>Updated: ${Components.timeAgo(policy.updated_at)}</div>
                </div>
                
                <div class="user-card-actions" style="margin-top:auto;padding-top:16px;border-top:1px solid rgba(255,255,255,0.1);justify-content:flex-start;gap:8px;">
                    ${canEdit ? `
                    <button class="btn btn-sm btn-secondary edit-policy-btn" data-policy-id="${policy.id}" title="Edit Policy">
                        <i data-lucide="pencil" style="width:14px;height:14px;"></i>
                    </button>
                    <button class="btn btn-sm btn-secondary duplicate-policy-btn" data-policy-id="${policy.id}" title="Duplicate Policy">
                        <i data-lucide="copy" style="width:14px;height:14px;"></i>
                    </button>
                    ` : ''}
                    ${canDelete ? `
                    <button class="btn btn-sm btn-danger delete-policy-btn" data-policy-id="${policy.id}" title="Delete Policy">
                        <i data-lucide="trash-2" style="width:14px;height:14px;"></i>
                    </button>
                    ` : ''}
                    ${!canEdit && !canDelete ? '<span class="text-muted" style="font-size:0.8rem;">Read-only</span>' : ''}
                </div>
            </div>
        `;
    },

    /**
     * Render step chips visualization
     * @param {Array} steps - Array of step objects
     */
    policyStepChips: (steps) => {
        if (!steps || steps.length === 0) {
            return '<span class="text-muted" style="font-size:0.85rem;">No steps configured</span>';
        }

        const chips = ['<span class="step-chip step-start">Start</span>'];

        steps.forEach((step, i) => {
            if (step.delay_seconds > 0) {
                const mins = Math.floor(step.delay_seconds / 60);
                const secs = step.delay_seconds % 60;
                const delayStr = mins > 0 ? `${mins}m${secs > 0 ? ` ${secs}s` : ''}` : `${secs}s`;
                chips.push(`<span class="step-delay">(${delayStr})</span>`);
            }

            // Sprint 4: chips read step.target_kind ("dm" / "channel").
            const isChannel = step.target_kind === 'channel';
            const icon = isChannel ? 'hash' : 'user';
            const label = isChannel ? 'Channel' : 'DM';
            chips.push(`<span class="step-chip"><i data-lucide="${icon}" style="width:12px;height:12px;"></i> ${label}</span>`);
        });

        return `<div class="step-chips-container" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">${chips.join('<span class="step-arrow">→</span>')}</div>`;
    },

    /**
     * Render single step row in editor
     * @param {Object} step - Step data
     * @param {number} index - Step index
     * @param {Array} users - Available users
     * @param {Array} teams - Available teams (for schedule display)
     * @param {string} policyTeamId - Current policy's team ID
     */
    policyStepRow: (step, index, users = [], teams = [], policyTeamId = '', scheduleId = '', providers = []) => {
        // Sprint 4: build the type dropdown from provider capabilities. Each
        // <option> value is "<provider>:<target_kind>", and the human label
        // capitalizes both pieces. If the registry is empty (offline / bad
        // wiring) fall back to a single Slack DM option so the editor stays
        // usable.
        const stepTypeOptions = [];
        if (providers.length === 0) {
            stepTypeOptions.push({ value: 'slack:dm', label: 'Slack DM', kind: 'dm' });
        } else {
            providers.forEach((p) => {
                (p.supported_target_kinds || []).forEach((kind) => {
                    const provLabel = p.name.charAt(0).toUpperCase() + p.name.slice(1);
                    const kindLabel = kind === 'dm' ? 'DM' : (kind.charAt(0).toUpperCase() + kind.slice(1));
                    stepTypeOptions.push({ value: `${p.name}:${kind}`, label: `${provLabel} ${kindLabel}`, kind });
                });
            });
        }
        const currentValue = `${step.provider || ''}:${step.target_kind || ''}`;
        // If the persisted (provider, target_kind) pair is no longer in the
        // registry (provider removed, kind dropped), surface it as a disabled
        // option so the editor doesn't silently switch step types.
        if (currentValue !== ':' && !stepTypeOptions.some(o => o.value === currentValue)) {
            stepTypeOptions.push({ value: currentValue, label: `${currentValue} (unknown)`, kind: step.target_kind || 'dm' });
        }
        const stepTypeSelectHtml = stepTypeOptions.map(o =>
            `<option value="${escapeHtml(o.value)}" ${o.value === currentValue ? 'selected' : ''}>${escapeHtml(o.label)}</option>`
        ).join('');

        const isChannel = step.target_kind === 'channel';

        // Schedule display logic
        const isSchedule = step.target_type === 'schedule';
        const scheduleValue = isSchedule ? (step.target_id || scheduleId || '') : '';

        // Determine label
        let scheduleLabel = 'Select team first';
        if (policyTeamId) {
            const team = teams.find(t => t.id === policyTeamId);
            if (team) {
                scheduleLabel = scheduleValue ? `Team Schedule (${team.name})` : 'No schedule configured';
            }
        }

        return `
            <div class="policy-step-row" data-step-index="${index}">
                <div class="step-header">
                    <div class="step-drag-handle">
                        <i data-lucide="grip-vertical"></i>
                    </div>
                    <span class="step-index-label">Step ${index + 1}</span>
                    <div class="step-header-actions">
                        <label class="toggle-switch" title="Continue to next step on failure">
                            <input type="checkbox" class="continue-on-failure-input" ${step.continue_on_failure !== false ? 'checked' : ''}>
                            <span class="toggle-slider"></span>
                            <span class="toggle-text">Continue on fail</span>
                        </label>
                        <button type="button" class="btn btn-icon btn-sm remove-step-btn" title="Remove step">
                            <i data-lucide="trash-2"></i>
                        </button>
                    </div>
                </div>

                <div class="step-fields">
                    <div class="step-field step-field-type">
                        <label>Type</label>
                        <select class="form-select step-type-select">
                            ${stepTypeSelectHtml}
                        </select>
                    </div>
                    
                    <div class="step-field step-field-target-type">
                        <label>Target Type</label>
                        <select class="form-select target-type-select">
                            ${isChannel ?
                '<option value="channel">Channel</option>' :
                `<option value="user" ${step.target_type === 'user' ? 'selected' : ''}>User</option>
                                 <option value="schedule" ${step.target_type === 'schedule' ? 'selected' : ''}>Schedule</option>`
            }
                        </select>
                    </div>
                    
                    <div class="step-field step-field-target target-selector-container">
                        <label>Target</label>
                        ${isChannel ?
                `<input type="text" class="form-input target-id-input" placeholder="C01234567" value="${escapeHtml(step.target_id || '')}">` :
                step.target_type === 'schedule' ?
                    `<select class="form-select target-id-input"><option value="${escapeHtml(scheduleValue)}" data-schedule-placeholder="true">${escapeHtml(scheduleLabel)}</option></select>` :
                    Components.searchableUserSelect(users, step.target_id, `user-datalist-${index}`)
            }
                    </div>
                </div>

                <div class="step-timing">
                    <div class="step-field step-field-sm">
                        <label>Delay (s)</label>
                        <input type="number" class="form-input delay-input" value="${step.delay_seconds || 0}" min="0">
                    </div>
                    <div class="step-field step-field-sm">
                        <label>Timeout (s)</label>
                        <input type="number" class="form-input timeout-input" value="${step.timeout_seconds || 30}" min="1">
                    </div>
                    <div class="step-field step-field-sm">
                        <label>Retries</label>
                        <input type="number" class="form-input max-attempts-input" value="${step.max_attempts || 5}" min="1" max="10">
                    </div>
                    <div class="step-field step-field-message">
                        <label>Message <span class="variables-hint" title="{{.Title}}, {{.Severity}}, {{.Team}}, {{.AlertsCount}}">ⓘ</span></label>
                        <input type="text" class="form-input message-input" placeholder="Custom message (optional)" value="${escapeHtml(step.message || '')}">
                    </div>
                </div>
            </div>
        `;
    },

    /**
     * Render policy builder modal content
     * @param {Object|null} policy - Policy data for editing, null for create
     * @param {Array} teams - Available teams
     * @param {Array} users - Available users
     */
    policyBuilderModal: (policy, teams, users) => {
        const isEdit = policy !== null;
        const isGlobalPolicy = isEdit && !policy.team_id;
        const currentScope = isGlobalPolicy ? 'global' : 'team';
        const isAdmin = Permissions.isAdmin();
        const defaultProvider = (State.providers || [])[0]?.name || '';
        const steps = policy?.steps || [{ provider: defaultProvider, target_kind: 'dm', target_type: 'user', target_id: '', delay_seconds: 0, timeout_seconds: 30, max_attempts: 5, message: '', continue_on_failure: true }];

        // Build scope selector HTML
        const scopeSelectorHtml = isEdit
            ? `
                <!-- Locked scope tabs for editing - same look but disabled -->
                <div class="form-group">
                    <label>Scope</label>
                    <div class="scope-tabs-sm">
                        <button type="button" class="scope-tab-sm ${!isGlobalPolicy ? 'active' : ''}" data-scope="team" disabled>Team</button>
                        <button type="button" class="scope-tab-sm ${isGlobalPolicy ? 'active' : ''}" data-scope="global" disabled>🌐 Global</button>
                    </div>
                    <small class="form-hint">Scope cannot be changed after creation</small>
                    <input type="hidden" name="policy-scope" value="${currentScope}">
                </div>
            `
            : `
                <!-- Compact scope tabs for new policies -->
                <div class="form-group">
                    <label>Scope *</label>
                    <div class="scope-tabs-sm">
                        <button type="button" class="scope-tab-sm active" data-scope="team">Team</button>
                        <button type="button" class="scope-tab-sm ${!isAdmin ? 'disabled' : ''}" data-scope="global" ${!isAdmin ? 'disabled title="Admin only"' : ''}>🌐 Global</button>
                    </div>
                    <input type="hidden" name="policy-scope" value="team" id="policy-scope-input">
                </div>
            `;

        // Team selector (shown only when scope is 'team')
        // For edit mode of team-scoped policy, show disabled select
        const teamSelectorHtml = isEdit && !isGlobalPolicy
            ? `
                <div class="form-group">
                    <label for="policy-team-select">Team</label>
                    <select id="policy-team-select" class="form-select" disabled>
                        ${teams.map(t => `<option value="${t.id}" ${policy?.team_id === t.id ? 'selected' : ''}>${escapeHtml(t.name)}</option>`).join('')}
                    </select>
                    <small class="form-hint">Team cannot be changed after creation</small>
                </div>
            `
            : `
                <div class="form-group" id="team-selector-group" ${isEdit ? 'style="display:none;"' : ''}>
                    <label for="policy-team-select">Team *</label>
                    <select id="policy-team-select" class="form-select">
                        <option value="">Select team...</option>
                        ${teams.map(t => `<option value="${t.id}" ${policy?.team_id === t.id ? 'selected' : ''}>${escapeHtml(t.name)}</option>`).join('')}
                    </select>
                </div>
            `;

        return `
            <div class="policy-builder">
                <div class="policy-meta-section" style="display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:24px;align-items:start;">
                    <div class="form-group">
                        <label for="policy-name-input">Policy Name *</label>
                        <input type="text" id="policy-name-input" class="form-input" placeholder="e.g., Critical Escalation" value="${escapeHtml(policy?.name || '')}">
                    </div>
                    <div>
                        ${scopeSelectorHtml}
                        ${teamSelectorHtml}
                    </div>
                </div>

                <div class="policy-steps-section">
                    <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:16px;">
                        <h4 style="margin:0;">Escalation Steps</h4>
                        <button class="btn btn-sm btn-secondary" id="add-step-btn">
                            <i data-lucide="plus" style="width:14px;height:14px;"></i>
                            Add Step
                        </button>
                    </div>
                    
                    <div id="policy-steps-list" class="policy-steps-list" style="display:flex;flex-direction:column;gap:16px;background:rgba(0,0,0,0.2);border-radius:8px;padding:16px;">
                        ${steps.map((step, i) => Components.policyStepRow(step, i, users, teams, policy?.team_id || '', '', State.providers || [])).join('')}
                    </div>
                </div>

                <div class="form-group" style="margin-top:24px;">
                    <label for="policy-description-input">Description</label>
                    <textarea id="policy-description-input" class="form-input" rows="2" placeholder="Optional description">${escapeHtml(policy?.description || '')}</textarea>
                </div>
            </div>
        `;
    },

    /**
     * Render manual alert group modal content
     * @param {Object} data - Modal data
     * @param {Array} data.teams - Team list
     * @param {string} data.teamId - Selected team ID
     * @param {string} data.severity - Selected severity
     * @param {string} data.title - Default title
     * @param {boolean} data.teamLocked - Whether team select is locked
     */
    manualAlertGroupModal: ({ teams = [], teamId = '', severity = 'critical', title = '', teamLocked = false }) => {
        const teamOptions = teams.map(t => `<option value="${escapeHtml(t.id)}" ${t.id === teamId ? 'selected' : ''}>${escapeHtml(t.name || t.id)}</option>`).join('');
        const teamDisabled = teamLocked ? 'disabled' : '';
        const teamHint = teamLocked
            ? `<small class="form-hint">Team is fixed by policy scope.</small>`
            : `<small class="form-hint">Escalation uses team + severity routing.</small>`;
        return `
            <form id="manual-alert-form">
                <div class="policy-builder-alert">
                    <i data-lucide="alert-triangle"></i>
                    <div>
                        This will create a manual alert group and send <strong>real notifications</strong>.
                        Routing follows team + severity mapping.
                    </div>
                </div>
                <div class="form-group">
                    <label for="manual-alert-team">Team *</label>
                    <select id="manual-alert-team" class="form-select" ${teamDisabled}>
                        ${teamOptions}
                    </select>
                    ${teamHint}
                </div>
                <div class="form-group">
                    <label for="manual-alert-severity">Severity *</label>
                    <select id="manual-alert-severity" class="form-select">
                        <option value="critical" ${severity === 'critical' ? 'selected' : ''}>Critical</option>
                        <option value="warning" ${severity === 'warning' ? 'selected' : ''}>Warning</option>
                        <option value="info" ${severity === 'info' ? 'selected' : ''}>Info</option>
                    </select>
                </div>
                <div class="form-group">
                    <label for="manual-alert-title">Title</label>
                    <input type="text" id="manual-alert-title" class="form-input" value="${escapeHtml(title)}" placeholder="Manual Alert">
                    <small class="form-hint">Title will be prefixed with [MANUAL].</small>
                </div>
            </form>
        `;
    },

    // ========================================
    // Integration Components
    // ========================================

    /**
     * Render integration card for integrations grid
     * @param {Object} integration - Integration data
     */
    integrationCard: (integration) => {
        const typeIconMap = {
            'slack': 'message-square',
            'telegram': 'send',
            'alertmanager_webhook': 'webhook',
            'generic_webhook': 'webhook'
        };
        const typeNameMap = {
            'slack': 'Slack',
            'telegram': 'Telegram',
            'alertmanager_webhook': 'Webhook',
            'generic_webhook': 'Generic Webhook'
        };

        const icon = typeIconMap[integration.type] || 'plug';
        const typeName = typeNameMap[integration.type] || integration.type;
        const isInbound = integration.direction === 'inbound';
        // Theme-aware muted colors - work in both light and dark
        const directionColor = isInbound ? '#059669' : '#2563eb';
        const directionBg = isInbound ? 'rgba(5, 150, 105, 0.1)' : 'rgba(37, 99, 235, 0.1)';
        const directionText = isInbound ? 'Inbound' : 'Outbound';
        const statusDot = integration.enabled
            ? '<span style="width: 8px; height: 8px; border-radius: 50%; background: #059669; display: inline-block;"></span>'
            : '<span style="width: 8px; height: 8px; border-radius: 50%; background: #dc2626; display: inline-block;"></span>';
        const statusText = integration.enabled ? 'Active' : 'Inactive';

        // Scope badge for generic_webhook
        let scopeBadge = '';
        if (integration.type === 'generic_webhook') {
            if (integration.team_id) {
                const team = (State.teams || []).find(t => t.id === integration.team_id);
                const teamName = team ? escapeHtml(team.name) : escapeHtml(integration.team_id);
                scopeBadge = `<span class="badge-team" style="margin-left: 8px;">${teamName}</span>`;
            } else {
                scopeBadge = '<span class="badge-global" style="margin-left: 8px;">Global</span>';
            }
        }

        // Deliveries button for generic_webhook
        const deliveriesBtn = integration.type === 'generic_webhook' ? `
            <button class="btn btn-sm btn-secondary deliveries-btn" data-integration-id="${integration.id}" title="Deliveries" style="padding: 6px 10px;">
                <i data-lucide="list" style="width: 14px; height: 14px;"></i>
            </button>
        ` : '';

        return `
            <div class="alert-group-card integration-card" data-integration-id="${integration.id}" style="cursor: pointer; position: relative;">
                <!-- Direction indicator + scope badge -->
                <div style="position: absolute; top: 12px; right: 12px; display: flex; align-items: center; gap: 6px;">
                    ${scopeBadge}
                    <div style="padding: 4px 10px; border-radius: 6px; background: ${directionBg}; color: ${directionColor}; font-size: 0.7rem; font-weight: 500; text-transform: uppercase; letter-spacing: 0.03em;">
                        ${directionText}
                    </div>
                </div>

                <!-- Header: Icon + Name -->
                <div style="display: flex; align-items: center; gap: 12px; margin-bottom: 16px; padding-right: 90px;">
                    <div style="width: 48px; height: 48px; border-radius: 12px; background: ${integration.type === 'slack' ? 'linear-gradient(135deg, #611f69, #4a154b)' : 'var(--accent-primary)'}; display: flex; align-items: center; justify-content: center; color: white; flex-shrink: 0;">
                        <i data-lucide="${icon}" style="width: 24px; height: 24px;"></i>
                    </div>
                    <div style="min-width: 0;">
                        <h3 style="margin: 0 0 4px 0; font-size: 1.1rem; font-weight: 600; color: var(--text-primary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${escapeHtml(integration.name)}</h3>
                        <span style="font-size: 0.8rem; color: var(--text-muted);">${typeName}</span>
                    </div>
                </div>

                <!-- Status & Meta -->
                <div style="display: flex; justify-content: space-between; align-items: center; padding-top: 12px; border-top: 1px solid var(--border-color);">
                    <div style="display: flex; align-items: center; gap: 6px; font-size: 0.8rem; color: var(--text-secondary);">
                        ${statusDot}
                        ${statusText}
                    </div>
                    <div style="display: flex; gap: 6px;">
                        ${deliveriesBtn}
                        <button class="btn btn-sm btn-secondary edit-integration-btn" data-integration-id="${integration.id}" title="Edit" style="padding: 6px 10px;">
                            <i data-lucide="pencil" style="width: 14px; height: 14px;"></i>
                        </button>
                        <button class="btn btn-sm btn-danger delete-integration-btn" data-integration-id="${integration.id}" title="Delete" style="padding: 6px 10px;">
                            <i data-lucide="trash-2" style="width: 14px; height: 14px;"></i>
                        </button>
                    </div>
                </div>
            </div>
        `;
    },

    /**
     * Render integration config fields based on type
     * @param {string} type - Integration type
     * @param {Object|null} config - Existing config (null for create)
     */
    integrationConfigFields: (type, config = null) => {
        if (type === 'slack') {
            const token = config?.token || '';
            const userToken = config?.user_token || '';
            const defaultChannel = config?.default_channel || '';
            const isMasked = token === '****';

            return `
                <div class="form-group">
                    <label for="config-token">Bot Token * <span class="tooltip-icon" data-tooltip="OAuth &amp; Permissions → Bot User OAuth Token"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-token" class="form-input" autocomplete="new-password"
                           placeholder="${isMasked ? 'Leave empty to keep existing' : 'xoxb-...'}"
                           value="">
                    ${isMasked ? '<small class="form-hint">Current token: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-user-token">User Token <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="OAuth &amp; Permissions → User OAuth Token (for usergroup sync)"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-user-token" class="form-input" autocomplete="new-password"
                           placeholder="${config?.user_token === '****' ? 'Leave empty to keep existing' : 'xoxp-...'}"
                           value="">
                    ${config?.user_token === '****' ? '<small class="form-hint">Current token: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-signing-secret">Signing Secret * <span class="tooltip-icon" data-tooltip="Settings → Basic Information → App Credentials → Signing Secret"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-signing-secret" class="form-input" autocomplete="new-password"
                           placeholder="${config?.signing_secret === '****' ? 'Leave empty to keep existing' : 'your signing secret'}"
                           value="">
                    ${config?.signing_secret === '****' ? '<small class="form-hint">Current secret: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-default-channel">Default Channel <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="Right-click channel → View channel details → copy Channel ID"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="text" id="config-default-channel" class="form-input"
                           placeholder="C01234567"
                           value="${escapeHtml(defaultChannel)}">
                    ${defaultChannel ? '<small class="form-hint">Leave empty to keep existing.</small>' : ''}
                </div>
                <div class="form-group">
                    <label class="toggle-switch">
                        <input type="checkbox" id="config-interactive" ${config?.interactive ? 'checked' : ''}>
                        <span class="toggle-slider"></span>
                        <span class="toggle-text">Enable interactive buttons (Acknowledge / Resolve)</span>
                    </label>
                    <small class="form-hint">Requires Interactivity URL configured in your Slack App settings.</small>
                </div>
            `;
        } else if (type === 'telegram') {
            const botToken = config?.bot_token || '';
            const secretToken = config?.secret_token || '';
            const defaultChatID = config?.default_chat_id || '';
            const botMasked = botToken === '****';
            const secretMasked = secretToken === '****';
            // Three states, three answers: a new integration starts off, an existing
            // one written before this field existed had buttons on, and an explicit
            // false stays off. Testing `config.interactive` alone would render the
            // legacy case unchecked and switch buttons off on the next save.
            const interactive = config != null && config.interactive !== false;

            return `
                <div class="form-group">
                    <label for="config-bot-token">Bot Token * <span class="tooltip-icon" data-tooltip="Create a bot via @BotFather → copy the HTTP API token"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-bot-token" class="form-input" autocomplete="new-password"
                           placeholder="${botMasked ? 'Leave empty to keep existing' : '123456:ABC-DEF...'}"
                           value="">
                    ${botMasked ? '<small class="form-hint">Current token: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-secret-token">Secret Token * <span class="tooltip-icon" data-tooltip="X-Telegram-Bot-Api-Secret-Token used to verify incoming webhook calls. Required - the webhook also carries account linking, so it is needed even with buttons off."><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-secret-token" class="form-input" autocomplete="new-password"
                           placeholder="${secretMasked ? 'Leave empty to keep existing' : 'webhook secret token'}"
                           value="">
                    ${secretMasked ? '<small class="form-hint">Current secret: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-default-chat-id">Default Chat ID <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="Numeric chat/channel id (e.g. -1001234567890). Reserved for future use."><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="text" id="config-default-chat-id" class="form-input"
                           placeholder="-1001234567890"
                           value="${escapeHtml(defaultChatID)}">
                    ${defaultChatID ? '<small class="form-hint">Leave empty to keep existing.</small>' : ''}
                </div>
                <div class="form-group">
                    <label class="toggle-switch">
                        <input type="checkbox" id="config-interactive" ${interactive ? 'checked' : ''}>
                        <span class="toggle-slider"></span>
                        <span class="toggle-text">Enable interactive buttons (Acknowledge / Resolve)</span>
                    </label>
                    <small class="form-hint">Requires a reachable webhook: TOKAY_SELF_URL must be set and the secret token configured.</small>
                </div>
            `;
        } else if (type === 'alertmanager_webhook') {
            const secret = config?.secret || '';
            const isMasked = secret === '****';
            const baseUrl = window.location.origin + '/webhook/alertmanager';
            const displayUrl = isMasked ? baseUrl + '?token=****' : baseUrl + '?token=YOUR_TOKEN';

            return `
                <div class="form-group">
                    <label for="config-secret">Webhook Token *</label>
                    <input type="password" id="config-secret" class="form-input" autocomplete="new-password"
                           placeholder="${isMasked ? 'Leave empty to keep existing' : 'your-secret-token'}"
                           value="">
                    ${isMasked ? '<small class="form-hint">Current token: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label>Webhook URL</label>
                    <div class="input-with-copy">
                        <input type="text" id="webhook-url-input" class="form-input" value="${escapeHtml(displayUrl)}" readonly>
                        <button type="button" class="btn btn-sm btn-secondary copy-url-btn">
                            <i data-lucide="copy" style="width:14px;height:14px;"></i>
                        </button>
                    </div>
                    <small class="form-hint">Use this URL in your Alertmanager configuration. Replace YOUR_TOKEN with your actual token.</small>
                </div>
            `;
        } else if (type === 'generic_webhook') {
            const url = config?.url || '';
            const secret = config?.secret || '';
            const isMasked = secret === '****';
            const timeout = config?.timeout_seconds || '';
            const headers = config?.custom_headers || {};
            const headersText = Object.entries(headers).map(([k, v]) => `${k}: ${v}`).join('\n');

            return `
                <div class="form-group">
                    <label for="config-webhook-url">URL * <span class="tooltip-icon" data-tooltip="The endpoint that will receive HTTP POST requests with alert event payloads"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="url" id="config-webhook-url" class="form-input"
                           placeholder="https://example.com/webhook"
                           value="${escapeHtml(url)}" required>
                </div>
                <div class="form-group">
                    <label for="config-webhook-secret">Signing Secret <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="HMAC-SHA256 key for verifying request authenticity via X-Tokay-Signature header. Leave empty to skip signing"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="password" id="config-webhook-secret" class="form-input" autocomplete="new-password"
                           placeholder="${isMasked ? 'Leave empty to keep existing' : 'your-signing-secret'}"
                           value="">
                    ${isMasked ? '<small class="form-hint">Current secret: ****. Leave empty to keep.</small>' : ''}
                </div>
                <div class="form-group">
                    <label for="config-webhook-timeout">Timeout (seconds) <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="Max seconds to wait for a response. Default: 30"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <input type="number" id="config-webhook-timeout" class="form-input"
                           min="1" max="60" placeholder="30"
                           value="${timeout ? escapeHtml(String(timeout)) : ''}">
                </div>
                <div class="form-group">
                    <label for="config-webhook-headers">Custom Headers <span class="optional">(optional)</span> <span class="tooltip-icon" data-tooltip="Extra HTTP headers sent with every request, e.g. Authorization"><i data-lucide="help-circle" style="width:14px;height:14px;color:var(--text-muted);"></i></span></label>
                    <textarea id="config-webhook-headers" class="form-input" rows="3"
                              placeholder="X-Custom-Header: value&#10;Authorization: Bearer token">${escapeHtml(headersText)}</textarea>
                    <small class="form-hint">One header per line, format: Header-Name: value</small>
                </div>
            `;
        }

        return '<p class="text-muted">Select an integration type to see configuration options.</p>';
    },

    /**
     * Render integration form modal content
     * @param {Object|null} integration - Integration data for editing, null for create
     */
    integrationFormModal: (integration = null) => {
        const isEdit = integration !== null;
        const isAdmin = window.Permissions && Permissions.isAdmin();
        const isTeamAdmin = !isAdmin && window.Permissions && Permissions.hasAnyTeamAdmin();

        // Types grouped by direction
        const typesByDirection = {
            outbound: isAdmin
                ? [{ value: 'slack', label: 'Slack' }, { value: 'telegram', label: 'Telegram' }, { value: 'generic_webhook', label: 'Generic Webhook' }]
                : [{ value: 'generic_webhook', label: 'Generic Webhook' }],
            inbound: [{ value: 'alertmanager_webhook', label: 'Alertmanager Webhook' }]
        };

        // Determine current direction based on type
        const currentDirection = integration?.direction || 'outbound';
        const defaultType = isTeamAdmin && !isEdit ? 'generic_webhook' : (typesByDirection[currentDirection][0]?.value || 'slack');
        const currentType = integration?.type || defaultType;
        const currentTypes = typesByDirection[currentDirection];

        // Parse config from RawMessage
        let config = null;
        if (integration?.config) {
            try {
                config = typeof integration.config === 'string'
                    ? JSON.parse(integration.config)
                    : integration.config;
            } catch (e) {
                console.warn('Failed to parse integration config:', e);
            }
        }

        // Direction tabs HTML
        let directionTabsHtml;
        if (isEdit) {
            directionTabsHtml = `
                <label>Direction</label>
                <div class="scope-tabs-sm">
                    <button type="button" class="scope-tab-sm ${currentDirection === 'outbound' ? 'active' : ''}" disabled>Outbound</button>
                    <button type="button" class="scope-tab-sm ${currentDirection === 'inbound' ? 'active' : ''}" disabled>Inbound</button>
                </div>
                <small class="form-hint">Direction cannot be changed after creation</small>
                <input type="hidden" id="integration-direction" value="${currentDirection}">
            `;
        } else if (isTeamAdmin) {
            // team_admin: direction locked to outbound, no tabs
            directionTabsHtml = `
                <input type="hidden" id="integration-direction" value="outbound">
            `;
        } else {
            directionTabsHtml = `
                <label>Direction *</label>
                <div class="scope-tabs-sm" id="direction-tabs">
                    <button type="button" class="scope-tab-sm active" data-direction="outbound">Outbound</button>
                    <button type="button" class="scope-tab-sm" data-direction="inbound">Inbound</button>
                </div>
                <input type="hidden" id="integration-direction" value="outbound">
            `;
        }

        // Scope/Team section (visible when type=generic_webhook)
        const showScope = currentType === 'generic_webhook';
        const currentScope = integration?.team_id ? 'team' : 'global';
        const adminTeamIds = window.Permissions ? Permissions.getAdminTeamIds() : [];
        const allTeams = State.teams || [];

        let scopeHtml = '';
        const scopeDisplay = showScope ? '' : ' style="display: none;"';

        if (isEdit && showScope) {
            // Edit mode: scope + team always disabled
            const teamName = integration?.team_id
                ? (allTeams.find(t => t.id === integration.team_id)?.name || integration.team_id)
                : '';
            scopeHtml = `
                <div class="form-row" id="scope-section">
                    <div class="form-group">
                        <label>Scope</label>
                        <div class="scope-tabs-sm">
                            <button type="button" class="scope-tab-sm ${currentScope === 'global' ? 'active' : ''}" disabled>Global</button>
                            <button type="button" class="scope-tab-sm ${currentScope === 'team' ? 'active' : ''}" disabled>Team</button>
                        </div>
                        <input type="hidden" id="integration-scope" value="${currentScope}">
                        <small class="form-hint">Scope cannot be changed after creation</small>
                    </div>
                    ${currentScope === 'team' ? `
                    <div class="form-group">
                        <label>Team</label>
                        <input type="text" class="form-input" value="${escapeHtml(teamName)}" disabled>
                        <input type="hidden" id="integration-team-id" value="${escapeHtml(integration?.team_id || '')}">
                    </div>
                    ` : '<input type="hidden" id="integration-team-id" value="">'}
                </div>
            `;
        } else if (!isEdit && isTeamAdmin) {
            // team_admin create: scope locked to "team", dropdown shows only admin teams
            const teamOptions = adminTeamIds.map(tid => {
                const team = allTeams.find(t => t.id === tid);
                const name = team ? team.name : tid;
                return `<option value="${escapeHtml(tid)}">${escapeHtml(name)}</option>`;
            }).join('');
            const singleTeam = adminTeamIds.length === 1;
            scopeHtml = `
                <div class="form-row" id="scope-section"${scopeDisplay}>
                    <div class="form-group">
                        <label>Scope</label>
                        <div class="scope-tabs-sm">
                            <button type="button" class="scope-tab-sm" disabled>Global</button>
                            <button type="button" class="scope-tab-sm active" disabled>Team</button>
                        </div>
                        <input type="hidden" id="integration-scope" value="team">
                    </div>
                    <div class="form-group">
                        <label for="integration-team-id">Team *</label>
                        <select id="integration-team-id" class="form-select" ${singleTeam ? 'disabled' : ''}>
                            ${singleTeam ? '' : '<option value="">Select team...</option>'}
                            ${teamOptions}
                        </select>
                    </div>
                </div>
            `;
        } else if (!isEdit && isAdmin) {
            // Admin create: scope tabs interactive, team dropdown shows all teams
            const teamOptions = allTeams.map(t =>
                `<option value="${escapeHtml(t.id)}">${escapeHtml(t.name || t.id)}</option>`
            ).join('');
            scopeHtml = `
                <div class="form-row" id="scope-section"${scopeDisplay}>
                    <div class="form-group">
                        <label>Scope *</label>
                        <div class="scope-tabs-sm" id="scope-tabs">
                            <button type="button" class="scope-tab-sm active" data-scope="global">Global</button>
                            <button type="button" class="scope-tab-sm" data-scope="team">Team</button>
                        </div>
                        <input type="hidden" id="integration-scope" value="global">
                    </div>
                    <div class="form-group" id="team-select-group" style="display: none;">
                        <label for="integration-team-id">Team *</label>
                        <select id="integration-team-id" class="form-select">
                            <option value="">Select team...</option>
                            ${teamOptions}
                        </select>
                    </div>
                </div>
            `;
        } else {
            // Fallback: hidden empty inputs (edit of non-webhook types)
            scopeHtml = '<div class="form-row" id="scope-section" style="display: none;"><input type="hidden" id="integration-scope" value=""><input type="hidden" id="integration-team-id" value=""></div>';
        }

        return `
            <form id="integration-form" autocomplete="off">
                <div class="form-section">
                    <div class="form-row" style="margin-bottom: var(--space-sm);">
                        <div class="form-group">
                            ${directionTabsHtml}
                        </div>
                        <div class="form-group">
                            <label>Status</label>
                            <div class="scope-tabs-sm" id="enabled-tabs">
                                <button type="button" class="scope-tab-sm ${integration?.enabled !== false ? 'active' : ''}" data-enabled="true">Enabled</button>
                                <button type="button" class="scope-tab-sm ${integration?.enabled === false ? 'active' : ''}" data-enabled="false">Disabled</button>
                            </div>
                            <input type="hidden" id="integration-enabled" value="${integration?.enabled !== false ? 'true' : 'false'}">
                        </div>
                    </div>
                    <div class="form-row">
                        <div class="form-group">
                            <label for="integration-type">Type *</label>
                            <select id="integration-type" class="form-select" ${isEdit || isTeamAdmin ? 'disabled' : ''}>
                                ${currentTypes.map(t => `<option value="${t.value}" ${currentType === t.value ? 'selected' : ''}>${t.label}</option>`).join('')}
                            </select>
                            ${isEdit ? '<small class="form-hint">Type cannot be changed after creation</small>' : ''}
                        </div>
                        <div class="form-group">
                            <label for="integration-name">Name *</label>
                            <input type="text" id="integration-name" class="form-input"
                                   placeholder="${currentType === 'slack' ? 'e.g., Production Slack' : currentType === 'telegram' ? 'e.g., Production Telegram' : currentType === 'generic_webhook' ? 'e.g., Prod Webhook' : currentType === 'alertmanager_webhook' ? 'e.g., Prod Alertmanager' : 'e.g., My Integration'}"
                                   value="${escapeHtml(integration?.name || '')}" required>
                        </div>
                    </div>
                    ${scopeHtml}
                </div>

                <div class="form-section">
                    <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: var(--space-md);">
                        <h4 class="form-section-title" style="margin-bottom: 0;">
                            <i data-lucide="settings"></i>
                            Configuration
                        </h4>
                        <button type="button" class="btn btn-sm btn-secondary" id="get-manifest-btn"
                                style="display: ${currentType === 'slack' ? 'inline-flex' : 'none'};">
                            <i data-lucide="file-code" style="width:14px;height:14px;"></i>
                            <span>App Manifest</span>
                        </button>
                    </div>
                    <div id="manifest-content" style="display: none; margin-bottom: var(--space-md);">
                        <p style="margin-bottom: var(--space-sm); color: var(--text-secondary); font-size: 0.8125rem;">
                            Copy this manifest &rarr;
                            <a href="https://api.slack.com/apps?new_app=1&manifest_yaml=" target="_blank" rel="noopener" style="color: var(--accent-primary);">api.slack.com/apps</a>
                            &rarr; <strong>Create New App</strong> &rarr; <strong>From a manifest</strong>
                        </p>
                        <div style="position: relative;">
                            <textarea id="manifest-yaml" class="form-input" readonly
                                      style="font-family: var(--font-mono, monospace); font-size: 0.8125rem; min-height: 240px; resize: vertical; white-space: pre; overflow-x: auto;"></textarea>
                            <button type="button" class="btn btn-sm btn-secondary" id="copy-manifest-btn"
                                    style="position: absolute; top: 8px; right: 8px;">
                                <i data-lucide="copy" style="width:14px;height:14px;margin-right:4px;"></i> Copy
                            </button>
                        </div>
                    </div>
                    <div id="integration-config-fields">
                        ${Components.integrationConfigFields(currentType, config)}
                    </div>
                </div>
            </form>
        `;
    },

    /**
     * Render delivery list panel
     * @param {Array} deliveries - Delivery objects
     * @param {Object} pagination - { page, total_pages, total }
     * @param {string} integrationId - Integration ID
     */
    deliveryListPanel: (deliveries, pagination, integrationId) => {
        if (!deliveries || deliveries.length === 0) {
            return `
                <div class="empty-state" style="padding: var(--space-xl) 0;">
                    <i data-lucide="inbox" class="empty-icon"></i>
                    <p>No deliveries yet</p>
                    <p class="text-sm text-muted">Deliveries will appear here when webhook events are sent</p>
                </div>
            `;
        }

        const statusBadge = (status) => `<span class="delivery-status delivery-status-${escapeHtml(status)}">${escapeHtml(status)}</span>`;
        const isTerminal = (s) => s === 'sent' || s === 'failed';

        const rows = deliveries.map(d => {
            const created = d.created_at ? new Date(d.created_at).toLocaleString() : '—';
            return `
                <tr data-delivery-id="${escapeHtml(d.id)}">
                    <td>${statusBadge(d.status)}</td>
                    <td>${d.last_http_status || '—'}</td>
                    <td>${d.attempts || 0}</td>
                    <td>${created}</td>
                    <td>
                        ${isTerminal(d.status) ? `<button class="btn btn-sm btn-secondary replay-btn" data-delivery-id="${escapeHtml(d.id)}" data-integration-id="${escapeHtml(integrationId)}" title="Replay" style="padding: 4px 8px;">
                            <i data-lucide="rotate-ccw" style="width: 12px; height: 12px;"></i>
                        </button>` : ''}
                    </td>
                </tr>
            `;
        }).join('');

        const page = pagination?.page || 1;
        const totalPages = pagination?.total_pages || 1;
        const total = pagination?.total || deliveries.length;

        return `
            <table class="delivery-table">
                <thead>
                    <tr>
                        <th>Status</th>
                        <th>HTTP</th>
                        <th>Attempts</th>
                        <th>Created</th>
                        <th></th>
                    </tr>
                </thead>
                <tbody>${rows}</tbody>
            </table>
            <div style="display: flex; justify-content: space-between; align-items: center; padding: var(--space-md) 0; font-size: 0.8rem; color: var(--text-muted);">
                <span>${total} deliveries</span>
                <div style="display: flex; gap: var(--space-sm); align-items: center;">
                    <button class="btn btn-sm btn-secondary delivery-prev-btn" ${page <= 1 ? 'disabled' : ''}>Prev</button>
                    <span>Page ${page} / ${totalPages}</span>
                    <button class="btn btn-sm btn-secondary delivery-next-btn" ${page >= totalPages ? 'disabled' : ''}>Next</button>
                </div>
            </div>
        `;
    },

    /**
     * Render delivery detail panel
     * @param {Object} delivery - Delivery object
     * @param {Array} attempts - DeliveryAttempt objects
     * @param {string} integrationId - Integration ID
     */
    deliveryDetailPanel: (delivery, attempts, integrationId) => {
        const statusBadge = (status) => `<span class="delivery-status delivery-status-${escapeHtml(status)}">${escapeHtml(status)}</span>`;
        const isTerminal = delivery.status === 'sent' || delivery.status === 'failed';

        const created = delivery.created_at ? new Date(delivery.created_at).toLocaleString() : '—';
        const sentAt = delivery.sent_at ? new Date(delivery.sent_at).toLocaleString() : '—';
        const nextAttempt = delivery.next_attempt_at ? new Date(delivery.next_attempt_at).toLocaleString() : '—';

        // Request payload
        let payloadHtml = '<p class="text-muted">No payload available</p>';
        if (delivery.request_payload) {
            try {
                const parsed = typeof delivery.request_payload === 'string' ? JSON.parse(delivery.request_payload) : delivery.request_payload;
                payloadHtml = `<pre class="delivery-payload">${escapeHtml(JSON.stringify(parsed, null, 2))}</pre>`;
            } catch {
                payloadHtml = `<pre class="delivery-payload">${escapeHtml(String(delivery.request_payload))}</pre>`;
            }
        }

        // Response body
        const responseBody = delivery.response_body_trunc
            ? `<pre class="delivery-payload">${escapeHtml(delivery.response_body_trunc)}</pre>`
            : '<p class="text-muted">No response body</p>';

        // Attempts history
        let attemptsHtml = '';
        if (attempts && attempts.length > 0) {
            const rows = attempts.map(a => {
                const ts = a.created_at ? new Date(a.created_at).toLocaleString() : '—';
                return `
                    <tr>
                        <td>${a.http_status || '—'}</td>
                        <td>${a.error ? escapeHtml(a.error) : '—'}</td>
                        <td style="max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${a.response_body_trunc ? escapeHtml(a.response_body_trunc) : '—'}</td>
                        <td>${ts}</td>
                    </tr>
                `;
            }).join('');
            attemptsHtml = `
                <div style="margin-bottom: var(--space-lg);">
                    <h4 class="form-section-title"><i data-lucide="list"></i> Attempt History</h4>
                    <table class="delivery-table">
                        <thead>
                            <tr><th>HTTP</th><th>Error</th><th>Response</th><th>Time</th></tr>
                        </thead>
                        <tbody>${rows}</tbody>
                    </table>
                </div>
            `;
        }

        return `
            <dl class="delivery-detail-meta">
                <div>
                    <dt>Status</dt>
                    <dd>${statusBadge(delivery.status)}</dd>
                </div>
                <div>
                    <dt>HTTP Status</dt>
                    <dd>${delivery.last_http_status || '—'}</dd>
                </div>
                <div>
                    <dt>Attempts</dt>
                    <dd>${delivery.attempts || 0}</dd>
                </div>
                <div>
                    <dt>Created</dt>
                    <dd>${created}</dd>
                </div>
                <div>
                    <dt>Sent At</dt>
                    <dd>${sentAt}</dd>
                </div>
                ${delivery.status === 'retry' ? `<div><dt>Next Attempt</dt><dd>${nextAttempt}</dd></div>` : ''}
            </dl>

            ${delivery.last_error ? `
                <div style="margin-bottom: var(--space-lg);">
                    <h4 class="form-section-title"><i data-lucide="alert-triangle"></i> Last Error</h4>
                    <pre class="delivery-payload">${escapeHtml(delivery.last_error)}</pre>
                </div>
            ` : ''}

            ${attemptsHtml}

            <div style="margin-bottom: var(--space-lg);">
                <h4 class="form-section-title"><i data-lucide="code"></i> Request Payload</h4>
                ${payloadHtml}
            </div>

            <div style="margin-bottom: var(--space-lg);">
                <h4 class="form-section-title"><i data-lucide="file-text"></i> Response Body</h4>
                ${responseBody}
            </div>
        `;
    },
};

/**
 * Escape HTML to prevent XSS
 * @param {string} str - String to escape
 */
function escapeHtml(str) {
    if (!str) return '';
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

/**
 * Escape string for safe use in HTML attribute values (double-quoted).
 * Escapes &, <, >, " and ' in addition to what escapeHtml does.
 */
function escapeAttr(str) {
    if (!str) return '';
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;');
}

// Export
window.Components = Components;
window.escapeHtml = escapeHtml;
window.escapeAttr = escapeAttr;
