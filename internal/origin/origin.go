// Package origin decides whether a browser request is same-origin with Talker.
package origin

import (
	"net/http"
	"net/url"
	"strings"
)

// Same reports whether the request's Origin matches the host the browser
// addressed. Behind a reverse proxy that rewrites Host, X-Forwarded-Host carries
// the browser-visible host and is accepted as well.
func Same(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	for _, forwarded := range strings.Split(r.Header.Get("X-Forwarded-Host"), ",") {
		if forwarded = strings.TrimSpace(forwarded); forwarded != "" && forwarded == u.Host {
			return true
		}
	}
	return false
}
