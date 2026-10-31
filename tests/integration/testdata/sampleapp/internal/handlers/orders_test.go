package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateOrderRequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	CreateOrder(rec, httptest.NewRequest(http.MethodPost, "/orders", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
