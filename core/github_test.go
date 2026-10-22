// Unit tests for calling the GitHub API (`github.go`)
package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	cfg := &Config{GitHubAPI: server.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}
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
