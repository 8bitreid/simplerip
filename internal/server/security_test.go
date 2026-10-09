package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/8bitreid/simplerip/internal/config"
)

func TestHostAllowList(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.AllowedHosts = []string{"Rip.Example.com:8080"}
	cfg.Notification.UIURL = "https://ui.example.org/"
	t.Setenv("SIMPLERIP_HOST", "ripper.example.net")
	hosts := newHostAllowList(cfg)

	tests := []struct {
		host string
		want bool
	}{
		{"192.168.1.20:8080", true},
		{"100.101.102.103", true},
		{"[::1]:8080", true},
		{"localhost:8080", true},
		{"ripper", true},
		{"ripper.tail1234.ts.net", true},
		{"ripper.local:8080", true},
		{"ripper.home.arpa", true},
		{"rip.example.com", true},
		{"ui.example.org", true},
		{"ripper.example.net:8080", true},
		{"evil.example", false},
		{"ts.net.evil.example", false},
		{"rebind.attacker.com:8080", false},
		{"bad;host", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := hosts.allows(tt.host); got != tt.want {
			t.Errorf("allows(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// newSecuredTestServer is a test server with the production security
// middleware installed.
func newSecuredTestServer() *Server {
	s := newTestServer(nil)
	s.useSecurityMiddleware()
	return s
}

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.e.ServeHTTP(rr, req)
	return rr
}

func TestSecurityMiddlewareRefusesUnknownHost(t *testing.T) {
	s := newSecuredTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "rebind.attacker.com:8080"
	if rr := serve(s, req); rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMisdirectedRequest)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "ripper.tail1234.ts.net"
	if rr := serve(s, req); rr.Code != http.StatusOK {
		t.Fatalf("allowed host status = %d, want 200", rr.Code)
	}
}

func TestSecurityMiddlewareRefusesCrossSitePost(t *testing.T) {
	s := newSecuredTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/cancel", strings.NewReader(`{"device":"/dev/sr0"}`))
	req.Host = "192.168.1.20:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	if rr := serve(s, req); rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestSecurityMiddlewareRefusesFormPost(t *testing.T) {
	s := newSecuredTestServer()
	// An old browser without Sec-Fetch-Site or Origin: only the JSON rule stops it.
	req := httptest.NewRequest(http.MethodPost, "/api/cancel", strings.NewReader("device=/dev/sr0"))
	req.Host = "192.168.1.20:8080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rr := serve(s, req); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnsupportedMediaType)
	}
}

func TestSecurityMiddlewareAllowsSameOriginJSON(t *testing.T) {
	s := newSecuredTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/auto-eject", strings.NewReader(`{"device":"/dev/sr0","enabled":true}`))
	req.Host = "ripper.tail1234.ts.net"
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "https://ripper.tail1234.ts.net")
	rr := serve(s, req)
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnsupportedMediaType || rr.Code == http.StatusMisdirectedRequest {
		t.Fatalf("same-origin JSON request was refused: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSecurityMiddlewareLimitsBodySize(t *testing.T) {
	s := newSecuredTestServer()
	body := `{"device":"` + strings.Repeat("a", 128*1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/cancel", bytes.NewReader([]byte(body)))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	if rr := serve(s, req); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	s := newSecuredTestServer()
	for path, contentType := range map[string]string{
		"/":        "text/html",
		"/app.js":  "text/javascript",
		"/app.css": "text/css",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost:8080"
		rr := serve(s, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, rr.Code)
		}
		h := rr.Header()
		if got := h.Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, got, contentType)
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("GET %s CSP = %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s missing nosniff/frame headers: %v", path, h)
		}
	}
}

// The CSP forbids inline scripts, so the page must load its code from files.
func TestIndexHasNoInlineScript(t *testing.T) {
	page := string(indexHTML)
	if strings.Contains(page, "<script>") || strings.Contains(page, "<style>") {
		t.Fatal("index.html has an inline <script> or <style>; move it into app.js or app.css")
	}
	if !strings.Contains(page, `<script src="/app.js">`) || !strings.Contains(page, `href="/app.css"`) {
		t.Fatal("index.html does not load /app.js and /app.css")
	}
}
