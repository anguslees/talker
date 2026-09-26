package web

import (
	"encoding/json"
	"image/color"
	"image/png"
	"mime"
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
	for _, path := range []string{"/api/live", "/assets/", "/web.go", "/package.json", "/audio.test.js", "/pwa.test.js", "/assets/sw.js", "/../web.go", "/missing.js"} {
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

func TestHandlerServesPWAAssets(t *testing.T) {
	for path, contentType := range map[string]string{
		"/manifest.webmanifest": "application/manifest+json",
		"/favicon.svg":          "image/svg+xml",
		"/icon-192.png":         "image/png",
		"/icon-512.png":         "image/png",
		"/apple-touch-icon.png": "image/png",
		"/pwa.js":               "text/javascript",
		"/sw.js":                "text/javascript",
	} {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := httptest.NewRecorder()
				Handler().ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("%s status = %d, want 200", method, response.Code)
				}
				got, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
				if err != nil || got != contentType {
					t.Errorf("content type = %q, %v; want %q", got, err, contentType)
				}
				if (response.Body.Len() > 0) != (method == http.MethodGet) {
					t.Errorf("%s body length = %d", method, response.Body.Len())
				}
				if response.Header().Get("Cache-Control") != "no-cache" {
					t.Error("assets must be revalidated")
				}
				if response.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Error("missing nosniff header")
				}
				policy := response.Header().Get("Content-Security-Policy")
				for _, directive := range []string{"script-src 'self';", "worker-src 'self';", "manifest-src 'self';"} {
					if !strings.Contains(policy, directive) {
						t.Errorf("missing CSP directive %q", directive)
					}
				}
			}
		})
	}
}

func TestManifest(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	var manifest struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "Talker" || manifest.ShortName != "Talker" {
		t.Errorf("incorrect app identity: %q / %q", manifest.Name, manifest.ShortName)
	}
	if manifest.ID != "/" || manifest.StartURL != "/" || manifest.Scope != "/" || manifest.Display != "standalone" {
		t.Errorf("incorrect standalone launch: %+v", manifest)
	}
	if manifest.ThemeColor != "#101415" || manifest.BackgroundColor != "#101415" {
		t.Error("manifest colors must match the console")
	}
	wantIcons := map[string]string{"/icon-192.png": "192x192", "/icon-512.png": "512x512"}
	for _, icon := range manifest.Icons {
		if wantIcons[icon.Src] != icon.Sizes || icon.Type != "image/png" || icon.Purpose != "any maskable" {
			t.Errorf("incorrect install icon: %+v", icon)
		}
		delete(wantIcons, icon.Src)
	}
	if len(wantIcons) != 0 {
		t.Errorf("missing install icons: %v", wantIcons)
	}
}

func TestPWAIcons(t *testing.T) {
	for path, size := range map[string]int{"/icon-192.png": 192, "/icon-512.png": 512, "/apple-touch-icon.png": 180} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			icon, err := png.Decode(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if icon.Bounds().Dx() != size || icon.Bounds().Dy() != size {
				t.Fatalf("icon bounds = %v, want %dx%d", icon.Bounds(), size, size)
			}
			background := color.RGBA{R: 0x10, G: 0x14, B: 0x15, A: 0xff}
			mint := color.RGBA{R: 0x92, G: 0xe4, B: 0xde, A: 0xff}
			foundMint := false
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					pixel := color.RGBAModel.Convert(icon.At(x, y)).(color.RGBA)
					if pixel.A != 0xff {
						t.Fatalf("icon must be opaque at (%d,%d)", x, y)
					}
					if pixel == mint {
						foundMint = true
					}
					// Maskable artwork must fit inside the central circle with radius 40% of the icon.
					dx, dy := float64(x)+0.5-float64(size)/2, float64(y)+0.5-float64(size)/2
					if dx*dx+dy*dy > 0.16*float64(size*size) && pixel != background {
						t.Fatalf("artwork outside maskable safe zone at (%d,%d)", x, y)
					}
				}
			}
			if !foundMint {
				t.Error("missing mint equalizer artwork")
			}
		})
	}
}
