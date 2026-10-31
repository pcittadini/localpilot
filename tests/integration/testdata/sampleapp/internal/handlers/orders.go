package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

type Order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

// CreateOrder stores a new order.
func CreateOrder(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		http.Error(w, "missing authorization header", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "invalid authorization scheme", http.StatusUnauthorized)
		return
	}
	tok := strings.TrimPrefix(authHeader, "Bearer ")
	if len(tok) < 32 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(o)
}
