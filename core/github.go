// Shared code for calling the GitHub API.
// It sends requests (`CallGitHubAPI`), turns failure answers into errors (`CheckGitHubAPI`, `GitHubAPIError`),
// runs GraphQL queries, and builds Contents API paths.
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
//   - `opt`: Method, `Accept`, body and token
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
//   - `dst`: Pointer to the value to fill with `data`
//
// Returns:
//   - A `GitHubAPIError` on HTTP failure.
//     A 502 `HTTPError` with the first message if the answer has `errors`
//
// NOTE: GraphQL can answer HTTP 200 and still report a failure in `errors` in the body.
// So any `errors` counts as an error.
func callGraphQL(cfg *Config, query string, variables map[string]any, dst any) error {
	res, err := CallGitHubAPI(cfg, "/graphql", GitHubAPIRequest{Method: http.MethodPost, Body: map[string]any{"query": query, "variables": variables}})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return err
	}
	var result struct {
		Data   json.RawMessage `json:"data"` // Raw JSON, not decoded yet. Decoded into `dst` at the end
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return Errorf(http.StatusBadGateway, "GitHub GraphQL: %s", result.Errors[0].Message)
	}
	return json.Unmarshal(result.Data, dst)
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
