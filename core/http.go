// HTTP handling shared by the API functions.
// It covers the request deadline and error answers (`requestTimeLimit`, `HTTPError`, `Respond`), reading and writing JSON, request checks (`Authorize`, `RequireSameOrigin`),
// the site address, and the length limits of text fields that people type.
package core

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Error answer that our API sends to the browser.
// If a handler returns this value as an error, `Respond` answers with `Status` and `{"error": Message}`
type HTTPError struct {
	Status  int         // Response status code
	Message string      // Message for the browser (English)
	Header  http.Header // Response headers to send with it. `nil` if there are none
}

// Implements Go's `error` interface. With this method, `*HTTPError` can be returned as an `error`
func (e *HTTPError) Error() string { return e.Message }

// Makes an `HTTPError` from status code `status` and a message in `fmt.Sprintf` format
func Errorf(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Adds one header (`key: value`) to the error answer.
// It returns itself, so calls can be chained like `Errorf(...).addHeader(...)`
func (e *HTTPError) addHeader(key string, value string) *HTTPError {
	if e.Header == nil {
		e.Header = http.Header{}
	}
	e.Header.Add(key, value)
	return e
}

// Time from the start of a request to the deadline of its `ctx` (`Respond`).
// A Vercel function is stopped at 300 seconds (measured, with or without `maxDuration` in `vercel.json`).
// The 10 seconds left let the server still answer and log why it stopped
const requestTimeLimit = 290 * time.Second

// Runs a handler and turns the returned error into a JSON error answer.
// The entry function of each API wraps its per-method handlers with this function.
// The handler gets the request with a `ctx` whose deadline is `requestTimeLimit` from now (`r.Context()`).
//   - `HTTPError`: answers with its status code and message.
//     For a 5xx whose error wraps a cause, the whole error goes to the server log
//   - `GitHubAPIError` (GitHub answered with a failure): always answers 502
//   - Any other error: answers 500 "Internal server error", and writes the details only to the server log
//
// Args:
//   - `handle`: Per-method handler. Returns `nil` when it has written the whole answer, or an error on failure
//
// NOTE: The `ctx` doesn't stop when the browser goes away (`context.WithoutCancel`).
// Otherwise closing the tab right after saving would stop a commit halfway, and the save would be lost.
// It stops only at its deadline, before Vercel stops the function.
// GitHub's status codes aren't used as our status codes.
// The page treats 401 as "session expired" and closes the session.
// If a 401 from GitHub rejecting the server token passed through, a token problem would look like a sign-out.
// The same goes for a 409 from GitHub being read as our save conflict (409).
func Respond(w http.ResponseWriter, r *http.Request, handle func(http.ResponseWriter, *http.Request) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestTimeLimit)
	defer cancel() // `cancel` frees the timer of the deadline when the request ends
	err := handle(w, r.WithContext(ctx))
	if err == nil {
		return
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) { // If `err` is a `*HTTPError` or wraps one with `%w`, it is taken out into `httpErr` (like `dynamic_cast`)
		if httpErr.Status >= 500 && err != error(httpErr) { // The error wraps a cause (e.g. why GitHub couldn't be reached) that the answer leaves out
			log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		}
		for key, values := range httpErr.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		WriteJSON(w, httpErr.Status, map[string]string{"error": httpErr.Message})
		return
	}
	var githubErr *GitHubAPIError
	if errors.As(err, &githubErr) {
		WriteJSON(w, http.StatusBadGateway, map[string]string{"error": githubErr.Error()})
		return
	}

	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal server error"})
}

// Answers 405 to an HTTP method that isn't supported
func WriteMethodNotAllowed(w http.ResponseWriter) {
	WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
}

// Sets the headers of a JSON answer. Every JSON answer goes through this function.
//
// NOTE: Answers contain personal data (notes, star lists).
// `Cache-Control: no-store` keeps them out of the browser cache and caches on the way.
func setJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

// Writes value `body` (a struct, a map, and so on) as a JSON answer with status code `status`
func WriteJSON(w http.ResponseWriter, status int, body any) {
	setJSONHeaders(w)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // By default `<`, `>` and `&` become Unicode escapes, in case the output goes into HTML. Answers don't go into HTML, so it is off
	_ = enc.Encode(body)     // If writing fails, the headers are already sent and nothing can be done, so the error is dropped
}

