package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEmbeddedConsole(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/", "/styles.css", "/app.js", "/audio.js", "/mic-worklet.js", "/playback-worklet.js", "/live.js"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if response.Body.Len() == 0 {
				t.Fatal("embedded asset is empty")
			}
			if strings.HasSuffix(path, ".js") && !strings.Contains(response.Header().Get("Content-Type"), "javascript") {
				t.Errorf("JavaScript content type = %q", response.Header().Get("Content-Type"))
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing nosniff header")
			}
			if !strings.Contains(response.Header().Get("Content-Security-Policy"), "script-src 'self'") {
				t.Error("missing script policy")
			}
			if !strings.Contains(response.Header().Get("Content-Security-Policy"), "connect-src 'self' wss://generativelanguage.googleapis.com;") {
				t.Error("connections must be limited to this origin and Google Live")
			}
		})
	}
}

func TestHandlerDoesNotExposeUnrecognizedPaths(t *testing.T) {
	for _, path := range []string{"/api/live", "/assets/", "/web.go", "/package.json", "/audio.test.js", "/../web.go", "/missing.js"} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, response.Code)
		}
	}
}

func TestHandlerSupportsHead(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodHead, "/mic-worklet.js", nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("HEAD status = %d, body length = %d", response.Code, response.Body.Len())
	}
}
