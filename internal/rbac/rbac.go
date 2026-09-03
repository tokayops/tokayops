// Package rbac provides Role-Based Access Control for TokayOps.
package rbac

import (
	"database/sql"

	"github.com/tokayops/tokayops/internal/model"
)

// Action represents a permission action in the RBAC system.
type Action string

// Action constants matching rbac-proposal.md
const (
	// Public internal (any authenticated user)
	ActionAlertView    Action = "alert.view"
	ActionAlertNoteAdd Action = "alert.note.add"
	ActionAlertCreate  Action = "alert.create"
	ActionTeamList     Action = "team.list"
	ActionTeamView     Action = "team.view"
	ActionScheduleView Action = "schedule.view"
	ActionUserList     Action = "user.list"
	ActionUserView     Action = "user.view"

	// Team member required
	ActionAlertAck       Action = "alert.ack"
	ActionAlertResolve   Action = "alert.resolve"
	ActionOverrideCreate Action = "schedule.override.create"
	ActionOverrideUpdate Action = "schedule.override.update"
	ActionOverrideDelete Action = "schedule.override.delete"

	// Team admin required
	ActionTeamUpdate       Action = "team.update"
	ActionTeamMemberAdd    Action = "team.member.add"
	ActionTeamMemberRemove Action = "team.member.remove"
	ActionScheduleEdit     Action = "schedule.edit"

	// Global admin only
	ActionTeamCreate         Action = "team.create"
	ActionTeamDelete         Action = "team.delete"
	ActionUserCreate         Action = "user.create"
	ActionUserUpdate         Action = "user.update"
	ActionUserDelete         Action = "user.delete"
	ActionUserRoleAssign     Action = "user.role.assign"
	ActionUserPasswordUpdate Action = "user.password.update"
	ActionIntegrationList    Action = "integration.list"
	ActionIntegrationView    Action = "integration.view"
	ActionIntegrationCreate  Action = "integration.create"
	ActionIntegrationUpdate  Action = "integration.update"
	ActionIntegrationDelete  Action = "integration.delete"
	// ActionDeliveryView is the delivery journal across every family and team:
	// the operational log under /deliveries and the journal of one commitment.
	// Global, because the journal is; a group's own deliveries go under
	// alert.view with the group's scope, like its timeline.
	ActionDeliveryView Action = "delivery.view"

	// Token actions (typically self-service)
	ActionTokenList   Action = "token.list"
	ActionTokenCreate Action = "token.create"
	ActionTokenDelete Action = "token.delete"

	// Policy actions
	ActionPolicyList   Action = "policy.list"
	ActionPolicyView   Action = "policy.view"
	ActionPolicyCreate Action = "policy.create"
	ActionPolicyUpdate Action = "policy.update"
	ActionPolicyDelete Action = "policy.delete"
)

// Scope represents the scope of a permission check.
type Scope struct {
	Type   string // "global", "team", "user"
	TeamID string // for team scope
	UserID string // for user scope
}

// GlobalScope returns a global scope.
func GlobalScope() Scope {
	return Scope{Type: "global"}
}

// TeamScope returns a team scope for the given team ID.
func TeamScope(teamID string) Scope {
	return Scope{Type: "team", TeamID: teamID}
}

// UserScope returns a user scope for the given user ID.
func UserScope(userID string) Scope {
	return Scope{Type: "user", UserID: userID}
}

// Checker provides RBAC permission checks.
type Checker struct {
	store directory
}

// NewChecker creates a new RBAC Checker.
// directory is the store as an access check needs it: who the user is, and what
// role they hold in a team. Two methods, and a check that needed a third would
// be a different kind of check.
type directory interface {
	// GetActiveUserByID excludes erased users, which is what makes a soft
	// delete terminal for authorization.
	GetActiveUserByID(id string) (*model.User, error)
	GetUserTeamRole(userID, teamID string) (model.TeamMemberRole, error)
}

func NewChecker(s directory) *Checker {
	return &Checker{store: s}
}

// publicInternalActions are allowed for any authenticated user.
var publicInternalActions = map[Action]bool{
	ActionAlertView:    true,
	ActionAlertNoteAdd: true,
	ActionAlertCreate:  true,
	ActionTeamList:     true,
	ActionTeamView:     true,
	ActionScheduleView: true,
	ActionUserList:     true,
	ActionUserView:     true,
	ActionPolicyList:   true,
}

