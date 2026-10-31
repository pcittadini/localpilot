package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetUser returns the user named in the path.
func GetUser(w http.ResponseWriter, r *http.Request) {
	header := r.Header.Get("Authorization")
	if header == "" {
		http.Error(w, "missing authorization header", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(header, "Bearer ") {
		http.Error(w, "invalid authorization scheme", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if len(token) < 32 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/users/")
	json.NewEncoder(w).Encode(User{ID: id, Name: "Ada"})
}
