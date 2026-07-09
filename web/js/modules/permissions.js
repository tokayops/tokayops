/**
 * Permissions Module
 * Handles client-side RBAC checks
 */

export const Permissions = {
    user: null,

    /**
     * Initialize with user data
     * @param {Object} user - User object from /api/auth/me
     */
    init(user) {
        this.user = user;
        console.log('Permissions initialized for:', user.role);
    },

    /**
     * Check if user is a global admin
     */
    isAdmin() {
        return this.user?.role === 'admin';
    },

    /**
     * Check if user is a member of the team
     * @param {string} teamId
     */
    isTeamMember(teamId) {
        if (!this.user) return false;
        if (this.isAdmin()) return true;
        return this.user.teams && this.user.teams[teamId] !== undefined;
    },

    /**
     * Check if user is an admin of the team
     * @param {string} teamId
     */
    isTeamAdmin(teamId) {
        if (!this.user) return false;
        if (this.isAdmin()) return true;
        return this.user.teams && this.user.teams[teamId] === 'team_admin';
    },

    /**
     * Check if user has team_admin role in ANY team
     */
    hasAnyTeamAdmin() {
        if (!this.user) return false;
        if (this.isAdmin()) return true;
        return Object.values(this.user.teams || {}).some(role => role === 'team_admin');
    },

    /**
     * Check if user can access Configure mode at all
     * Returns true for admin or any team_admin
     */
    canAccessConfigure() {
        return this.isAdmin() || this.hasAnyTeamAdmin();
    },

    /**
     * Get team IDs where the user has team_admin role.
     * Always returns an array (empty for admin — caller should check isAdmin() separately).
     */
    getAdminTeamIds() {
        if (!this.user) return [];
        return Object.entries(this.user.teams || {})
            .filter(([_, role]) => role === 'team_admin')
            .map(([teamId]) => teamId);
    },

    /**
     * Check if user can perform an action
     * @param {string} action - Action name (e.g., 'edit_team')
     * @param {Object} context - Context (e.g., { teamId: 'devops' })
     */
    can(action, context = {}) {
        if (!this.user) return false;
        if (this.isAdmin()) return true;

        switch (action) {
            case 'create_team':
                return this.isAdmin();

            case 'delete_team':
                return this.isAdmin();

            case 'edit_team':
            case 'manage_members':
                return this.isTeamAdmin(context.teamId);

            case 'view_team':
                // Public internal visibility? Or restricted?
                // Backend `GetTeamByID` requires `ActionTeamView` with `ScopeGlobal`.
                // But usually teams are visible to all auth users.
                return true;

            case 'ack_alert':
            case 'resolve_alert':
            case 'create_override':
            case 'delete_override':
                // Requires Team Member
                return this.isTeamMember(context.teamId);

            case 'manage_schedule':
                // Requires Schedule Edit = Team Admin usually
                // Backend: ActionScheduleEdit -> ScopeTeam(teamID)
                // Rule 6: ActionScheduleEdit allows TeamAdmin.
                return this.isTeamAdmin(context.teamId);

            case 'create_policy':
            case 'edit_policy':
            case 'delete_policy':
                // Team-scoped: requires team_admin; global: requires admin
                // context.teamId = null for global policies
                if (!context.teamId) return this.isAdmin();
                return this.isTeamAdmin(context.teamId);

            case 'manage_users':
                return this.isAdmin();

            default:
                return false;
        }
    }
};

// Make available globally for inline calls in HTML generation if needed
window.Permissions = Permissions;
