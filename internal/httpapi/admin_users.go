package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/adminusers"
	"github.com/yhwyxy/examSystem/internal/auth"
)

func mountAdminUsers(admin chi.Router, deps Dependencies) {
	admin.Get("/me", currentAdminHandler)
	userRouter := admin.With(deps.Auth.RequireRole(auth.RoleAdmin))
	userRouter.Get("/users", listAdminUsersHandler(deps))
	userRouter.Post("/users", createAdminUserHandler(deps))
	userRouter.Patch("/users/{username}", updateAdminUserHandler(deps))
}

func currentAdminHandler(w http.ResponseWriter, r *http.Request) {
	username, role := auth.UsernameFromContext(r.Context()), auth.RoleFromContext(r.Context())
	if username == "" {
		username, role = "local-admin", auth.RoleAdmin
	}
	WriteJSON(w, http.StatusOK, map[string]any{"username": username, "role": role})
}

func listAdminUsersHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AdminUsers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ADMIN_USERS_NOT_CONFIGURED", "admin user store unavailable")
			return
		}
		users, err := deps.AdminUsers.List(r.Context())
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "ADMIN_USERS_FAILED", "admin users are unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"users": users})
	}
}

func createAdminUserHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AdminUsers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ADMIN_USERS_NOT_CONFIGURED", "admin user store unavailable")
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "admin user request is invalid")
			return
		}
		actor := adminActor(r)
		user, err := deps.AdminUsers.Create(r.Context(), adminusers.CreateInput{Username: body.Username, Password: body.Password, Role: body.Role, Actor: actor})
		if err != nil {
			writeAdminUserError(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"user": user})
	}
}

func updateAdminUserHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AdminUsers == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ADMIN_USERS_NOT_CONFIGURED", "admin user store unavailable")
			return
		}
		var body struct {
			Password string `json:"password"`
			Role     string `json:"role"`
			Active   *bool  `json:"active"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "admin user request is invalid")
			return
		}
		username := strings.TrimSpace(chi.URLParam(r, "username"))
		user, err := deps.AdminUsers.Update(r.Context(), adminusers.UpdateInput{
			Username: username, Password: body.Password, Role: body.Role, Active: body.Active, Actor: adminActor(r),
		})
		if err != nil {
			writeAdminUserError(w, err)
			return
		}
		if deps.Auth != nil {
			deps.Auth.InvalidateUser(username)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"user": user, "reauthentication_required": true})
	}
}

func adminActor(r *http.Request) string {
	if actor := auth.UsernameFromContext(r.Context()); actor != "" {
		return actor
	}
	return "local-admin"
}

func writeAdminUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, adminusers.ErrInvalidUser):
		writeAdminError(w, http.StatusBadRequest, "INVALID_ADMIN_USER", "username, role, or password does not meet policy")
	case errors.Is(err, adminusers.ErrUserExists):
		writeAdminError(w, http.StatusConflict, "ADMIN_USER_EXISTS", "username already exists")
	case errors.Is(err, adminusers.ErrUserNotFound):
		writeAdminError(w, http.StatusNotFound, "ADMIN_USER_NOT_FOUND", "admin user not found")
	case errors.Is(err, adminusers.ErrLastAdmin), errors.Is(err, adminusers.ErrSelfDemotion):
		writeAdminError(w, http.StatusConflict, "LAST_ADMIN_REQUIRED", "at least one active admin account must remain")
	default:
		writeAdminError(w, http.StatusInternalServerError, "ADMIN_USERS_FAILED", "admin user change failed")
	}
}
