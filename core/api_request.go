// Reading and checking the requests that our API gets.
// It covers the JSON body and the `?id=` value, the length limits of text fields that people type,
// request checks (`Authorize`, `RequireSameOrigin`), and the site address a request came to.
// Making the answers is in `api_response.go`.
package core

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fills a value with the JSON of the request body.
//
// Args:
//   - `dst`: Pointer to the value to fill
//
// Returns:
//   - A 400 `HTTPError` if the body isn't JSON or doesn't fit the shape
func ReadAPIRequestJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return Errorf(http.StatusBadRequest, "Malformed request body")
	}
	return nil
}

// Reads the `?id=` value of the request address as an integer.
//
// Returns:
//   - The ID. A 400 `HTTPError` if it is missing or not a number
func ParseAPIRequestID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		return 0, Errorf(http.StatusBadRequest, "Invalid id")
	}
	return id, nil
}

// Maximum length (in characters) of text fields that people type.
// All limits live here, and `CheckTextLength` checks them.
// The `maxLength` of the input boxes on the page (`TEXT_LIMITS` in `public/app.js`) has the same values.
// The real check is done by the server
const (
	MaxNoteLength                = 2000 // Note of an archive item. Also keeps notes from growing the archive file so much that the list answer passes Vercel's response limit (4.5MB, docs)
	MaxStarListNameLength        = 100  // Star list name
	MaxStarListDescriptionLength = 300  // Star list description
)

// Checks the length of the text field `value`.
// Length is counted in characters (runes), not bytes.
// A Korean letter is 3 bytes in UTF-8, but it counts as one character.
//
// Args:
//   - `field`: Field name for the error message (English)
//   - `required`: If `true`, an empty value is refused too
//   - `max`: Maximum number of characters
//
// Returns:
//   - `nil` if it fits, otherwise a 400 `HTTPError`
func CheckTextLength(field string, value string, required bool, max int) error {
	n := utf8.RuneCountInString(value)
	if required && (n == 0 || n > max) {
		return Errorf(http.StatusBadRequest, "%s must be 1 to %d characters", field, max)
	}
	if n > max {
		return Errorf(http.StatusBadRequest, "%s must be at most %d characters", field, max)
	}
	return nil
}

// Refuses requests sent from other sites (CSRF).
// Browsers add an `Origin` header to requests sent to another origin.
// If that host differs from this site, the request is refused.
//
// Returns:
//   - `nil` for the same site or when there is no `Origin`. A 403 `HTTPError` for another site
//
// NOTE: A request without `Origin` comes from a client that isn't a browser, so it passes.
// For requests from browsers, the session cookie is also `SameSite=Strict`, so the browser doesn't send it with requests from other sites.
func RequireSameOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host != readRequestHost(r) {
		return Errorf(http.StatusForbidden, "Cross-origin request rejected")
	}
	return nil
}

// Returns the host of this site as the browser opened it (`example.vercel.app`, `localhost:3000`)
func readRequestHost(r *http.Request) string {
	if host := r.Header.Get("X-Forwarded-Host"); host != "" { // Vercel puts the original host the browser opened in this header (docs)
		return host
	}
	return r.Host
}

// Returns the address of this site that the request came to, in the `NormalizeSiteURL` shape (`https://example.vercel.app`).
//
// NOTE: The request has no scheme, so it is decided here.
// Only the local development server (`localhost`, `127.0.0.1`) is http.
// Everything else is https.
func ReadAPIRequestSiteURL(r *http.Request) string {
	host := strings.ToLower(readRequestHost(r))
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	scheme := "https"
	if hostname == "localhost" || hostname == "127.0.0.1" {
		scheme = "http"
	}
	return NormalizeSiteURL(scheme + "://" + host)
}

// Brings a site address into the same shape as a browser origin, `scheme://host[:port]`.
// The scheme and host become lower case, the default port (https 443, http 80) is removed, and the path is dropped.
// This way a stored address and a compared address have the same shape.
//
// Args:
//   - `raw`: Address typed by a person or returned by GitHub
//
// Returns:
//   - The normalized address. An empty string if it can't be parsed as an address
//
// NOTE: Browsers change addresses into this shape when they send requests.
// Suppose a person pastes an address with capital letters or `:443`, and it is stored and compared as it is.
// Then the page is already at that address and has nowhere to move, and starting sign-in is refused forever.
func NormalizeSiteURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = "" // Browsers also leave out the default port in requests
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port) // An IPv6 address is wrapped in `[...]` to separate it from the port
	case strings.Contains(host, ":"): // Puts back the brackets of an IPv6 address that `Hostname` removed
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// Does the first steps of an API request from a signed-in user.
// It refuses requests from other sites, reads the environment variable, checks the session cookie,
// and opens the DB repository, which must be a starkive repository (`OpenDBRepo`).
//
// Returns:
//   - The opened DB repository.
//     An `HTTPError` otherwise: 403 for a request from another site, 401 without a session, 409 for a DB repository that can't be used, and so on
//
// NOTE: A request from another site is always refused unless it is a GET.
// If callers chose whether to check, a new write API could forget it.
// The session is checked with the environment variable alone, before asking GitHub.
// So a request without sign-in causes no GitHub request.
func Authorize(r *http.Request) (*DBRepo, error) {
	ctx := r.Context() // The deadline that `ServeAPIRequest` set for this request
	if r.Method != http.MethodGet {
		if err := RequireSameOrigin(r); err != nil {
			return nil, err
		}
	}
	cfg, err := LoadEnvConfig() // Don't ask GitHub until the session is checked
	if err != nil {
		return nil, err
	}
	if err := requireSession(r, cfg); err != nil {
		return nil, err
	}
	return OpenDBRepo(ctx, cfg)
}
