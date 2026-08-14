package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// maxResolveUserIDs bounds one resolve request.
//
// The endpoint is reachable by any authenticated user, so an unbounded array
// would be an invitation to make the server materialize an arbitrary result
// set for a single call. The client chunks; 500 is comfortably more than a
// rendered calendar page names.
const maxResolveUserIDs = 500

// ResolveUsersRequest is a set of user IDs to put names to.
type ResolveUsersRequest struct {
	UserIDs []string `json:"user_ids"`
}

// ResolvedUserDTO is a display name and nothing else.
//
// Not model.User: this is the one read that also answers for erased people,
// and their row still carries whatever anonymization left behind. Returning
// the full user here would hand every authenticated caller the remains of a
// profile that was deliberately erased. Name is all a calendar needs.
type ResolvedUserDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ResolveUsersResponse omits the IDs it could not resolve rather than
// returning nulls for them: "not in this list" is one state for the client to
// render, and a placeholder row would have to be told apart from a real one.
type ResolveUsersResponse struct {
	Users []ResolvedUserDTO `json:"users"`
}

// ResolveUsers godoc
// @Summary Resolve user IDs to display names
// @Description Display read for historical data. Includes erased users, whose row survives so history that names their ID stays legible. Returns only id and name.
// @Tags users
// @Accept json
// @Produce json
// @Param request body ResolveUsersRequest true "User IDs"
// @Success 200 {object} ResolveUsersResponse
// @Failure 400 {object} ErrorResponse
// @Router /api/v1/users/resolve [post]
func (a *API) ResolveUsers(c echo.Context) error {
	var req ResolveUsersRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, CodeInvalidRequestBody, "invalid request body")
	}

	// Deduplicated before the limit is applied: a calendar naturally repeats
	// the same person across shifts, and counting the repeats against the
	// budget would reject a page that asks about a handful of people.
	seen := make(map[string]struct{}, len(req.UserIDs))
	ids := make([]string, 0, len(req.UserIDs))
	for _, id := range req.UserIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > maxResolveUserIDs {
		return badRequest(c, CodeValidationFailed, "too many user_ids")
	}

	out := ResolveUsersResponse{Users: []ResolvedUserDTO{}}
	if len(ids) == 0 {
		return c.JSON(http.StatusOK, out)
	}

	// The DISPLAY read: it does not filter deleted_at, which is the whole
	// point. GetUser is the active read and answers 404 for an erased person,
	// so a client resolving history through it could not tell an erased user
	// from a broken reference.
	users, err := a.store.GetUsersByIDs(ids)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error(), Code: CodeInternal})
	}
	for _, u := range users {
		out.Users = append(out.Users, ResolvedUserDTO{ID: u.ID, Name: u.Name})
	}
	return c.JSON(http.StatusOK, out)
}
