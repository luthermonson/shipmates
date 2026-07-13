package fleetobserver

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed ui/index.html ui/app.js ui/style.css
var uiFiles embed.FS

// ProductHandler serves the read-only browser and the authenticated GET API.
// Static assets are fixed; every data read still crosses Service authentication.
func (s *Service) ProductHandler() http.Handler {
	api := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, basePath+"/") {
			api.ServeHTTP(w, r)
			return
		}
		secureHeaders(w)
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		var name, kind string
		switch r.URL.Path {
		case "/", "/observer":
			name = "ui/index.html"
			kind = "text/html; charset=utf-8"
		case "/observer/app.js":
			name = "ui/app.js"
			kind = "application/javascript"
		case "/observer/style.css":
			name = "ui/style.css"
			kind = "text/css"
		default:
			writeError(w, 404, "not_found")
			return
		}
		b, err := uiFiles.ReadFile(name)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", kind)
		w.WriteHeader(200)
		_, _ = w.Write(b)
	})
}
