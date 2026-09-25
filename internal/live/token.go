package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

const tokenEndpoint = "https://generativelanguage.googleapis.com/v1beta/auth_tokens"
const liveEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained"

type Broker struct {
	config       Config
	key          string
	declarations []*genai.FunctionDeclaration
	client       *http.Client
	endpoint     string
	mu           sync.Mutex
	lastRequest  time.Time
}

type Token struct {
	URL       string                 `json:"url"`
	Setup     *genai.LiveClientSetup `json:"setup"`
	Model     string                 `json:"model"`
	Voice     string                 `json:"voice"`
	QuietMS   int64                  `json:"quiet_ms"`
	ExpiresAt time.Time              `json:"expires_at"`
}

func NewBroker(config Config, key string, declarations []*genai.FunctionDeclaration) (*Broker, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(config.Model) {
		return nil, errors.New("model must be a Gemini model ID, without the models/ prefix")
	}
	return &Broker{config: config, key: strings.TrimSpace(key), declarations: declarations, endpoint: tokenEndpoint, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (b *Broker) Mint(ctx context.Context, handle string) (Token, error) {
	if b.key == "" {
		return Token{}, errors.New("set GEMINI_API_KEY or GEMINI_API_KEY_FILE on the Talker server")
	}
	if len(handle) > 8192 {
		return Token{}, errors.New("invalid session resumption handle")
	}
	config := setup(b.config, b.declarations)
	config.SessionResumption.Handle = handle
	expires := time.Now().UTC().Add(30 * time.Minute)
	body := struct {
		Uses                 int                    `json:"uses"`
		ExpireTime           time.Time              `json:"expireTime"`
		NewSessionExpireTime time.Time              `json:"newSessionExpireTime"`
		Setup                *genai.LiveClientSetup `json:"bidiGenerateContentSetup"`
	}{1, expires, time.Now().UTC().Add(time.Minute), config}
	data, err := json.Marshal(body)
	if err != nil {
		return Token{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(data))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", b.key)
	resp, err := b.client.Do(req)
	if err != nil {
		return Token{}, errors.New("could not reach Gemini token service")
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Token{}, errors.New("invalid Gemini token service response")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &failure)
		message := strings.ReplaceAll(failure.Error.Message, b.key, "[redacted]")
		if len(message) > 1000 {
			message = message[:1000]
		}
		return Token{}, fmt.Errorf("Gemini token service HTTP %d: %s", resp.StatusCode, message)
	}
	var token struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &token) != nil || !strings.HasPrefix(token.Name, "auth_tokens/") || len(token.Name) > 65536 {
		return Token{}, errors.New("Gemini returned an invalid ephemeral token")
	}
	return Token{URL: liveEndpoint + "?access_token=" + url.QueryEscape(token.Name), Setup: config, Model: b.config.Model, Voice: b.config.Voice, QuietMS: b.config.QuietPeriod.Milliseconds(), ExpiresAt: expires}, nil
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !checkOrigin(r) {
		http.Error(w, "same-origin browser request required", http.StatusForbidden)
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil && err != io.EOF {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON object", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	limited := time.Since(b.lastRequest) < time.Second
	if !limited {
		b.lastRequest = time.Now()
	}
	b.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "wait a moment before reconnecting", http.StatusTooManyRequests)
		return
	}
	token, err := b.Mint(r.Context(), body.Handle)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(token)
}
