// Unit tests for calling the GitHub API (`github.go`)
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests how `callGraphQL` reads each kind of GraphQL answer, against a test server.
//   - An answer with `data` only fills the value and isn't an error
//   - An answer with `data` and errors (a partial result) fills the value and returns `graphQLErrors`,
//     which tells a node that wasn't found from a node that failed for another reason, and from an error elsewhere.
//     For a caller that doesn't take it out, it is still a 502 `HTTPError`
//   - An answer with no `data` is a 502 `HTTPError`, not a partial result.
//     So is an answer that isn't JSON, or whose `data` has another shape
//   - A rate limit answered with 200 and an error, or with 403, wraps `errGitHubRateLimited`.
//     The one answered with 200 is also a 502 `HTTPError`, and the 403 one keeps GitHub's status.
//     The last point used with no error is still a success
//   - Any other HTTP failure is a `GitHubAPIError` that doesn't wrap `errGitHubRateLimited`
func TestCallGraphQL(t *testing.T) {
	ctx := context.Background()
	var answer func(w http.ResponseWriter) // What the server answers. Set by each case
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			http.Error(w, `{"message":"unexpected request"}`, http.StatusTeapot)
			return
		}
		answer(w)
	}))
	t.Cleanup(server.Close)
	cfg := &EnvConfig{GitHubAPI: server.URL, DBRepoAccessToken: "test-token"}
	type nodesData struct {
		Nodes []*struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}

	answer = func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":{"nodes":[{"name":"a"}]}}`) }
	var full nodesData
	if err := callGraphQL(ctx, cfg, "query", nil, &full); err != nil || len(full.Nodes) != 1 || full.Nodes[0].Name != "a" {
		t.Errorf("data only: %v, %+v", err, full)
	}
	answer = func(w http.ResponseWriter) { // The query that used the last point
		w.Header().Set("X-RateLimit-Remaining", "0")
		fmt.Fprint(w, `{"data":{"nodes":[{"name":"a"}]}}`)
	}
	var last nodesData
	if err := callGraphQL(ctx, cfg, "query", nil, &last); err != nil || len(last.Nodes) != 1 {
		t.Errorf("last point with no error: %v, %+v", err, last)
	}

	answer = func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"data":{"nodes":[{"name":"a"},null,null]},"errors":[
			{"type":"NOT_FOUND","path":["nodes",1],"message":"Could not resolve to a node"},
			{"type":"NOT_FOUND","path":["owner",0],"message":"Could not resolve to a User"},
			{"type":"FORBIDDEN","path":["nodes",2],"message":"Resource protected by organization SAML enforcement"}]}`)
	}
	var partial nodesData
	err := callGraphQL(ctx, cfg, "query", nil, &partial)
	var nodeErrs graphQLErrors
	var httpErr *HTTPError
	if !errors.As(err, &nodeErrs) || len(partial.Nodes) != 3 || partial.Nodes[0].Name != "a" || partial.Nodes[1] != nil {
		t.Errorf("partial result: %v, %+v", err, partial)
	}
	if nodeErrs.isNodeNotFound(0) || !nodeErrs.isNodeNotFound(1) || nodeErrs.isNodeNotFound(2) {
		t.Errorf("not found: %v %v %v", nodeErrs.isNodeNotFound(0), nodeErrs.isNodeNotFound(1), nodeErrs.isNodeNotFound(2))
	}
	inField := graphQLErrors{{Type: "FORBIDDEN", Path: []any{"nodes", float64(3), "root"}}, {Type: "NOT_FOUND", Path: []any{"owner", float64(0)}}}
	if !inField.hasErrorInNode(3) || inField.hasErrorInNode(0) || !nodeErrs.hasErrorInNode(2) || nodeErrs.hasErrorInNode(0) {
		t.Errorf("errors in a node: %v %v %v %v", inField.hasErrorInNode(3), inField.hasErrorInNode(0), nodeErrs.hasErrorInNode(2), nodeErrs.hasErrorInNode(0))
	}
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || httpErr.Message != "GitHub GraphQL: Could not resolve to a node" {
		t.Errorf("partial result as an HTTP error: %+v", httpErr)
	}

	cases := []struct {
		name          string
		answer        func(w http.ResponseWriter)
		isRateLimited bool // The error wraps `errGitHubRateLimited`
		wantStatus    int  // Status of the `HTTPError` it wraps. 0 if it wraps none
		githubStatus  int  // Status of the `GitHubAPIError` it wraps. 0 if it wraps none
	}{
		{"no data", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`)
		}, false, http.StatusBadGateway, 0},
		{"null data and no errors", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null}`)
		}, false, http.StatusBadGateway, 0},
		{"not JSON", func(w http.ResponseWriter) {
			fmt.Fprint(w, `<html>Unicorn!</html>`)
		}, false, http.StatusBadGateway, 0},
		{"data of another shape", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":{"nodes":"a"}}`)
		}, false, http.StatusBadGateway, 0},
		{"rate limit with 200", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		}, true, http.StatusBadGateway, 0},
		{"rate limit with 200, header only (the measured shape)", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"API rate limit exceeded for user ID 4242."}]}`)
		}, true, http.StatusBadGateway, 0},
		{"rate limit type only", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		}, true, http.StatusBadGateway, 0},
		{"secondary rate limit with 200", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "60")
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit"}]}`)
		}, true, http.StatusBadGateway, 0},
		{"rate limit with 403", func(w http.ResponseWriter) {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden)
		}, true, 0, http.StatusForbidden},
		{"server error", func(w http.ResponseWriter) {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		}, false, 0, http.StatusBadGateway},
	}
	for _, c := range cases {
		answer = c.answer
		var data nodesData
		err := callGraphQL(ctx, cfg, "query", nil, &data)
		var nodeErrs graphQLErrors
		var httpErr *HTTPError
		status := 0
		if errors.As(err, &httpErr) {
			status = httpErr.Status
		}
		if err == nil || errors.As(err, &nodeErrs) || errors.Is(err, errGitHubRateLimited) != c.isRateLimited ||
			status != c.wantStatus || !hasGitHubStatusOrNone(err, c.githubStatus) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// Reports whether error `err` wraps a `GitHubAPIError` with status `status`, or wraps none when `status` is 0
func hasGitHubStatusOrNone(err error, status int) bool {
	var apiErr *GitHubAPIError
	if status == 0 {
		return !errors.As(err, &apiErr)
	}
	return hasGitHubStatus(err, status)
}

// Tests which errors `isTemporaryGitHubError` counts as passing on their own.
//   - Passing on their own: the deadline of `ctx` cut the work, no answer came, GitHub failing on its side (5xx),
//     the rate limit, and other commits that kept moving the branch. Also when wrapped in another error
//   - Needing a person: a refused token (401), a missing permission (403), a rejected request (422),
//     an answer that can't be read, and any other error
//   - A status that GitHub answered is judged by the status even after the deadline of `ctx` has passed
func TestIsTemporaryGitHubError(t *testing.T) {
	ctx := context.Background()
	cutCtx, cancel := context.WithCancel(ctx)
	cancel() // Already ended, as if its deadline had passed
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"deadline cut the work", cutCtx, errors.New("any error"), true},
		{"no answer", ctx, newGitHubUnreachableError(errors.New("connection reset")), true},
		{"GitHub failing", ctx, &GitHubAPIError{Status: http.StatusBadGateway}, true},
		{"wrapped", ctx, fmt.Errorf("refresh readmes: %w", &GitHubAPIError{Status: http.StatusServiceUnavailable}), true},
		{"rate limit", ctx, fmt.Errorf("%w: %w", errGitHubRateLimited, &GitHubAPIError{Status: http.StatusForbidden}), true},
		{"branch kept moving", ctx, errBranchMoved, true},
		{"token refused", ctx, &GitHubAPIError{Status: http.StatusUnauthorized, Message: "Bad credentials"}, false},
		{"missing permission", ctx, &GitHubAPIError{Status: http.StatusForbidden, Message: "Resource not accessible by personal access token"}, false},
		{"rejected request", ctx, &GitHubAPIError{Status: http.StatusUnprocessableEntity}, false},
		{"answer can't be read", ctx, newGitHubUnreadableError(errors.New("invalid character")), false},
		{"other error", ctx, errors.New("broken file"), false},
		{"token refused before the deadline passed", cutCtx, &GitHubAPIError{Status: http.StatusUnauthorized}, false},
		{"GitHub failing before the deadline passed", cutCtx, &GitHubAPIError{Status: http.StatusBadGateway}, true},
	}
	for _, c := range cases {
		if got := isTemporaryGitHubError(c.ctx, c.err); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// Tests which errors `hasGitHubStatusBetween` and `hasGitHubStatus` match.
//   - A status inside the range matches, both ends included, also when wrapped in another error
//   - A status outside the range, and an error that isn't a `GitHubAPIError`, don't match
//   - `hasGitHubStatus` matches only the exact status
func TestHasGitHubStatusBetween(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"lowest end", &GitHubAPIError{Status: 500}, true},
		{"highest end", &GitHubAPIError{Status: 599}, true},
		{"wrapped", fmt.Errorf("refresh readmes: %w", &GitHubAPIError{Status: 503}), true},
		{"below", &GitHubAPIError{Status: 499}, false},
		{"above", &GitHubAPIError{Status: 600}, false},
		{"not from GitHub", Errorf(http.StatusBadGateway, "GitHub didn't answer in time"), false},
	}
	for _, c := range cases {
		if got := hasGitHubStatusBetween(c.err, 500, 599); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	if !hasGitHubStatus(&GitHubAPIError{Status: 404}, 404) || hasGitHubStatus(&GitHubAPIError{Status: 404}, 403) {
		t.Error("hasGitHubStatus doesn't match the exact status")
	}
}

