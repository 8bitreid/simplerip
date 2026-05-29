package metadata

import (
	"net/http"
	"net/url"
	"testing"
)

type rewriteTransport struct {
	base       *url.URL
	targetHost string
	rt         http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	if t.targetHost == "" || req.URL.Host == t.targetHost {
		u.Scheme = t.base.Scheme
		u.Host = t.base.Host
		clone.URL = &u
		clone.Host = t.base.Host
	}
	return t.rt.RoundTrip(clone)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}
