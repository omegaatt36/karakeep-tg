package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"ready", http.StatusOK, false},
		{"unavailable", http.StatusServiceUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			err := healthcheck(t.Context(), server.URL)

			if (err != nil) != tt.wantErr {
				t.Fatalf("healthcheck error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	if err := healthcheck(t.Context(), url); err == nil {
		t.Fatal("healthcheck succeeded against a closed server")
	}
}
