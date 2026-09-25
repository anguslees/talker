package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"talker/internal/hermes"
	"talker/internal/tasks"
)

func TestLocalAPIProtectionAndEvents(t *testing.T) {
	client, _ := hermes.New("", "")
	manager, err := tasks.Open(context.Background(), client, filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	h := New(Config{Model: "test-model", Voice: "Aoede", EventToken: "test-event-token", Tasks: manager, Web: stub, Control: stub, Token: stub})
	for _, test := range []struct {
		method, path, host, origin, forwardedHost, token, body string
		status                                                 int
	}{
		{"GET", "/api/config", "localhost:8080", "", "", "", "", 200},
		// Reads are reachable through any host, e.g. an authenticating reverse proxy.
		{"GET", "/api/config", "talker.proxy.example", "", "", "", "", 200},
		{"POST", "/api/live/token", "localhost:8080", "https://evil.example", "", "", "{}", 403},
		{"POST", "/api/live/token", "localhost:8080", "http://localhost:8080", "", "", "{}", 200},
		// Proxy rewrote Host to the upstream; the browser-visible host arrives in X-Forwarded-Host.
		{"POST", "/api/live/token", "127.0.0.1:8080", "https://talker.proxy.example", "talker.proxy.example", "", "{}", 200},
		{"POST", "/api/live/token", "127.0.0.1:8080", "https://evil.example", "talker.proxy.example", "", "{}", 403},
		{"POST", "/api/events", "localhost:8080", "", "", "", "{}", 401},
		{"POST", "/api/events", "localhost:8080", "", "", "test-event-token", `{"title":"Build finished","body":"All tests passed"}`, 202},
		{"POST", "/api/events/ack", "localhost:8080", "", "", "", `{"ids":[]}`, 403},
		{"POST", "/api/events/ack", "localhost:8080", "http://localhost:8080", "", "", `{"ids":[]}`, 200},
	} {
		r := httptest.NewRequest(test.method, "http://"+test.host+test.path, strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
		if test.forwardedHost != "" {
			r.Header.Set("X-Forwarded-Host", test.forwardedHost)
		}
		if test.token != "" {
			r.Header.Set("Authorization", "Bearer "+test.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s host=%s origin=%s got%d want%d: %s", test.method, test.path, test.host, test.origin, w.Code, test.status, w.Body.String())
		}
		if test.status == 200 && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("API response must not be cached")
		}
	}
	if len(manager.Events()) != 1 {
		t.Fatal("expected one persisted background event")
	}
}
