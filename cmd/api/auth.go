package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/ai-club/sandbox-host/internal/httpjson"
)

type principalKey struct{}

func (a *api) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		principal, ok := a.config.authenticate(token)
		if !ok {
			httpjson.WriteError(w, 401, "unauthorized", "invalid bearer token")
			return
		}
		team := r.URL.Query().Get("teamId")
		if team != "" && team != principal.TeamID {
			httpjson.WriteError(w, 403, "team_forbidden", "token is not authorized for this team")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	}
}
func principalFrom(r *http.Request) *tokenConfig {
	return r.Context().Value(principalKey{}).(*tokenConfig)
}