// Answers with a data file of the DB repository as it is.
// If the browser accepts it, the answer is compressed with gzip.
//
// Args:
//   - `path`: File path in the DB repository (`StarItemsDBPath` and so on)
//
// Returns:
//   - The error if the file couldn't be read. If the file doesn't exist yet, it answers with an empty list `[]`
//
// NOTE: A data file is already the JSON array the browser needs, so it isn't decoded and encoded again.
func WriteDBFile(w http.ResponseWriter, r *http.Request, cfg *Config, path string) error {
	ctx := r.Context() // The deadline that `Respond` set for this request
	content, err := ReadFile(ctx, cfg, path)
	if err != nil {
		return err
	}
	if content == nil {
		content = []byte("[]")
	}
	setJSONHeaders(w)
	w.Header().Add("Vary", "Accept-Encoding") // Tells that the body differs by request header even at the same address
	if !acceptsGzip(r) {
		_, err = w.Write(content)
		return err
	}
	// The whole file goes in one answer, so Vercel's response limit (4.5MB, docs) caps the archive size.
	// Compression makes it about 4 times smaller (measured)
	w.Header().Set("Content-Encoding", "gzip")
	zw := gzip.NewWriter(w) // Writer that compresses what is written and passes it on to `w`. `Close` writes the remaining data and the end mark
	if _, err = zw.Write(content); err != nil {
		return err
	}
	return zw.Close()
}

// Reports whether the request's `Accept-Encoding` says it accepts gzip.
// The value is a comma-separated list like `gzip, deflate, br`.
// Each item can have a preference like `;q=0.5`.
// `q=0` means "not accepted" (docs: RFC 9110).
func acceptsGzip(r *http.Request) bool {
	for _, item := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(item, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q, hasQ := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !hasQ {
			return true // No preference means 1
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
		return err == nil && value > 0
	}
	return false
}

// Fills a value with the JSON of the request body.
//
// Args:
//   - `dst`: Pointer to the value to fill
//
// Returns:
//   - A 400 `HTTPError` if the body isn't JSON or doesn't fit the shape
func ReadJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return Errorf(http.StatusBadRequest, "Malformed request body")
	}
	return nil
}

// Reads the `?id=` value of the request address as an integer.
//
// Returns:
//   - The ID. A 400 `HTTPError` if it is missing or not a number
func ParseIDParam(r *http.Request) (int64, error) {
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
	MaxNoteLength                = 2000 // Note of an archive item. Also keeps notes from growing the archive file so much that the list answer passes Vercel's response limit (4.5MB)
	MaxStarListNameLength        = 100  // Star list name
	MaxStarListDescriptionLength = 300  // Star list description
)

// Checks the length of a text field.
// Length is counted in characters (runes), not bytes.
// A Korean letter is 3 bytes in UTF-8, but it counts as one character.
//
// Args:
//   - `field`: Field name for the error message (English)
//   - `value`: Value to check
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
// For requests from browsers, the session cookie's `SameSite=Strict` also keeps it out of requests from other sites.
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
	if host := r.Header.Get("X-Forwarded-Host"); host != "" { // Vercel puts the original host the browser opened in this header
		return host
	}
	return r.Host
}

// Returns the address of this site that the request came to, in the `NormalizeSiteURL` shape (`https://example.vercel.app`).
//
// NOTE: The request has no scheme, so it is decided here.
// Only the local development server (`localhost`, `127.0.0.1`) is http. Everything else is https.
func ReadSiteURL(r *http.Request) string {
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
// fills in the DB repository and checks that it is a starkive repository (`useDBRepo`).
//
// Returns:
//   - The server config with the DB repository filled in.
//     An `HTTPError` otherwise: 403 for a request from another site, 401 without a session, 409 for a DB repository that can't be used, and so on
//
// NOTE: A request from another site is always refused unless it is a GET.
// If callers chose whether to check, a new write API could forget it.
// The session is checked with the environment variable alone, before asking GitHub.
// So a request without sign-in causes no GitHub request.
func Authorize(r *http.Request) (*Config, error) {
	ctx := r.Context() // The deadline that `Respond` set for this request
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
	if err := useDBRepo(ctx, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
