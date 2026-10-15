// Secret keys made from the server token (`deriveKey`), and signing and checking of the sign-in session cookie.
//
// NOTE: Don't change the format in this file.
// A session cookie stays in the browser for about 6 months.
// If the signing method changes, every device is signed out (`core/session_test.go` guards this with fixed values).
// The sign-in method can change (GitHub sign-in), so it lives in `github_login.go`.
package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Name, lifetime and attributes of the session cookie.
// The cookie value is `<expiry time (seconds)>.<signature>`.
// The server checks only the signature and the expiry, so it needs no place to store sessions.
const (
	sessionCookie = "session"
	SessionTTL    = 182 * 24 * time.Hour                           // About 6 months. Within the browser's cookie lifetime limit (Chrome 400 days, docs)
	cookieAttrs   = "Path=/api; HttpOnly; Secure; SameSite=Strict" // Scripts can't read it, and requests started from other sites don't carry it
)

// `Set-Cookie` value that deletes the session cookie (on sign-out, or when the session isn't valid)
const ClearSessionCookie = sessionCookie + "=; Max-Age=0; " + cookieAttrs

// Makes a secret key for one purpose from the server token: HMAC-SHA256(key = token, message = purpose).
//
// Args:
//   - `purpose`: Purpose string. "starkive-session" for session signatures, "starkive-github-app" for encrypting the App client secret,
//     "starkive-sync" for the scheduled sync key, "starkive-check-deployment" for the deployment check proof
//
// Returns:
//   - A 32-byte key
//
// NOTE: No separate secret needs to be configured.
// When the token changes, everything made with these keys becomes invalid together (sessions, the encrypted App config, the sync key).
// Different purposes give different keys, so a signature or ciphertext for one purpose can't be used for another.
// If the token leaks, the whole DB repository is already exposed.
// So making the keys from the token doesn't add to what is lost.
func deriveKey(cfg *Config, purpose string) []byte {
	mac := hmac.New(sha256.New, []byte(cfg.DBRepoAccessToken))
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

// Signs a value: HMAC-SHA256(key = `deriveKey(token, "starkive-session")`, message = value), written in base64url.
//
// Args:
//   - `value`: Value to sign (the expiry time of a session cookie, or a sign-in state with its purpose in front)
func sign(value string, cfg *Config) string {
	mac := hmac.New(sha256.New, deriveKey(cfg, "starkive-session"))
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Makes the `Set-Cookie` value of the session cookie for a browser that signed in.
//
// Args:
//   - `now`: Current time. The cookie expires `SessionTTL` after it
func NewSessionCookie(cfg *Config, now time.Time) string {
	exp := strconv.FormatInt(now.Add(SessionTTL).Unix(), 10)
	return fmt.Sprintf("%s=%s.%s; Max-Age=%d; %s", sessionCookie, exp, sign(exp, cfg), int(SessionTTL.Seconds()), cookieAttrs)
}

// Checks that the session cookie of the request is valid.
//
// Returns:
//   - `nil` if it is valid.
//     A 401 `HTTPError` if the cookie is missing, the signature is wrong, or it has expired.
//     The error carries a `Set-Cookie` header that deletes the cookie
//
// NOTE: It checks with the token in the environment variable only, and doesn't ask GitHub.
// So requests from people who aren't signed in don't use the server token's rate limit.
func requireSession(r *http.Request, cfg *Config) error {
	unauthorized := Errorf(http.StatusUnauthorized, "Sign-in required").addHeader("Set-Cookie", ClearSessionCookie)

	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return unauthorized
	}
	exp, sig, found := strings.Cut(cookie.Value, ".")
	if !found || !hmac.Equal([]byte(sig), []byte(sign(exp, cfg))) { // `hmac.Equal`: constant-time compare, so the time taken doesn't tell how many characters match
		return unauthorized
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expires <= time.Now().Unix() {
		return unauthorized
	}
	return nil
}
