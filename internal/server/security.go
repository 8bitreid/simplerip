package server

import (
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/8bitreid/simplerip/internal/config"
)

// The web UI has no login: who can reach it is decided by the network
// (Tailscale, LAN). These protections stop a web page open in a trusted
// browser from driving or reading the API on that browser's behalf.

// requestBodyLimit caps request bodies. The UI only sends small JSON objects.
const requestBodyLimit = "64K"

// contentSecurityPolicy allows only the UI's own files. Inline styles stay
// allowed because the UI renders style attributes; inline scripts do not.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self'; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// privateHostSuffixes are names no public DNS can point at this server, so
// a DNS rebinding page cannot use them.
var privateHostSuffixes = []string{
	".ts.net", ".local", ".lan", ".home", ".internal", ".home.arpa", ".localdomain",
}

// useSecurityMiddleware installs the protections in front of every route.
func (s *Server) useSecurityMiddleware() {
	s.e.Use(
		requireAllowedHost(newHostAllowList(s.cfg)),
		securityHeaders,
		middleware.BodyLimit(requestBodyLimit),
		rejectCrossOrigin(http.NewCrossOriginProtection()),
		requireJSONBody,
	)
}

// hostAllowList decides which Host headers the server answers to. Refusing
// unknown names blocks DNS rebinding, where an attacker's domain is pointed
// at this server so their page counts as same-origin.
type hostAllowList struct {
	extra map[string]bool
}

func newHostAllowList(cfg *config.Config) hostAllowList {
	l := hostAllowList{extra: map[string]bool{}}
	add := func(host string) {
		if host = normalizeHost(host); host != "" {
			l.extra[host] = true
		}
	}
	for _, host := range cfg.Server.AllowedHosts {
		add(host)
	}
	add(os.Getenv("SIMPLERIP_HOST"))
	if u, err := url.Parse(cfg.Notification.UIURL); err == nil {
		add(u.Host)
	}
	return l
}

// allows reports whether host (a Host header, with or without port) is one
// this server answers to: an IP address, a single-label name, a private
// suffix, or a configured name.
func (l hostAllowList) allows(host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	if l.extra[host] || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range privateHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// normalizeHost lowercases host and strips any port, IPv6 brackets and
// trailing dot.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.Trim(host, "[]"), ".")
}

func requireAllowedHost(hosts hostAllowList) echo.MiddlewareFunc {
	var logged sync.Map // log each refused host once
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			host := c.Request().Host
			if hosts.allows(host) {
				return next(c)
			}
			if _, seen := logged.LoadOrStore(host, true); !seen {
				slog.Warn("refused request for unknown host; add it to server.allowed_hosts if it is yours", "host", host)
			}
			return c.JSON(http.StatusMisdirectedRequest, map[string]string{
				"error": "unknown host; add it to server.allowed_hosts or SIMPLERIP_ALLOWED_HOSTS",
			})
		}
	}
}

func securityHeaders(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		return next(c)
	}
}

// rejectCrossOrigin refuses state-changing requests that a browser marks as
// coming from another site (CSRF). Safe methods and non-browser clients pass.
func rejectCrossOrigin(cop *http.CrossOriginProtection) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := cop.Check(c.Request()); err != nil {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
			}
			return next(c)
		}
	}
}

// requireJSONBody refuses state-changing requests whose body is not JSON.
// HTML forms cannot send JSON, so a forged form post fails even in a browser
// without Sec-Fetch-Site. Requests without a body send no Content-Type.
func requireJSONBody(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return next(c)
		}
		contentType := req.Header.Get(echo.HeaderContentType)
		if contentType == "" {
			return next(c)
		}
		if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != echo.MIMEApplicationJSON {
			return c.JSON(http.StatusUnsupportedMediaType, map[string]string{"error": "request body must be JSON"})
		}
		return next(c)
	}
}
