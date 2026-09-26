package api

import (
	"net/http"

	"github.com/hustenreizjuengling/picache/internal/auth"
)

// registerUserRoutes registers the account management (docs/API.md): an
// admin's interactive session only, never API tokens, so a token can never
// create accounts or change roles.
func (s *Server) registerUserRoutes() {
	s.route("GET /api/v1/system/users", permSession, s.usersList)
	s.route("POST /api/v1/system/users", permSession, s.usersCreate)
	s.route("PUT /api/v1/system/users/{id}", permSession, s.usersUpdate)
	s.route("DELETE /api/v1/system/users/{id}", permSession, s.usersDelete, routeDestructive)
}

func (s *Server) usersList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Auth.Users(r.Context())
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) usersCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		Role            string `json:"role"`
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.d.Auth.CreateUser(r.Context(), principal(r), in.CurrentPassword, in.Username, in.Password, in.Role)
	if err != nil {
		return err
	}
	s.audit(r, "auth.user.create", u.Username, map[string]string{"role": u.Role})
	return created(w, u)
}

func (s *Server) usersUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Role            *string `json:"role"`
		Password        *string `json:"password"`
		DisableTOTP     *bool   `json:"disableTotp"`
		CurrentPassword string  `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	u, ch, err := s.d.Auth.UpdateUser(r.Context(), p, id,
		auth.UserUpdate{Role: in.Role, Password: in.Password, DisableTOTP: in.DisableTOTP != nil && *in.DisableTOTP},
		in.CurrentPassword)
	if err != nil {
		return err
	}
	s.audit(r, "auth.user.update", u.Username, ch)
	if id == p.UserID && ch.RoleChanged {
		// The caller's own sessions ended with the role change.
		s.clearSessionCookies(w, r)
	}
	return ok(w, u)
}

func (s *Server) usersDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	u, err := s.d.Auth.DeleteUser(r.Context(), p, id, in.CurrentPassword)
	if err != nil {
		return err
	}
	s.audit(r, "auth.user.delete", u.Username, map[string]string{"role": u.Role})
	if id == p.UserID {
		s.clearSessionCookies(w, r)
	}
	return noContent(w)
}
