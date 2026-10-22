// Integration tests for the request deadline that `core.ServeAPIRequest` sets, and a guard that the server code makes no request without it
package test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that a write goes on to the end when the browser goes away (`core.ServeAPIRequest`).
// The request's own `ctx` is cancelled before the handler runs, as when the tab is closed right after pressing save.
// The note is still saved, because the work follows only the deadline that `core.ServeAPIRequest` sets
func TestWriteFinishesAfterTheBrowserLeaves(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	browserGone, leave := context.WithCancel(context.Background())
	leave()
	req := httptest.NewRequest("PATCH", "http://example.test/api/archive-star-items?id=1", strings.NewReader(`{"note":"kept"}`)).WithContext(browserGone)
	req.Header.Set("Origin", origin)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	api.HandleArchiveStarItems(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(gh.readFile(core.StarItemsDBPath), `"note":"kept"`) {
		t.Errorf("write after the browser left: %d %s", rec.Code, rec.Body)
	}
}

// Tests that a request cut at its deadline isn't kept as the result of finding the DB repository (`core.FindDBRepo`).
// Keeping it would fail every other request for a while, though the repository is fine
func TestDeadlineErrorIsNotCached(t *testing.T) {
	setup(t, sampleStarItems, sampleStarLists)
	cfg, err := core.LoadEnvConfig()
	if err != nil {
		t.Fatal(err)
	}
	pastDeadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := core.FindDBRepo(pastDeadline, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cut at the deadline: %v", err)
	}
	if repo, err := core.FindDBRepo(context.Background(), cfg); err != nil || repo.State() != core.DBRepoStarkive {
		t.Errorf("next request: %+v %v", repo, err)
	}
}

// Tests that the server code (`core` and `api`, without tests) never makes a context or a request without the deadline of the request.
// `context.Background()`, `context.TODO()` and `context.WithoutCancel` make a context with no deadline.
// Only `api_response.go`, where `core.ServeAPIRequest` is, may use `context.WithoutCancel`, under the deadline it sets right after.
// `http.NewRequest` and the shortcuts of `net/http` (`http.Get`, `http.Post`, `http.PostForm`, `http.Head`) send a request with no context.
// (`http.NewRequestWithContext` takes one.)
// `http.DefaultClient` has no time limit, so a request through it could wait past `githubTimeout`.
// Either way, a GitHub request could run past the function time limit.
// Every context must come from the request (`r.Context()`, whose deadline `core.ServeAPIRequest` sets) or from a `ctx` argument
func TestServerCodeKeepsTheRequestDeadline(t *testing.T) {
	sourcePaths := []string{}
	for _, pattern := range []string{"../core/*.go", "../api/*.go"} {
		paths, _ := filepath.Glob(pattern)
		sourcePaths = append(sourcePaths, paths...)
	}
	if len(sourcePaths) == 0 {
		t.Fatal("no source files found")
	}
	for _, path := range sourcePaths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0) // Without `parser.ParseComments`, so a comment that names them doesn't count
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			// A name like `context.Background` is a selector expression: the package name, a dot, and the name in it
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			pkg, isIdent := selector.X.(*ast.Ident)
			if !isIdent {
				return true
			}
			name := pkg.Name + "." + selector.Sel.Name
			isAllowedWithoutCancel := filepath.Base(path) == "api_response.go" && name == "context.WithoutCancel"
			if !isAllowedWithoutCancel && slices.Contains([]string{"context.Background", "context.TODO", "context.WithoutCancel", "http.NewRequest", "http.Get", "http.Post", "http.PostForm", "http.Head", "http.DefaultClient"}, name) {
				t.Errorf("%s: %s has no deadline of the request", fileSet.Position(selector.Pos()), name)
			}
			return true
		})
	}
}

// Tests that a sync whose `longStepsEndBy` has already come runs neither the metadata nor the README step (`core.SyncArchiveWithGitHub`).
// Both stop fetching at that time, so the rest of the request is left for commits and the answer
func TestSyncLongStepsStopAtTheirEnd(t *testing.T) {
	setup(t, sampleStarItems, sampleStarLists)
	result, err := core.SyncArchiveWithGitHub(context.Background(), openTestDBRepo(t), time.Now())
	if err != nil || result.NumMetaCheckedRepos != 0 || result.NumCheckedReadmes != 0 {
		t.Errorf("long steps after their end: %+v, %v", result, err)
	}
}