// teamMemberActions require at least team_member role.
var teamMemberActions = map[Action]bool{
	ActionAlertAck:       true,
	ActionAlertResolve:   true,
	ActionOverrideCreate: true,
	ActionOverrideUpdate: true,
	ActionOverrideDelete: true,
	ActionPolicyView:     true, // Team member can view team policy
}

// teamAdminActions require team_admin role.
var teamAdminActions = map[Action]bool{
	ActionTeamUpdate:       true,
	ActionTeamMemberAdd:    true,
	ActionTeamMemberRemove: true,
	ActionScheduleEdit:     true,
	ActionPolicyCreate:     true, // Team admin can manage team policy
	ActionPolicyUpdate:     true,
	ActionPolicyDelete:     true,
	// Team admin can manage team-scoped webhook integrations
	ActionIntegrationList:   true,
	ActionIntegrationView:   true,
	ActionIntegrationCreate: true,
	ActionIntegrationUpdate: true,
	ActionIntegrationDelete: true,
}

// globalAdminActions require global admin role.
var globalAdminActions = map[Action]bool{
	ActionTeamCreate:     true,
	ActionTeamDelete:     true,
	ActionUserCreate:     true,
	ActionUserUpdate:     true,
	ActionUserDelete:     true,
	ActionUserRoleAssign: true,
	ActionDeliveryView:   true,
}

// HasPermission evaluates whether a user has permission to perform an action.
// Returns (allowed, error). If error is non-nil, permission should be denied.
func (c *Checker) HasPermission(userID string, action Action, scope Scope) (bool, error) {
	// The ACTIVE read: authorization must not grant anything to an erased
	// user. The session check already stops them a layer earlier; this is the
	// same rule stated where the decision is actually made.
	user, err := c.store.GetActiveUserByID(userID)
	if err != nil {
		return false, err
	}

	// Rule 1: Admin bypass - admin can do anything
	if user.Role == model.UserRoleAdmin {
		return true, nil
	}

	// Rule 2: Public internal actions - any authenticated user
	if publicInternalActions[action] {
		return true, nil
	}

	// Rule 3: Global admin actions - deny for non-admins
	if globalAdminActions[action] {
		return false, nil
	}

	// Rule 4: Team-scoped actions
	if scope.Type == "team" && scope.TeamID != "" {
		teamRole, err := c.store.GetUserTeamRole(userID, scope.TeamID)
		if err != nil {
			if err == sql.ErrNoRows {
				// Not a team member
				return false, nil
			}
			return false, err
		}

		// Check if action requires team_admin
		if teamAdminActions[action] {
			return teamRole == model.TeamMemberRoleAdmin, nil
		}

		// Check if action requires team_member (or higher)
		if teamMemberActions[action] {
			return teamRole == model.TeamMemberRoleAdmin || teamRole == model.TeamMemberRoleMember, nil
		}
	}

	// Rule 5: User-scoped actions (self-only)
	if scope.Type == "user" && scope.UserID != "" {
		return scope.UserID == userID, nil
	}

	// Rule 6: Policy Viewing
	// - Global policies (scope.Type="global"): allowed for everyone (public internal)
	// - Team policies (scope.Type="team"): handled by Rule 4 (Team Member check)
	if action == ActionPolicyView && scope.Type == "global" {
		return true, nil
	}

	// Default deny
	return false, nil
}

// IsAdmin returns true if the user is an ACTIVE global admin. An erased user
// is nobody's administrator.
func (c *Checker) IsAdmin(userID string) (bool, error) {
	user, err := c.store.GetActiveUserByID(userID)
	if err != nil {
		return false, err
	}
	return user.Role == model.UserRoleAdmin, nil
}

// IsTeamMember returns true if the user is a member of the given team.
func (c *Checker) IsTeamMember(userID, teamID string) (bool, error) {
	_, err := c.store.GetUserTeamRole(userID, teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsTeamAdmin returns true if the user is an admin of the given team.
func (c *Checker) IsTeamAdmin(userID, teamID string) (bool, error) {
	role, err := c.store.GetUserTeamRole(userID, teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return role == model.TeamMemberRoleAdmin, nil
}
