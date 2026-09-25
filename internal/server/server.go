package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"talker/internal/origin"
	"talker/internal/tasks"
)

type Config struct {
	Model      string
	Voice      string
	EventToken string
	Tasks      *tasks.Manager
	Web        http.Handler
	Control    http.Handler
	Token      http.Handler
}

func New(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", cfg.Web)
	mux.Handle("GET /api/control", cfg.Control)
	mux.Handle("POST /api/live/token", sameOrigin(cfg.Token))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"model": cfg.Model, "voice": cfg.Voice, "transport": "browser-direct"})
	})
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": cfg.Tasks.Recent(50)})
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"events": cfg.Tasks.Pending(30)})
	})
	mux.Handle("POST /api/events/ack", sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.IDs) > 100 {
			http.Error(w, "too many event IDs", http.StatusBadRequest)
			return
		}
		if err := cfg.Tasks.Ack(body.IDs); err != nil {
			http.Error(w, "could not persist acknowledgement", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"acknowledged": true})
	})))
	mux.HandleFunc("POST /api/events", func(w http.ResponseWriter, r *http.Request) {
		if cfg.EventToken == "" {
			http.Error(w, "external events disabled; set TALKER_EVENT_TOKEN", http.StatusServiceUnavailable)
			return
		}
		provided := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+cfg.EventToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if !decode(w, r, &body) {
			return
		}
		event, err := cfg.Tasks.AddEvent(body.Title, body.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, event)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !origin.Same(r) {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
