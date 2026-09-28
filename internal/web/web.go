// Package web serves Talker's embedded browser console.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// FS contains the console assets in the assets directory.
//
//go:embed assets
var FS embed.FS

// Handler serves the console at / and its root-relative assets.
func Handler() http.Handler {
	assets, err := fs.Sub(FS, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html", "/styles.css", "/app.js", "/audio.js", "/mic-worklet.js", "/playback-worklet.js", "/live.js":
		case "/pwa.js", "/sw.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case "/manifest.webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
		case "/favicon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		case "/icon-192.png", "/icon-512.png", "/apple-touch-icon.png":
			w.Header().Set("Content-Type", "image/png")
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' wss://generativelanguage.googleapis.com; worker-src 'self'; manifest-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		files.ServeHTTP(w, r)
	})
}
