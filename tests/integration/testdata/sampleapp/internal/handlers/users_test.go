package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetUserRequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	GetUser(rec, httptest.NewRequest(http.MethodGet, "/users/1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
