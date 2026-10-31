package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunningServerHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	if err := checkHealth(server.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
}
