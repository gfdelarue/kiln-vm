package middleware

import (
	"net/http"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// TimeoutExceptStreaming applies a request timeout to normal API calls while
// preserving long-lived endpoints like streaming and profile preparation.
func TimeoutExceptStreaming(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		timeoutHandler := chimw.Timeout(timeout)(next)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isLongRunning(r) {
				next.ServeHTTP(w, r)
				return
			}
			timeoutHandler.ServeHTTP(w, r)
		})
	}
}

func isLongRunning(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case strings.Contains(r.Header.Get("Accept"), "text/event-stream"),
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket"),
		path == "/vms",
		path == "/vm-profile-images/ensure",
		path == "/vm-images" && r.Method == http.MethodPost,
		strings.HasPrefix(path, "/vm-profiles/") && strings.HasSuffix(path, "/prepare"):
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "vms" || parts[1] == "" {
		return false
	}
	switch {
	case len(parts) == 3 && parts[2] == "exec" && r.Method == http.MethodPost,
		len(parts) == 3 && parts[2] == "shell",
		len(parts) == 4 && parts[2] == "forward" && parts[3] != "":
		return true
	}
	return false
}
