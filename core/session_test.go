// Unit tests for signing and checking the session cookie (`session.go`)
package core

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Checks that the signature of a fixed input equals a fixed value, so the session cookie signing method doesn't change.
//
// NOTE: The session cookie stays in the browser for up to 182 days.
// If a code change alters the signing method, every signed-in device is signed out.
// So the result is pinned to a fixed value.
func TestSignKnownValue(t *testing.T) {
	cfg := &EnvConfig{DBRepoAccessToken: "github_pat_test_0123456789abcdefghij"}
	if got, want := signWithSessionKey(cfg, "1794450948"), "GI8QlXVs-e65y7Ohb371ozsXa-L7DyNEBat6iBF-WrM"; got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

// Tests the session cookie check.
// A freshly made cookie passes.
// A cookie forged with a later expiry, and a cookie from before the server token changed, are refused
func TestSessionCookieRoundTrip(t *testing.T) {
	cfg := &EnvConfig{DBRepoAccessToken: "token-a"}
	set := NewSessionCookie(cfg, time.Now())
	value := strings.SplitN(set, ";", 2)[0] // The `Set-Cookie` value without attributes: "session=<expiry>.<signature>"

	req := httptest.NewRequest("GET", "/api/login-session", nil)
	req.Header.Set("Cookie", value)
	if err := requireSession(req, cfg); err != nil {
		t.Fatalf("fresh cookie rejected: %v", err)
	}

	// Forged cookie: same signature, later expiry
	exp, sig, _ := strings.Cut(strings.TrimPrefix(value, "session="), ".")
	forged := httptest.NewRequest("GET", "/api/login-session", nil)
	forged.Header.Set("Cookie", "session=9"+exp+"."+sig)
	if requireSession(forged, cfg) == nil {
		t.Fatal("forged cookie accepted")
	}

	// The signing key comes from the token.
	// So when the server token changes (the allowed user, the token owner, may change too), existing cookies are invalid
	if requireSession(req, &EnvConfig{DBRepoAccessToken: "token-b"}) == nil {
		t.Fatal("cookie survived a change of the server token")
	}
}

// Tests that a session cookie past its lifetime (`SessionTTL`) is refused
func TestSessionCookieExpired(t *testing.T) {
	cfg := &EnvConfig{DBRepoAccessToken: "token-a"}
	set := NewSessionCookie(cfg, time.Now().Add(-SessionTTL-time.Minute))
	req := httptest.NewRequest("GET", "/api/login-session", nil)
	req.Header.Set("Cookie", strings.SplitN(set, ";", 2)[0])
	if requireSession(req, cfg) == nil {
		t.Fatal("expired cookie accepted")
	}
}
