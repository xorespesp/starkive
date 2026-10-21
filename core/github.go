// Shared code for calling the GitHub API.
// It sends requests (`CallGitHubAPI`), turns failure answers into errors (`CheckGitHubAPI`, `GitHubAPIError`),
// tells rate limit answers apart (`checkGitHubAPIWithRateLimit`), runs GraphQL queries (`callGraphQL`, `graphQLErrors`),
// and builds Contents API paths.
// Calls for each feature (saving the archive, sync, sign-in) are in their own files.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Longest time to wait for a GitHub answer.
// No single request takes longer than this, so the work ends within the Vercel function time limit.
const githubTimeout = 20 * time.Second

// HTTP client for GitHub requests. Go's default client has no time limit, so a separate one is made
var httpClient = &http.Client{Timeout: githubTimeout}

// Options of a GitHub API request. Empty fields use the default
type GitHubAPIRequest struct {
	Method  string        // HTTP method. GET if empty
	Accept  string        // `Accept` header. `application/vnd.github+json` if empty
	Body    any           // Body to send as JSON. `nil` if there is none
	Token   string        // Token to send instead of the DB repository token. The DB repository token if empty
	Timeout time.Duration // Time limit of this request. `githubTimeout` if 0 or longer than it
}

// Sends a request to the GitHub API (REST or GraphQL).
//
// Args:
//   - `path`: Path and query after `cfg.GitHubAPI` (e.g. `/repos/me/archive/git/ref/heads/main`)
//   - `opt`: Method, `Accept`, body, token and time limit
//
// Returns:
//   - The response. Failure answers (4xx, 5xx) are returned without an error too.
//     The caller checks it with `CheckGitHubAPI`, and closes it with `res.Body.Close()` after reading
//   - The error if the request couldn't be sent or no answer came back (including a timeout)
func CallGitHubAPI(cfg *Config, path string, opt GitHubAPIRequest) (*http.Response, error) {
	if opt.Method == "" {
		opt.Method = http.MethodGet
	}
	if opt.Accept == "" {
		opt.Accept = "application/vnd.github+json"
	}

	var body io.Reader // If `nil`, the request is sent without a body
	if opt.Body != nil {
		encoded, err := json.Marshal(opt.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(opt.Method, cfg.GitHubAPI+path, body)
	if err != nil {
		return nil, err
	}
	token := cfg.DBRepoAccessToken
	if opt.Token != "" {
		token = opt.Token
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", opt.Accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "starkive")
	req.Header.Set("X-Github-Next-Global-ID", "1") // Gets the `node_id` of old repositories in the new format (`R_kgDO...`) too. GitHub plans to retire the old format (`MDEw...`) (docs)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := httpClient
	if opt.Timeout > 0 && opt.Timeout < githubTimeout {
		client = &http.Client{Timeout: opt.Timeout} // A client is cheap to make. It shares the default connection pool
	}
	return client.Do(req)
}

// Sends a GitHub GraphQL query and fills a value with the `data` of the result.
//
// Args:
//   - `query`: GraphQL query
//   - `variables`: Query variables
//   - `timeout`: Time limit of the request. `githubTimeout` if 0 or longer than it
//   - `dst`: Pointer to the value to fill with `data`
//
// Returns:
//   - `graphQLErrors` if the answer has errors along with `data` (a partial result).
//     `dst` is filled even then
//   - An error that wraps `errGitHubRateLimited` if GitHub says the rate limit was reached.
//     It also wraps a 502 `HTTPError` when the answer was 200, or the `GitHubAPIError` when it was 403 or 429
//   - A `GitHubAPIError` on any other HTTP failure.
//     A 502 `HTTPError` if the answer has no `data`
//
// NOTE: GraphQL can answer HTTP 200 and still report failures in `errors` in the body.
// A rate limit is answered the same way, with no points left or a `Retry-After` header (docs).
// The error type `RATE_LIMITED` isn't in the docs and wasn't measured, so it is only one of the signs.
// With no points left, any error counts as the rate limit, even beside usable `data`.
// That `data` is dropped, because the next request would be refused anyway.
func callGraphQL(cfg *Config, query string, variables map[string]any, timeout time.Duration, dst any) error {
	body := map[string]any{"query": query, "variables": variables}
	res, err := CallGitHubAPI(cfg, "/graphql", GitHubAPIRequest{Method: http.MethodPost, Body: body, Timeout: timeout})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := checkGitHubAPIWithRateLimit(res); err != nil {
		return err
	}
	var result struct {
		Data   json.RawMessage `json:"data"` // Raw JSON, not decoded yet. Decoded into `dst` at the end
		Errors graphQLErrors   `json:"errors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return err
	}
	for _, e := range result.Errors {
		if e.Type == "RATE_LIMITED" || res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" {
			return fmt.Errorf("%w: %w", errGitHubRateLimited, Errorf(http.StatusBadGateway, "GitHub GraphQL: %s", e.Message)) // Two `%w`: `errors.Is` and `errors.As` find both errors
		}
	}
	if len(result.Data) == 0 || string(result.Data) == "null" {
		if len(result.Errors) > 0 {
			return Errorf(http.StatusBadGateway, "%s", result.Errors.Error())
		}
		return Errorf(http.StatusBadGateway, "GitHub GraphQL returned no data")
	}
	if err := json.Unmarshal(result.Data, dst); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return result.Errors
	}
	return nil
}

// One error in a GitHub GraphQL answer
type graphQLError struct {
	Type    string `json:"type"`    // Kind of error (e.g. `NOT_FOUND`). Some errors have none
	Path    []any  `json:"path"`    // Field where it happened, like `["nodes", 1]`. It mixes strings and numbers, so it is read as `any`. JSON numbers become `float64`
	Message string `json:"message"` // GitHub's message
}

// Errors of a GitHub GraphQL answer that also has `data` (a partial result).
// Each field that failed is `null` in `data`, and the rest of `data` can be used (docs).
// A caller that can use a partial result takes this out of the error with `errors.As`.
// For any other caller it is a failure like any other.
type graphQLErrors []graphQLError

// Implements Go's `error` interface. The message is the first error's
func (errs graphQLErrors) Error() string { return "GitHub GraphQL: " + errs[0].Message }

// Returns the same failure as a 502 `HTTPError`, so `Respond` answers 502 as for any GitHub failure.
// `errors.As` looks through this method (Go's convention for an error that wraps another)
func (errs graphQLErrors) Unwrap() error { return Errorf(http.StatusBadGateway, "%s", errs.Error()) }

// Reports whether the errors say that the node at position `index` of `nodes` wasn't found (`NOT_FOUND`), such as a deleted repository (docs).
// Only this says a node is gone.
// A node that is `null` for another reason may come back later.
func (errs graphQLErrors) isNodeNotFound(index int) bool {
	for _, e := range errs {
		if e.Type != "NOT_FOUND" || len(e.Path) != 2 || e.Path[0] != "nodes" {
			continue
		}
		if i, ok := e.Path[1].(float64); ok && int(i) == index { // `.(float64)`: takes the value in an `any` out as its original type
			return true
		}
	}
	return false
}

// Reports whether any error is about the node at position `index` of `nodes`, or about a field inside it (path `["nodes", index, ...]`).
// A field that failed is `null` in `data` too, so it can't be told from a field that is really `null`.
func (errs graphQLErrors) hasErrorInNode(index int) bool {
	for _, e := range errs {
		if len(e.Path) < 2 || e.Path[0] != "nodes" {
			continue
		}
		if i, ok := e.Path[1].(float64); ok && int(i) == index {
			return true
		}
	}
	return false
}

// Error for a GitHub answer that says the rate limit was reached
var errGitHubRateLimited = errors.New("GitHub rate limit reached")

// Checks a GitHub answer like `CheckGitHubAPI`, and wraps a rate limit answer with `errGitHubRateLimited`.
// A rate limit answer is 429, or 403 with no requests left, a `Retry-After` header, or "rate limit" in the message (docs).
func checkGitHubAPIWithRateLimit(res *http.Response) error {
	err := CheckGitHubAPI(res)
	var apiErr *GitHubAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	isRateLimited := res.StatusCode == http.StatusTooManyRequests ||
		(res.StatusCode == http.StatusForbidden && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(apiErr.Message), "rate limit")))
	if isRateLimited {
		return fmt.Errorf("%w: %w", errGitHubRateLimited, err) // Two `%w`: `errors.Is` finds both errors
	}
	return err
}

// Error for a failure that GitHub answered with an HTTP status code. `CheckGitHubAPI` makes it.
// Failures of the sign-in code exchange endpoint, which answers in JSON, are the same.
//
// NOTE: The status code belongs to GitHub, so it isn't used as the status code of our API (`Respond` always answers 502).
// To decide by GitHub's status code inside the server, use `hasGitHubStatus`.
// Sometimes GitHub answers success (200) but the content can't be used (GraphQL `errors`, an empty user, and so on).
// That is not this type.
// It is a 502 `HTTPError` that we make after looking at the content.
type GitHubAPIError struct {
	Status  int    // GitHub's status code
	Message string // GitHub's message. If the token lacks a permission, the needed permission is added at the end
}

// Implements Go's `error` interface. The message is `GitHub <status code>: <message>`
func (e *GitHubAPIError) Error() string { return fmt.Sprintf("GitHub %d: %s", e.Status, e.Message) }

// Reports whether error `err` is a failure that GitHub answered with status code `status` (e.g. 404).
// It also looks inside errors wrapped with `%w` (`errors.As`)
func hasGitHubStatus(err error, status int) bool {
	var apiErr *GitHubAPIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// Checks whether a GitHub answer is a failure (not 2xx).
//
// Args:
//   - `res`: Response returned by `CallGitHubAPI`
//
// Returns:
//   - `nil` on success (the caller reads and closes the body).
//     On failure, it closes the body and returns a `GitHubAPIError` with GitHub's message and the needed permission
func CheckGitHubAPI(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	defer res.Body.Close() // `defer`: runs when this function ends (like a scope guard in C++)

	message := res.Status
	var body struct {
		Message string `json:"message"` // Struct tag: puts the JSON key `message` into this field
	}
	if json.NewDecoder(res.Body).Decode(&body) == nil && body.Message != "" {
		message = body.Message
	}

	// If the token lacks a permission, GitHub puts the permission this request needs in a header (e.g. "contents=write").
	// It is a clue for fixing the token, so it is added to the message
	if needed := res.Header.Get("X-Accepted-GitHub-Permissions"); needed != "" {
		message += " (token needs: " + needed + ")"
	}
	return &GitHubAPIError{Status: res.StatusCode, Message: message}
}

// Builds the Contents API path of one file in the DB repository (`/repos/<repository>/contents/<path>`).
//
// Args:
//   - `filePath`: File path in the repository. Characters not allowed in a path are encoded
func BuildContentsPath(cfg *Config, filePath string) string {
	escaped := (&url.URL{Path: filePath}).EscapedPath()
	return fmt.Sprintf("/repos/%s/contents/%s", cfg.DBRepoFullName, escaped)
}
