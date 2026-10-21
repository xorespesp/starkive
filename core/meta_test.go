// Unit tests for the metadata snapshots (`meta.go`)
package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Tests which repositories `FetchRepoMeta` reports as gone.
//   - A repository is gone only when GraphQL answers `null` with `NOT_FOUND` at its position
//   - `null` for another reason (for example, an organization's sign-in rule) and an ID of another kind (`{}`) aren't gone
//   - The info of the other repositories is still given
//   - An answer with no data is an error, so no repository is judged gone
func TestFetchRepoMeta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"nodes":[{"id":"R_a","nameWithOwner":"o/a"},null,null,{}]},"errors":[
			{"type":"NOT_FOUND","path":["nodes",1],"message":"Could not resolve to a node"},
			{"type":"FORBIDDEN","path":["nodes",2],"message":"Resource protected by organization SAML enforcement"}]}`)
	}))
	t.Cleanup(server.Close)
	cfg := &Config{GitHubAPI: server.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}

	nodes, missing, err := FetchRepoMeta(cfg, []string{"R_a", "R_gone", "R_saml", "U_user"})
	if err != nil || len(nodes) != 4 || nodes[0] == nil || nodes[1] != nil || nodes[2] != nil || nodes[3] != nil || !slices.Equal(missing, []string{"R_gone"}) {
		t.Errorf("%v, nodes %q, missing %v", err, nodes, missing)
	}

	noData := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"message":"Something went wrong"}]}`)
	}))
	t.Cleanup(noData.Close)
	cfg.GitHubAPI = noData.URL
	if nodes, missing, err := FetchRepoMeta(cfg, []string{"R_a"}); err == nil || nodes != nil || missing != nil {
		t.Errorf("no data: %v, %q, %v", err, nodes, missing)
	}
}