// Tests how `DecodeGitHubResponse` tells a body that stopped coming from one that can't be read.
//   - JSON that ends in the middle (a dropped connection) is an answer that never came (`gitHubUnreachableError`)
//   - A body that isn't JSON (an HTML error page) is an answer that can't be read, not `gitHubUnreachableError`.
//     Both are 502 `HTTPError`s
func TestDecodeGitHubResponse(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		wantUnreachable bool
	}{
		{"stopped in the middle", `{"name":"rep`, true},
		{"HTML in place of JSON", "<html>Unicorn!</html>", false},
	}
	for _, c := range cases {
		var dst map[string]any
		err := DecodeGitHubResponse(&http.Response{Body: io.NopCloser(strings.NewReader(c.body))}, &dst)
		var unreachable *gitHubUnreachableError
		var httpErr *HTTPError
		if errors.As(err, &unreachable) != c.wantUnreachable || !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// Tests that `callGraphQL` sends no more queries in a request after a rate limit answer (`gitHubRateLimitMark`).
//   - After the first rate limit answer, a query isn't sent, and its error wraps that answer with the time to retry
//   - Other failures don't stop later queries
//   - A `ctx` without a mark (one not made by `ServeAPIRequest`) keeps sending
func TestCallGraphQLStopsAfterRateLimit(t *testing.T) {
	requests := 0
	var answer func(w http.ResponseWriter) // What the server answers. Set by each step
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		answer(w)
	}))
	t.Cleanup(server.Close)
	cfg := &EnvConfig{GitHubAPI: server.URL, DBRepoAccessToken: "test-token"}
	var data struct{}
	ok := func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":{}}`) }
	failing := func(w http.ResponseWriter) { http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway) }
	limited := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden)
	}

	ctx := withGitHubRateLimitMark(context.Background())
	answer = failing
	if err := callGraphQL(ctx, cfg, "query", nil, &data); errors.Is(err, errGitHubRateLimited) || requests != 1 {
		t.Errorf("server error: %v, requests %d", err, requests)
	}
	answer = limited
	if err := callGraphQL(ctx, cfg, "query", nil, &data); !errors.Is(err, errGitHubRateLimited) || !strings.Contains(err.Error(), "(retry after 60s)") || requests != 2 {
		t.Errorf("rate limit: %v, requests %d", err, requests)
	}
	answer = ok
	err := callGraphQL(ctx, cfg, "query", nil, &data)
	if !errors.Is(err, errGitHubRateLimited) || !hasGitHubStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), "(retry after 60s)") || requests != 2 {
		t.Errorf("after the rate limit: %v, requests %d", err, requests)
	}

	// Without a mark, every query is sent
	answer = limited
	callGraphQL(context.Background(), cfg, "query", nil, &data)
	answer = ok
	if err := callGraphQL(context.Background(), cfg, "query", nil, &data); err != nil || requests != 4 {
		t.Errorf("without a mark: %v, requests %d", err, requests)
	}
}

// Tests when to retry, as `formatGitHubRetryTime` reads it from the headers of a rate limit answer
func TestFormatGitHubRetryTime(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"retry after seconds", map[string]string{"Retry-After": "60"}, " (retry after 60s)"},
		{"retry after in another form", map[string]string{"Retry-After": "soon"}, " (retry after soon)"},
		{"reset with no requests left", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1792779420"}, " (resets at 18:17:00 UTC)"},
		{"retry after comes first", map[string]string{"Retry-After": "30", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1792779420"}, " (retry after 30s)"},
		{"reset while requests are left", map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "1792779420"}, ""},
		{"no headers", map[string]string{}, ""},
	}
	for _, c := range cases {
		header := http.Header{}
		for key, value := range c.header {
			header.Set(key, value)
		}
		if got := formatGitHubRetryTime(header); got != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
