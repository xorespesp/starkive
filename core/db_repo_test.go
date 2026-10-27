// Tests for the DB repository: the things that define its format (`DBRepoFormat`), and finding it (`FindDBRepo`).
package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Catches changes that may require raising the format version.
// It writes out the DB repository layout (path constants, the shard count, where sample IDs go and sample README paths),
// the marker, the line that marks a truncated README,
// and the JSON shape of the config file (`dbConfigFile`) and the data files (`StarItem`, `StarList`, `RepoMeta`), and pins their hash.
// When they change, the test fails.
// Then decide whether `DBRepoFormat` must go up (a layout or shape change is a major change),
// so an old dashboard or workflow doesn't read or write the new layout wrongly.
// If it doesn't need to go up, only update the hash.
// The contract with the workflow is guarded separately by `TestSyncWorkflowChangeIsReviewed` in the setup tool.
func TestDBRepoFormatChangeIsReviewed(t *testing.T) {
	readmePath := FetchedReadme{extension: "md"}.buildSnapshotPath(12345)
	absentMarkerPath := FetchedReadme{isAbsent: true}.buildSnapshotPath(12345)
	shardRule := fmt.Sprint(StarItemShardCount) // The shard count and where sample IDs go fix the item layout
	for _, id := range []int64{1, 8, 12345, 1420266966} {
		shardRule += " " + BuildStarItemShardPath(computeShardIndexOfStarItem(id))
	}
	shape := strings.Join([]string{
		MarkerDBPath, ConfigDBPath, StarItemsDBDir, shardRule, StarListsDBPath, RepoMetaDBPath, SyncWorkflowPath,
		ReadmesDBDir, readmePath, absentMarkerPath, readmeTruncationLine,
		string(EncodeDBRepoMarker()),
		describeJSONShape(reflect.TypeFor[dbConfigFile]()),
		describeJSONShape(reflect.TypeFor[StarItem]()),
		describeJSONShape(reflect.TypeFor[StarList]()),
		describeJSONShape(reflect.TypeFor[RepoMeta]()),
	}, "\n")
	const reviewed = "f2639a82a616b0b209d19d3b1f8e4c74e72410556cabe0aabcab12309ce73e56"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(shape))); got != reviewed {
		t.Errorf("the DB repository format changed (sha256 %s). Decide whether core.DBRepoFormat must go up, then update the hash in this test:\n%s", got, shape)
	}
}

// Writes out, as text, the JSON shape of type `t`.
// For a struct, it writes the JSON tag and shape of each exported field, in field order (the field order is the key order in the file).
// For a pointer, slice or map, it writes the shape of the element.
// For a type that makes its own JSON (`json.Marshaler`, e.g. `time.Time`) and for everything else, it writes the type name
func describeJSONShape(t reflect.Type) string {
	marshaler := reflect.TypeFor[json.Marshaler]()
	if t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) {
		return t.String()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + describeJSONShape(t.Elem())
	case reflect.Slice:
		return "[]" + describeJSONShape(t.Elem())
	case reflect.Map:
		return "map[" + describeJSONShape(t.Key()) + "]" + describeJSONShape(t.Elem())
	case reflect.Struct:
		fields := []string{}
		for field := range t.Fields() {
			if !field.IsExported() || field.Tag.Get("json") == "-" { // Fields that don't appear in JSON
				continue
			}
			fields = append(fields, fmt.Sprintf("%q:%s", field.Tag.Get("json"), describeJSONShape(field.Type)))
		}
		return "{" + strings.Join(fields, ",") + "}"
	}
	return t.Kind().String()
}

// Tests that `FindDBRepo` gives each caller its own copy, with the caller's own config, also when the result comes from the cache (`dbRepoCache`).
// The second call shares the cache key with the first (`buildEnvCacheKey`) but has another `GitHubWeb`.
// Changing the first caller's repository changes neither the kept result nor the second caller's
func TestFindDBRepoGivesEachCallerItsOwnCopy(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repositories/4242":
			fmt.Fprint(w, `{"full_name":"me/archive","default_branch":"main"}`)
		case "/graphql":
			marker, _ := json.Marshal(string(EncodeDBRepoMarker())) // The marker file content as a JSON string
			fmt.Fprintf(w, `{"data":{"repository":{"isEmpty":false,"marker":{"text":%s}}}}`, marker)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	first := &EnvConfig{DBRepoID: 4242, DBRepoAccessToken: "test-token", GitHubAPI: server.URL, GitHubWeb: "https://first.example"}
	second := &EnvConfig{DBRepoID: 4242, DBRepoAccessToken: "test-token", GitHubAPI: server.URL, GitHubWeb: "https://second.example"}
	firstRepo, err := FindDBRepo(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	firstRepo.branch = "changed"
	secondRepo, err := FindDBRepo(ctx, second)
	if err != nil || firstRepo.cfg != first || secondRepo.cfg != second || secondRepo.branch != "main" || secondRepo.state != DBRepoStarkive {
		t.Errorf("first %+v, second %+v, %v", firstRepo, secondRepo, err)
	}
}
