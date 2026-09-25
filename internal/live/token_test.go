package live

import (
	"context"
	"encoding/json"
	"google.golang.org/genai"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{Model: "gemini-3.8-live", Voice: "Aoede", Temperature: 1.1, QuietPeriod: 2 * time.Second}
}

func TestEphemeralTokenIsConstrainedAndResumable(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("x-goog-api-key") != "test-server-key" {
			t.Error("wrong token request authentication")
		}
		if r.URL.RawQuery != "" {
			t.Error("key must not be in query")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{"name":"auth_tokens/test-temporary-token"}`)
	}))
	defer upstream.Close()
	b, _ := NewBroker(testConfig(), "test-server-key", nil)
	b.endpoint = upstream.URL
	token, err := b.Mint(context.Background(), "opaque-resume-handle")
	if err != nil {
		t.Fatal(err)
	}
	if received["uses"] != float64(1) {
		t.Fatal("token must be single use")
	}
	constraint := received["bidiGenerateContentSetup"].(map[string]any)
	if constraint["model"] != "models/gemini-3.8-live" {
		t.Fatal("wrong model constraint")
	}
	if constraint["sessionResumption"].(map[string]any)["handle"] != "opaque-resume-handle" {
		t.Fatal("resumption must be bound into the token")
	}
	if _, ok := received["fieldMask"]; ok {
		t.Fatal("entire setup must be constrained")
	}
	u, err := url.Parse(token.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "generativelanguage.googleapis.com" || u.Query().Get("access_token") != "auth_tokens/test-temporary-token" {
		t.Fatal("invalid constrained socket URL")
	}
	data, _ := json.Marshal(token)
	if strings.Contains(string(data), "test-server-key") {
		t.Fatal("permanent API key leaked to browser")
	}
	if token.Setup.SessionResumption.Handle != "opaque-resume-handle" || token.QuietMS != 2000 {
		t.Fatal("client setup differs from constrained setup")
	}
}

func TestBrokerRedactsAPIKeyInErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"invalid secret-key"}}`)
	}))
	defer upstream.Close()
	b, _ := NewBroker(testConfig(), "secret-key", nil)
	b.endpoint = upstream.URL
	_, err := b.Mint(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestBrokerRejectsCrossOriginAndSetupOverrides(t *testing.T) {
	b, _ := NewBroker(testConfig(), "", nil)
	for _, test := range []struct {
		origin, body string
		status       int
	}{
		{"https://evil.example", `{}`, 403},
		{"", `{}`, 403},
		{"http://localhost:8080", `{"model":"other-model"}`, 400},
		{"http://localhost:8080", `{} {}`, 400},
	} {
		r := httptest.NewRequest("POST", "http://localhost:8080/api/live/token", strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("body=%s origin=%s got%d want%d", test.body, test.origin, w.Code, test.status)
		}
	}
}

func TestLanguageIsPinnedInSetupAndPrompt(t *testing.T) {
	cfg := testConfig()
	cfg.Language = "en-US"
	s := setup(cfg, nil)
	if s.GenerationConfig.SpeechConfig.LanguageCode != "en-US" {
		t.Fatalf("languageCode not set: %+v", s.GenerationConfig.SpeechConfig)
	}
	if !strings.Contains(s.SystemInstruction.Parts[0].Text, "Always speak English") {
		t.Fatal("system prompt must reinforce the pinned language by name")
	}
	cfg.Language = ""
	s = setup(cfg, nil)
	if s.GenerationConfig.SpeechConfig.LanguageCode != "" || strings.Contains(s.SystemInstruction.Parts[0].Text, "LANGUAGE\n") {
		t.Fatal("empty language must leave auto-detection untouched")
	}
	for code, want := range map[string]string{"en-GB": "English", "de": "German", "pt-BR": "Portuguese", "xx-YY": "xx-YY"} {
		if got := languageName(code); got != want {
			t.Fatalf("languageName(%q)=%q want %q", code, got, want)
		}
	}
}

// Start-of-speech detection defaults to the conservative setting so keyboard
// clacks and clicks are not committed as the user speaking (and do not barge in).
func TestSpeechDetectionDefaultsAreConservative(t *testing.T) {
	cfg := testConfig()
	cfg.SpeechStart, cfg.SpeechPrefix = "low", 200*time.Millisecond
	vad := setup(cfg, nil).RealtimeInputConfig.AutomaticActivityDetection
	if vad.StartOfSpeechSensitivity != genai.StartSensitivityLow || *vad.PrefixPaddingMs != 200 {
		t.Fatalf("start detection %+v", vad)
	}
	if vad.EndOfSpeechSensitivity != genai.EndSensitivityLow || *vad.SilenceDurationMs != 500 {
		t.Fatal("end-of-speech detection must stay permissive so pauses do not cut the user off")
	}
	cfg.SpeechStart, cfg.SpeechPrefix = "HIGH", 80*time.Millisecond
	vad = setup(cfg, nil).RealtimeInputConfig.AutomaticActivityDetection
	if vad.StartOfSpeechSensitivity != genai.StartSensitivityHigh || *vad.PrefixPaddingMs != 80 {
		t.Fatalf("high sensitivity %+v", vad)
	}
	if startSensitivity("") != genai.StartSensitivityLow {
		t.Fatal("unset must fall back to low, never to the API's unspecified default")
	}
}
