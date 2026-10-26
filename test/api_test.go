// Integration tests for the APIs and the sync, plus the fake GitHub (`fakeGitHub`) and most helpers that the tests share.
// The helpers that call the sync like the workflow and open the DB repository are in `sync_schedule_test.go`.
// The fake GitHub runs as an `httptest` server.
// The GitHub addresses that the server code calls (`STARKIVE_GH_API_URL`, `STARKIVE_GH_WEB_URL`) point to it, so the real GitHub isn't touched.
// API entry functions (`api.Handle...`) are called directly without an HTTP server, and their responses are checked.
//
// NOTE: These tests can't live in `api/`.
// Vercel tries to turn every .go file in `api/` into a serverless function (measured).
package test

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Fake accounts and values used by all tests
const (
	testUserID       = 4242                  // The server token's owner. The only person who can sign in to the dashboard
	testToken        = "token_4242"          // The server token. The fake GitHub treats `token_<ID>` as the token of the user with that ID
	testRepoID       = 5150                  // ID of the DB repository (`me/archive`)
	otherUserID      = 777                   // Another GitHub user who isn't the token owner
	testClientID     = "test-client-id"      // Client ID of the sign-in GitHub App
	testClientSecret = "test-client-secret"  // Client secret of the sign-in GitHub App
	origin           = "http://example.test" // `Origin` of the requests that `call` sends. Same site as the request address
)

// A fake server that imitates only the GitHub APIs the tests need.
// Its fields set the repository content and GitHub's behavior (outages, missing permissions, commits from another device at the same time, etc.).
// It records the requests it received and the commits made.
// File writes imitate GitHub's Git Data API in three steps: create a tree, create a commit, move the branch.
// If the branch moved in between, the move is refused with 422.
type fakeGitHub struct {
	mu                  sync.Mutex                    // Lock for the fields below. The server code sends several requests to GitHub at the same time
	files               map[string]string             // Files of the commit at the branch tip (`head`): path in the repository -> content
	head                string                        // Commit sha that the branch (`main`) points to
	snapshots           map[string]map[string]string  // Past commit sha -> that commit's files. Used for reads at a commit (`?ref=`)
	parents             map[string]string             // Commit sha on the branch -> parent commit sha. Used to imitate the compare API
	trees               map[string]map[string]string  // Created tree sha -> that tree's files
	pending             map[string]fakeCommit         // Commit sha created but not yet on the branch -> commit
	commits             []string                      // Messages of the commits on the branch, in the order they were made
	starred             []string                      // My GitHub Stars (owner/repo). Repository IDs are made from the names (`makeRepoID`)
	interfereOnce       func(files map[string]string) // A commit from another device that cuts in once, right before the next branch move. It gets the current files and changes them
	interfereAll        bool                          // If `true`, another device commits first on every branch move (constant conflicts)
	treeStatus          int                           // If not 0, tree creation fails with this status code
	treeCalls           int                           // Number of tree creation requests
	treeListCalls       int                           // Number of folder listing requests (tree API with `<commit>:<folder>`)
	blobs               map[string]string             // Created blob sha -> content
	blobReads           int                           // Number of blob reads by sha (`/git/blobs/<sha>`)
	fileReads           []string                      // Paths of the file reads (Contents API), in the order they came
	fileDownloads       []string                      // Paths of the file reads answered with the content, not with 304 (`If-None-Match`)
	readmes             map[int64]fakeReadme          // READMEs of repositories by repository ID. A repository not in it has no README. GraphQL finds it by `node_id` `R_<ID>`
	readmeQueries       int                           // Number of README queries by GraphQL (folder listings and file contents)
	readmeListedIDs     [][]string                    // `node_id`s of each README folder listing query, in the order the queries came
	readmeQueryFails    func(queryNumber int) bool    // If not `nil`, README queries by GraphQL for which it returns `true` fail with `readmeFailStatus`. `queryNumber` counts from 1
	readmeFailStatus    int                           // Status code of the failures that `readmeQueryFails` picks. 502 if 0
	readmeRateLimitFrom int                           // If not 0, README queries by GraphQL from this one on (counting from 1) get the rate limit answer (200 with an error)
	refStatus           int                           // If not 0, the branch move fails with this status code and `refMessage`. The branch doesn't move
	refMessage          string                        // GitHub message used when failing with `refStatus`
	refLostReply        bool                          // If `true`, the branch move is applied but the response is 502 (GitHub applied it, then the response was lost)
	otherAfterLostReply bool                          // If `true`, one more commit from another device lands right after the lost response. Used with `refLostReply`
	gone                map[string]bool               // `node_id`s that GraphQL answers with `NOT_FOUND` (repositories deleted or made private)
	githubStarLists     map[string][]string           // My GitHub star lists: name -> repositories in it (owner/repo)
	graphqlDown         bool                          // If `true`, all GraphQL requests fail with `graphqlDownStatus`
	graphqlDownStatus   int                           // Status code of the failures that `graphqlDown` makes. 502 (a temporary outage) if 0
	metaQueries         int                           // Number of repository info queries by GraphQL (`nodes` by `node_id`, not README queries)
	metaRateLimitFrom   int                           // If not 0, repository info queries from this one on (counting from 1) get the secondary rate limit answer (403)
	githubStarListsDown bool                          // If `true`, only the GitHub star list query (`viewer.lists`) fails with 502
	ownerDown           bool                          // If `true`, `/user` with the server token (the token owner check) fails with 502
	tokenRejected       bool                          // If `true`, every request with the server token is refused with 401 "Bad credentials" (token expired or revoked)
	repoMissing         bool                          // If `true`, looking up the repository by ID gives 404 (the token doesn't select the DB repository)
	repoLookups         int                           // Number of repository lookups by ID (`/repositories/{ID}`)
	repoEmpty           bool                          // If `true`, it is an empty repository with no commits. Writing the first file with the Contents API clears it
	workflowsDenied     bool                          // If `true`, the token has no Workflows permission. Writes that change `.github/workflows/` are refused with 403
	starringDenied      bool                          // If `true`, the token has no Starring permission. Reading Stars gives 403
	starredPageReads    int                           // Number of requests for a page of my Stars
	starredFailFrom     int                           // If not 0, pages of my Stars from this one on fail with 502
	actionsDenied       bool                          // If `true`, the token has no Actions permission. The Actions API gives 403 with the needed permission in a header
	ownerCalls          int                           // Number of `/user` requests with the server token
	contentsHangup      bool                          // If `true`, file read requests are cut off without a response (network error)
	branchReadsLag      bool                          // If `true`, files read by branch name have the previous commit's content (GitHub briefly shows a just-moved branch at the old commit). Reads by commit sha are correct
	branchReadNotFound  string                        // If not empty, a read of this path by branch name gives 404 (a passing 404 for a file that exists). Reads by commit sha are correct
	requests            int                           // Number of all requests received
	workflowState       string                        // State of the sync workflow (`active`, `disabled_manually`, `disabled_inactivity`, etc.)
	workflowToggles     int                           // Number of requests to turn the workflow on or off
	runs                []fakeRun                     // Runs of the sync workflow
	badCommitSubjects   []string                      // Commit subjects that break the format (`commitSubjectRule`). Collected when each commit is made, so none are missed even if a test clears `commits`
}

// Records the message of a commit on the branch.
// If the subject breaks the format (`commitSubjectRule`), it also goes into `badCommitSubjects`,
// and is reported when the test ends (`checkCommitSubjects`).
// Call it while holding `mu`.
//
// Args:
//   - `message`: The whole commit message (subject, blank line, body)
func (f *fakeGitHub) recordCommit(message string) {
	f.commits = append(f.commits, message)
	if subject, _, _ := strings.Cut(message, "\n"); !commitSubjectRule.MatchString(subject) {
		f.badCommitSubjects = append(f.badCommitSubjects, subject)
	}
}

// One workflow run on the fake GitHub
type fakeRun struct {
	event      string    // Why it ran: `schedule` (scheduled) or `workflow_dispatch` (manual)
	status     string    // `queued`, `pending`, `in_progress` or `completed`
	conclusion string    // Result when finished: `success`, `failure`, etc. Empty while in progress
	started    time.Time // Start time
}

// README of one repository on the fake GitHub
type fakeReadme struct {
	name        string // File name (e.g. `README.md`)
	content     string // File content
	unavailable bool   // If `true`, GraphQL gives `null` with an error for the repository, as for a repository it can't give now
}

// Answers a README query by GraphQL: a listing of the `.github` and root folders, or file contents.
// A repository's README (`readmes`) is in its root folder next to `main.go`, and the root has no folder, so no later folder listing comes.
// Like GitHub, it answers only the fields the query asks for, and names each answer by the alias in the query.
// It lists each folder by the path in its expression (`HEAD:<path>`), and finds a file only at the path its variable gives (`HEAD:<name>`).
// A query that uses a variable it wasn't given, or doesn't declare it with the type GitHub expects, fails as a whole.
// A README that is `unavailable` gets `null` with an error, as GitHub does for a repository it can't give now.
// Call it while holding `mu`.
//
// Args:
//   - `query`: The GraphQL query, which tells whether it is a listing or a content query
//   - `rawBody`: The whole request body, for the variables of each file in a content query (`$id<position>`, `$path<position>`)
func (f *fakeGitHub) answerReadmeQuery(w http.ResponseWriter, query string, rawBody []byte) {
	f.readmeQueries++
	if f.readmeQueryFails != nil && f.readmeQueryFails(f.readmeQueries) {
		status := f.readmeFailStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		http.Error(w, `{"message":"Server Error"}`, status)
		return
	}
	if f.readmeRateLimitFrom != 0 && f.readmeQueries >= f.readmeRateLimitFrom {
		// The measured shape: 200, an error with no type, and no points left
		w.Header().Set("X-RateLimit-Remaining", "0")
		io.WriteString(w, `{"data":null,"errors":[{"message":"API rate limit exceeded for user ID 4242."}]}`)
		return
	}
	if !isEveryGraphQLVariableDeclared(query) {
		io.WriteString(w, `{"data":null,"errors":[{"message":"Variable is not declared, is declared with another type, or is declared but not used"}]}`)
		return
	}
	var body struct {
		Variables map[string]any `json:"variables"`
	}
	json.Unmarshal(rawBody, &body)
	readmeOf := func(nodeID string) (fakeReadme, bool) {
		var id int64
		fmt.Sscanf(nodeID, "R_%d", &id)
		readme, ok := f.readmes[id]
		return readme, ok
	}
	data := map[string]any{}
	errs := []map[string]any{}
	switch {
	case readmeFolderFields.MatchString(query):
		nodeIDs, _ := body.Variables["ids"].([]any)
		listedIDs := []string{}
		nodes := []any{}
		for i, id := range nodeIDs {
			listedIDs = append(listedIDs, id.(string))
			nodeID := id.(string)
			readme, ok := readmeOf(nodeID)
			switch {
			case f.gone[nodeID]:
				nodes = append(nodes, nil)
				errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{"nodes", i}, "message": "Could not resolve to a node"})
			case ok && readme.unavailable:
				nodes = append(nodes, nil)
				errs = append(errs, map[string]any{"type": "FORBIDDEN", "path": []any{"nodes", i}, "message": "README failed"})
			default:
				node := map[string]any{}
				for _, field := range readmeFolderFields.FindAllStringSubmatch(query, -1) {
					alias, folderPath := field[1], field[2]
					if folderPath != "" { // Only the root folder exists
						node[alias] = nil
						continue
					}
					names := []string{"main.go"}
					if ok {
						names = append(names, readme.name)
					}
					entries := []any{}
					for _, name := range names {
						entry := map[string]any{}
						if graphQLQueryAsks(query, "name") {
							entry["name"] = name
						}
						if graphQLQueryAsks(query, "type") {
							entry["type"] = "blob"
						}
						if graphQLQueryAsks(query, "mode") {
							entry["mode"] = 0o100644
						}
						entries = append(entries, entry)
					}
					node[alias] = map[string]any{"entries": entries}
				}
				if graphQLQueryAsks(query, "id") {
					node["id"] = nodeID
				}
				nodes = append(nodes, node)
			}
		}
		f.readmeListedIDs = append(f.readmeListedIDs, listedIDs)
		data["nodes"] = nodes
	default:
		for _, field := range readmeContentFields.FindAllStringSubmatch(query, -1) {
			alias, idVariable, pathVariable := field[1], field[2], field[3]
			nodeID, idGiven := body.Variables[idVariable].(string)
			path, pathGiven := body.Variables[pathVariable].(string)
			if !idGiven || !pathGiven {
				io.WriteString(w, `{"data":null,"errors":[{"message":"Variable is used but not given"}]}`)
				return
			}
			readme, ok := readmeOf(nodeID)
			if !ok || path != "HEAD:"+readme.name {
				data[alias] = map[string]any{"object": nil}
				continue
			}
			file := map[string]any{}
			if graphQLQueryAsks(query, "isBinary") {
				file["isBinary"] = false
			}
			if graphQLQueryAsks(query, "text") {
				file["text"] = readme.content
			}
			data[alias] = map[string]any{"object": file}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
}

// One field of a README content query: its alias, the variable of the `node_id`, and the variable of the file path.
// It matches the query that `core.FetchReadmesByNodeIDs` builds
var readmeContentFields = regexp.MustCompile(`(\w+): node\(id: \$(\w+)\) \{ \.\.\. on Repository \{ object\(expression: \$(\w+)\)`)

// One folder field of a README listing query: its alias and the folder path in its expression (`HEAD:<path>`, empty for the root)
var readmeFolderFields = regexp.MustCompile(`(\w+): object\(expression: "HEAD:([^"]*)"\)`)

// Reports whether GraphQL query `query` asks for field `name` (a whole word that isn't an argument or a variable)
func graphQLQueryAsks(query string, name string) bool {
	return regexp.MustCompile(`[{\s]` + name + `[\s}]`).MatchString(query)
}

// Reports whether README query `query` declares in its head exactly the variables its body uses,
// each with the type GitHub expects (`$ids: [ID!]!`, `$id<n>: ID!`, `$path<n>: String!`).
// GitHub fails the whole query otherwise (docs).
func isEveryGraphQLVariableDeclared(query string) bool {
	head, body, _ := strings.Cut(query, "{")
	declared := map[string]string{} // Variable name -> declared type
	for _, m := range regexp.MustCompile(`\$(\w+): ([\w!\[\]]+)`).FindAllStringSubmatch(head, -1) {
		declared[m[1]] = m[2]
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$(\w+)`).FindAllStringSubmatch(body, -1) {
		used[m[1]] = true
		wantType := "String!"
		switch {
		case m[1] == "ids":
			wantType = "[ID!]!"
		case strings.HasPrefix(m[1], "id"):
			wantType = "ID!"
		}
		if declared[m[1]] != wantType {
			return false
		}
	}
	return len(used) == len(declared)
}

// A commit the fake GitHub created that isn't on the branch yet
type fakeCommit struct {
	tree    string // The commit's tree sha
	parent  string // Parent commit sha. Must equal the current `head` when the branch moves
	message string // Commit message
}

// Returns the files at a commit.
// Call it while holding `mu`.
//
// Args:
//   - `commit`: Commit sha. If it is the current `head`, these are the current files
//
// Returns:
//   - Path -> content. `nil` for an unknown commit
func (f *fakeGitHub) readFilesAt(commit string) map[string]string {
	if commit == f.head {
		return f.files
	}
	return f.snapshots[commit]
}

// Returns the files of a tree sha.
// It is `tree-of-<commit sha>` from reading the branch tip, or a tree made with the tree API (`trees`).
// Call it while holding `mu`.
func (f *fakeGitHub) readFilesOfTree(tree string) map[string]string {
	if files, ok := f.trees[tree]; ok {
		return files
	}
	if commit, ok := strings.CutPrefix(tree, "tree-of-"); ok { // A commit sha isn't a tree sha, so it isn't accepted as one
		return f.readFilesAt(commit)
	}
	return nil
}

// Moves the branch to a new commit.
// The files of the current `head` stay in `snapshots`, so reads at that commit still work.
// Call it while holding `mu`.
//
// Args:
//   - `next`: New commit sha
//   - `files`: Files of the new commit. Don't change them after passing them (they are stored as is, not copied)
func (f *fakeGitHub) moveHead(next string, files map[string]string) {
	f.parents[next] = f.head
	f.snapshots[f.head] = f.files
	f.head = next
	f.files = files
}

// Copies a file map, so that each commit keeps its own file state.
//
// Args:
//   - `files`: Path -> content
//
// NOTE: Assigning a Go map copies a reference to the same content (like a pointer in C++).
// Changing it without a copy would also change the files of past commits.
func copyFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	return out
}

// Returns the git blob sha of file content `content`, the value GitHub gives as the sha of a file.
// It is the SHA-1 of `blob <size>` and a NUL byte, followed by the content (docs)
func computeGitBlobSHA(content string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content))))
}

// Returns a `longStepsEndBy` (the time the long sync steps end by) far enough away that every sync step runs to the end in a test.
// The README step keeps time for its commit before it (`readmeCommitReserve`), so a near time would skip that step
func testLongStepsEndBy() time.Time { return time.Now().Add(time.Hour) }

// Makes a fake repository ID from the repository name (`owner/repo`).
// It is the CRC32 of the name, so the same name always gives the same ID
func makeRepoID(name string) int64 { return int64(crc32.ChecksumIEEE([]byte(name))) }

// Answers each request like GitHub, by path and method.
// Paths it doesn't imitate get 404.
// This method makes `fakeGitHub` satisfy the `http.Handler` interface, so it can be passed to `httptest.NewServer`.
func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	// A request with a refused token gets 401 "Bad credentials", whatever the path
	if f.tokenRejected && r.Header.Get("Authorization") == "Bearer "+testToken {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	const contents = "/repos/me/archive/contents/"
	const workflow = "/repos/me/archive/actions/workflows/starkive-sync.yml"
	switch {
	case strings.HasPrefix(r.URL.Path, "/repos/me/archive/actions/") && f.actionsDenied:
		w.Header().Set("X-Accepted-GitHub-Permissions", "actions=read")
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)

	case strings.HasPrefix(r.URL.Path, workflow) && f.files[core.SyncWorkflowPath] == "":
		// Without the workflow file, the Actions API doesn't know the workflow either
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)

	case r.URL.Path == workflow && r.Method == http.MethodGet:
		// Workflow state
		json.NewEncoder(w).Encode(map[string]any{"state": f.workflowState})

	case (r.URL.Path == workflow+"/enable" || r.URL.Path == workflow+"/disable") && r.Method == http.MethodPut:
		// Turn the workflow on or off.
		// Turning on a workflow that is already on is harmless (measured)
		f.workflowToggles++
		if strings.HasSuffix(r.URL.Path, "/disable") {
			if f.workflowState != "active" { // Disabling a disabled workflow gives 403 with the same header as a missing permission (measured)
				w.Header().Set("X-Accepted-GitHub-Permissions", "actions=write")
				http.Error(w, `{"message":"Unable to disable a workflow that is not active."}`, http.StatusForbidden)
				return
			}
			f.workflowState = "disabled_manually"
		} else {
			f.workflowState = "active"
		}
		w.WriteHeader(http.StatusNoContent)

	case r.URL.Path == workflow+"/dispatches" && r.Method == http.MethodPost:
		// Starting a run by hand.
		// A workflow that is off refuses with 422 (measured), and so does a ref other than the default branch.
		// With `return_run_details`, the answer has the new run's ID and page (measured)
		var body struct {
			Ref              string `json:"ref"`
			ReturnRunDetails bool   `json:"return_run_details"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.workflowState != "active" {
			http.Error(w, `{"message":"Cannot trigger a 'workflow_dispatch' on a disabled workflow"}`, http.StatusUnprocessableEntity)
			return
		}
		if body.Ref != "main" {
			http.Error(w, `{"message":"No ref found for: `+body.Ref+`"}`, http.StatusUnprocessableEntity)
			return
		}
		f.runs = append(f.runs, fakeRun{event: "workflow_dispatch", status: "queued", started: time.Now()})
		if !body.ReturnRunDetails {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		id := len(f.runs)
		fmt.Fprintf(w, `{"workflow_run_id":%d,"run_url":"https://api.github.com/repos/me/archive/actions/runs/%d","html_url":"https://github.com/me/archive/actions/runs/%d"}`, id, id, id)

	case r.URL.Path == workflow+"/runs" && r.Method == http.MethodGet:
		// Workflow run list, newest first, one page of `per_page` (`page` starts at 1), as GitHub answers (measured).
		// GitHub now and then answers an empty list to a query with an `event`, `status` or `created` filter (measured).
		// Here such a query always gets an empty list, so code that relies on a filter fails the tests
		query := r.URL.Query()
		runs := []map[string]any{}
		if !query.Has("event") && !query.Has("status") && !query.Has("created") {
			order := make([]int, len(f.runs))
			for i := range order {
				order[i] = i
			}
			slices.SortStableFunc(order, func(a int, b int) int { return f.runs[b].started.Compare(f.runs[a].started) })
			for _, i := range order {
				run := f.runs[i]
				runs = append(runs, map[string]any{"id": i + 1, "event": run.event, "status": run.status, "conclusion": run.conclusion,
					"run_started_at": run.started, "html_url": fmt.Sprintf("https://github.com/me/archive/actions/runs/%d", i+1)})
			}
		}
		total := len(runs)
		perPage, page := 30, 1 // GitHub's defaults (docs)
		fmt.Sscan(query.Get("per_page"), &perPage)
		fmt.Sscan(query.Get("page"), &page)
		runs = runs[min((page-1)*perPage, len(runs)):min(page*perPage, len(runs))]
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": runs})

	case strings.HasPrefix(r.URL.Path, contents) && r.Method == http.MethodGet:
		// File read (Contents API).
		// `ref` is a branch name or a commit sha
		if f.contentsHangup {
			conn, _, _ := w.(http.Hijacker).Hijack() // Cuts the connection without a response. `.(http.Hijacker)`: gets the interface value as another interface
			conn.Close()
			return
		}
		if f.repoEmpty { // A file read in an empty repository gives 404
			http.Error(w, `{"message":"This repository is empty."}`, http.StatusNotFound)
			return
		}
		ref := r.URL.Query().Get("ref")
		isBranch := ref == "main" || ref == "refs/heads/main"
		files := f.files // For the branch name (`main` or `refs/heads/main`), the current files
		if !isBranch {
			files = f.readFilesAt(ref)
		} else if previous, ok := f.snapshots[f.parents[f.head]]; f.branchReadsLag && ok {
			files = previous
		}
		path := strings.TrimPrefix(r.URL.Path, contents)
		f.fileReads = append(f.fileReads, path)
		if isBranch && path == f.branchReadNotFound {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		content, ok := files[path]
		// The `ETag` of a file is its blob sha, and a request with the same `If-None-Match` gets 304 with no body,
		// also at another commit and by branch name (measured)
		if etag := `"` + computeGitBlobSHA(content) + `"`; ok && r.Header.Get("If-None-Match") == etag {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		} else if ok {
			w.Header().Set("ETag", etag)
			f.fileDownloads = append(f.fileDownloads, path)
		}
		if !ok {
			// For a folder instead of a file, answers with a list like GitHub (here an empty array)
			for name := range files {
				if strings.HasPrefix(name, path+"/") {
					io.WriteString(w, "[]")
					return
				}
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		io.WriteString(w, content)

	case strings.HasPrefix(r.URL.Path, contents) && r.Method == http.MethodPut:
		// Write one file (Contents API).
		// The setup tool uses it for an empty repository's first commit (the marker).
		// In an empty repository, this commit becomes the first commit
		path := strings.TrimPrefix(r.URL.Path, contents)
		if f.workflowsDenied && strings.HasPrefix(path, ".github/workflows/") {
			w.Header().Set("X-Accepted-GitHub-Permissions", "contents=write; contents=write,workflows=write")
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
			return
		}
		var body struct {
			Message string `json:"message"`
			Content string `json:"content"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		decoded, _ := base64.StdEncoding.DecodeString(body.Content)
		files := copyFiles(f.files)
		if f.repoEmpty {
			files = map[string]string{}
			f.repoEmpty = false
		}
		files[path] = string(decoded)
		f.moveHead(fmt.Sprintf("commit-put-%d", len(f.commits)+1), files)
		f.recordCommit(body.Message)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)

	case r.URL.Path == fmt.Sprintf("/repositories/%d", testRepoID) && r.Method == http.MethodGet:
		// Look up a repository by ID. Returns the name and the default branch
		f.repoLookups++
		if f.repoMissing {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"full_name":"me/archive","private":true,"default_branch":"main"}`, testRepoID)

	case r.URL.Path == "/repos/me/archive/git/ref/heads/main" && r.Method == http.MethodGet:
		// Read the branch.
		// An empty repository gives 409 "Git Repository is empty." (measured)
		if f.repoEmpty {
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
			return
		}
		fmt.Fprintf(w, `{"object":{"sha":%q}}`, f.head)

	case r.URL.Path == "/repos/me/archive/commits" && r.Method == http.MethodGet:
		// List commits from a branch, newest first: the first entry is the branch tip with its tree (no changed files).
		// The tree sha is `tree-of-<commit sha>`, so a new tree based on it can find the commit's files.
		// It answers only the full branch name with one commit per page. A short name can also mean a tag
		if f.repoEmpty {
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
			return
		}
		if r.URL.Query().Get("sha") != "refs/heads/main" || r.URL.Query().Get("per_page") != "1" {
			http.Error(w, `{"message":"No commit found for SHA"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `[{"sha":%q,"commit":{"tree":{"sha":"tree-of-%s"}}}]`, f.head, f.head)

	case strings.HasPrefix(r.URL.Path, "/repos/me/archive/git/trees/") && r.Method == http.MethodGet:
		// List a folder at a commit (`<commit sha>:<folder path>`, `recursive=1`).
		// The branch named in full (`refs/heads/main`) works in place of the commit sha.
		// Paths are relative to the folder.
		// Only files are listed, because the server reads only files.
		// A folder with no file gives 404, which the server reads as an empty folder.
		// A request without `recursive=1` also gives 404, because the server always sends it
		f.treeListCalls++
		commitSHA, folderPath, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/me/archive/git/trees/"), ":")
		if commitSHA == "refs/heads/main" {
			commitSHA = f.head
		}
		entries := []string{}
		for path, content := range f.readFilesAt(commitSHA) {
			if relativePath, inFolder := strings.CutPrefix(path, folderPath+"/"); inFolder {
				entries = append(entries, fmt.Sprintf(`{"path":%q,"type":"blob","sha":%q}`, relativePath, computeGitBlobSHA(content)))
			}
		}
		if len(entries) == 0 || r.URL.Query().Get("recursive") != "1" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"tree":[%s],"truncated":false}`, strings.Join(entries, ","))

	case r.URL.Path == "/repos/me/archive/git/trees" && r.Method == http.MethodPost:
		// Create a tree: the received files overwrite the files of `base_tree`, and entries with a `null` sha delete theirs.
		// An empty repository gives 409, and writing a workflow without the Workflows permission gives 403 (measured)
		f.treeCalls++
		if f.treeStatus != 0 {
			http.Error(w, `{"message":"Invalid tree"}`, f.treeStatus)
			return
		}
		if f.repoEmpty {
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
			return
		}
		var body struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path    string          `json:"path"`
				Content *string         `json:"content"` // A pointer: `nil` if the key is missing
				SHA     json.RawMessage `json:"sha"`     // Raw JSON of the value, to tell `null` from a missing key
			} `json:"tree"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, e := range body.Tree {
			if f.workflowsDenied && strings.HasPrefix(e.Path, ".github/workflows/") {
				w.Header().Set("X-Accepted-GitHub-Permissions", "contents=write; contents=write,workflows=write")
				http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
				return
			}
		}
		base := f.readFilesOfTree(body.BaseTree)
		if base == nil { // A base tree that doesn't exist is refused
			http.Error(w, `{"message":"GitRPC::BadObjectState"}`, http.StatusUnprocessableEntity)
			return
		}
		files := copyFiles(base)
		for _, e := range body.Tree {
			switch {
			case e.Content != nil:
				files[e.Path] = *e.Content
			case string(e.SHA) != "null": // A blob made earlier (`/git/blobs`), by its sha
				var blobSHA string
				json.Unmarshal(e.SHA, &blobSHA)
				content, ok := f.blobs[blobSHA]
				if !ok {
					http.Error(w, `{"message":"BadObjectState"}`, http.StatusUnprocessableEntity)
					return
				}
				files[e.Path] = content
			default:
				if _, ok := files[e.Path]; !ok { // Deleting a path that isn't there fails the whole tree (measured)
					http.Error(w, `{"message":"GitRPC::BadObjectState"}`, http.StatusUnprocessableEntity)
					return
				}
				delete(files, e.Path)
			}
		}
		if maps.Equal(files, base) { // Same files as the base tree give the same tree sha, like git
			fmt.Fprintf(w, `{"sha":%q}`, body.BaseTree)
			return
		}
		sha := fmt.Sprintf("tree-%d", len(f.trees)+1)
		f.trees[sha] = files
		fmt.Fprintf(w, `{"sha":%q}`, sha)

	case strings.HasPrefix(r.URL.Path, "/repos/me/archive/git/blobs/") && r.Method == http.MethodGet:
		// Read a blob by its sha, in raw format: any file content of a commit, a made tree, or a made blob
		f.blobReads++
		sha := strings.TrimPrefix(r.URL.Path, "/repos/me/archive/git/blobs/")
		for _, files := range slices.Concat([]map[string]string{f.files, f.blobs}, slices.Collect(maps.Values(f.snapshots)), slices.Collect(maps.Values(f.trees))) {
			for _, content := range files {
				if computeGitBlobSHA(content) == sha {
					io.WriteString(w, content)
					return
				}
			}
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)

	case r.URL.Path == "/repos/me/archive/git/blobs" && r.Method == http.MethodPost:
		// Create a blob from base64 content.
		// Its sha is the git blob sha of the decoded bytes
		var body struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		content, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil || body.Encoding != "base64" {
			http.Error(w, `{"message":"Invalid blob"}`, http.StatusUnprocessableEntity)
			return
		}
		sha := computeGitBlobSHA(string(content))
		f.blobs[sha] = string(content)
		fmt.Fprintf(w, `{"sha":%q}`, sha)

	case r.URL.Path == "/repos/me/archive/git/commits" && r.Method == http.MethodPost:
		// Create a commit.
		// It isn't on the branch yet
		var body struct {
			Message string   `json:"message"`
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sha := fmt.Sprintf("commit-%d", len(f.pending)+len(f.snapshots)+1)
		f.pending[sha] = fakeCommit{tree: body.Tree, parent: body.Parents[0], message: body.Message}
		fmt.Fprintf(w, `{"sha":%q}`, sha)

	case r.URL.Path == "/repos/me/archive/git/refs/heads/main" && r.Method == http.MethodPatch:
		// Move the branch (no force).
		// If the commit's parent isn't the current `head`, it gives 422 "Update is not a fast forward"
		var body struct {
			SHA string `json:"sha"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.refStatus != 0 {
			http.Error(w, fmt.Sprintf(`{"message":%q}`, f.refMessage), f.refStatus)
			return
		}
		if f.interfereAll { // Another device always commits first
			f.moveHead(f.head+"-other", copyFiles(f.files))
		}
		if f.interfereOnce != nil {
			// Another device committed once first, so the branch moved
			files := copyFiles(f.files)
			f.interfereOnce(files)
			f.interfereOnce = nil
			f.moveHead(f.head+"-other", files)
		}
		commit := f.pending[body.SHA]
		if commit.parent != f.head {
			http.Error(w, `{"message":"Update is not a fast forward"}`, http.StatusUnprocessableEntity)
			return
		}
		f.moveHead(body.SHA, f.trees[commit.tree])
		f.recordCommit(commit.message)
		if f.refLostReply { // The move was applied, and only the response was lost
			if f.otherAfterLostReply {
				f.moveHead(f.head+"-other", copyFiles(f.files))
			}
			http.Error(w, `{"message":"Bad Gateway"}`, http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"object":{"sha":%q}}`, body.SHA)

	case strings.HasPrefix(r.URL.Path, "/repos/me/archive/compare/") && r.Method == http.MethodGet:
		// Compare two commits (`base...tip`).
		// If following parents from tip reaches base, tip contains base (`ahead`, or `identical` for the same commit)
		base, tip, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/me/archive/compare/"), "...")
		status := "diverged"
		for c := tip; c != ""; c = f.parents[c] {
			if c == base && c == tip {
				status = "identical"
				break
			}
			if c == base {
				status = "ahead"
				break
			}
		}
		fmt.Fprintf(w, `{"status":%q}`, status)

	case r.URL.Path == "/login/oauth/access_token" && r.Method == http.MethodPost:
		// Exchange a sign-in code for a user token.
		// Like GitHub, a failure also answers 200 with the reason in `error`.
		// The code `user-<ID>` becomes that user's token `token_<ID>`
		r.ParseForm()
		var id int64
		switch {
		case r.Form.Get("client_id") != testClientID || r.Form.Get("client_secret") != testClientSecret:
			io.WriteString(w, `{"error":"incorrect_client_credentials"}`)
		case func() bool { _, err := fmt.Sscanf(r.Form.Get("code"), "user-%d", &id); return err == nil }():
			fmt.Fprintf(w, `{"access_token":"token_%d","token_type":"bearer","scope":""}`, id)
		default:
			io.WriteString(w, `{"error":"bad_verification_code"}`)
		}

	case r.URL.Path == "/user" && r.Method == http.MethodGet:
		// The token's owner.
		// `token_<ID>` belongs to the user with that ID.
		// Other tokens get 401
		if r.Header.Get("Authorization") == "Bearer "+testToken {
			f.ownerCalls++
			if f.ownerDown {
				http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
				return
			}
		}
		var id int64
		if _, err := fmt.Sscanf(r.Header.Get("Authorization"), "Bearer token_%d", &id); err != nil {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"login":"user%d"}`, id, id)

	case r.URL.Path == "/repos/torvalds/linux":
		// One repository to look up when adding a repository directly (Add)
		io.WriteString(w, `{"id":2325298,"node_id":"R_2325298","full_name":"torvalds/linux"}`)

	case r.URL.Path == "/user/starred" && r.Method == http.MethodGet:
		// One page of my Stars (`per_page`, `page`)
		f.starredPageReads++
		if f.starringDenied {
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
			return
		}
		page := 1 // Like GitHub, the first page if `page` is missing
		var perPage int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		fmt.Sscan(r.URL.Query().Get("per_page"), &perPage)
		if f.starredFailFrom != 0 && page >= f.starredFailFrom {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
			return
		}
		items := []string{}
		for i := (page - 1) * perPage; i < min(page*perPage, len(f.starred)); i++ {
			name := f.starred[i]
			items = append(items, fmt.Sprintf(`{"starred_at":"2026-10-01T00:00:00Z","repo":{"id":%d,"node_id":"R_%d","full_name":%q,"stargazers_count":7}}`,
				makeRepoID(name), makeRepoID(name), name))
		}
		// Like GitHub, if it isn't the last page, the `Link` header gives the last page number
		lastPage := max(1, (len(f.starred)+perPage-1)/perPage)
		if page < lastPage {
			w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/user/starred?per_page=%d&page=%d>; rel="next", <https://api.github.com/user/starred?per_page=%d&page=%d>; rel="last"`,
				perPage, page+1, perPage, lastPage))
		}
		io.WriteString(w, "["+strings.Join(items, ",")+"]")

	case r.URL.Path == "/graphql":
		// GraphQL: the DB repository state (`isEmpty` and the marker), the GitHub star list query (`viewer`), and repository info by `node_id` (`nodes`)
		if f.graphqlDown {
			status := f.graphqlDownStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			http.Error(w, `{"message":"Server Error"}`, status)
			return
		}
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				IDs    []string `json:"ids"`
				Owner  string   `json:"owner"`
				Name   string   `json:"name"`
				Marker string   `json:"marker"`
			} `json:"variables"`
		}
		rawBody, _ := io.ReadAll(r.Body)
		json.Unmarshal(rawBody, &body)
		if strings.Contains(body.Query, `"HEAD:.github"`) || strings.Contains(body.Query, "expression: $path") {
			f.answerReadmeQuery(w, body.Query, rawBody)
			return
		}
		if strings.Contains(body.Query, "isEmpty") {
			// DB repository state: `isEmpty` for an empty repository.
			// Otherwise the file at `HEAD:<path>` is answered as `marker`.
			// A missing file is `null`
			if body.Variables.Owner+"/"+body.Variables.Name != "me/archive" {
				io.WriteString(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository"}]}`)
				return
			}
			marker := "null"
			if content, ok := f.files[strings.TrimPrefix(body.Variables.Marker, "HEAD:")]; ok && !f.repoEmpty {
				text, _ := json.Marshal(content)
				marker = `{"text":` + string(text) + `}`
			}
			fmt.Fprintf(w, `{"data":{"repository":{"isEmpty":%t,"marker":%s}}}`, f.repoEmpty, marker)
			return
		}
		if strings.Contains(body.Query, "viewer") {
			if f.githubStarListsDown {
				http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
				return
			}
			// GitHub star lists and their repositories.
			// Go's map order changes each time, so the answer is sorted by name to stay the same
			names := make([]string, 0, len(f.githubStarLists))
			for name := range f.githubStarLists {
				names = append(names, name)
			}
			slices.Sort(names)
			githubStarLists := []string{}
			for i, name := range names {
				items := []string{}
				for _, repo := range f.githubStarLists[name] {
					items = append(items, fmt.Sprintf(`{"databaseId":%d}`, makeRepoID(repo)))
				}
				githubStarLists = append(githubStarLists, fmt.Sprintf(`{"id":"UL_%d","name":%q,"items":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}}`,
					i, name, strings.Join(items, ",")))
			}
			fmt.Fprintf(w, `{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}}}}`, strings.Join(githubStarLists, ","))
			return
		}
		f.metaQueries++
		if f.metaRateLimitFrom != 0 && f.metaQueries >= f.metaRateLimitFrom {
			// The measured message. The `Retry-After` header is the documented way to give the wait
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, http.StatusForbidden)
			return
		}
		// Repository info: `R_1` and the repositories in my Stars (`R_<ID>`) get their info.
		// `gone` gets `null` with a `NOT_FOUND` error,
		// `R_3` gets an ID that isn't a repository (`{}`), and everything else gets `null`
		nodes := []string{}
		errs := []string{}
		for i, id := range body.Variables.IDs {
			node := "null"
			switch {
			case f.gone[id]:
				errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["nodes",%d],"message":"Could not resolve to a node"}`, i))
			case id == "R_1":
				node = `{"id":"R_1","nameWithOwner":"a/one","stargazerCount":42}`
			case id == "R_3":
				node = "{}" // An ID of another kind, not a repository
			default:
				for _, name := range f.starred {
					if id == fmt.Sprintf("R_%d", makeRepoID(name)) {
						node = fmt.Sprintf(`{"id":%q,"nameWithOwner":%q,"description":"about %s","primaryLanguage":{"name":"Go","color":"#00ADD8"},"repositoryTopics":{"nodes":[{"topic":{"name":"cli"}}]},"homepageUrl":"","licenseInfo":{"spdxId":"MIT"},"stargazerCount":7,"isArchived":false}`, id, name, name)
					}
				}
			}
			nodes = append(nodes, node)
		}
		fmt.Fprintf(w, `{"data":{"nodes":[%s]},"errors":[%s]}`, strings.Join(nodes, ","), strings.Join(errs, ","))

	default: // A path it doesn't imitate
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// Reads one file at the branch tip.
// Tests use it to check what the server wrote.
//
// Args:
//   - `path`: Path in the repository
//
// Returns:
//   - The file content. An empty string if the file doesn't exist
func (f *fakeGitHub) readFile(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[path]
}

// Returns the archive items of all shards at the branch tip as one JSON array, one item per line, shard by shard (`joinStarItems`).
// Tests use it to check what the server wrote.
func (f *fakeGitHub) readStarItems() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return joinStarItems(f.files)
}

// Puts archive items `content` (a JSON array, one item per line) into `files` as the shards, split by ID like the server.
// Each item keeps its line as it is, and every shard is written (an empty one as `[]`).
// An empty string means no item.
// Content that isn't a JSON array goes into shard 00 as it is, and the other shards are empty.
//
// NOTE: A test about the items, not about how they are stored, puts and reads them only through three helpers.
// They are `putStarItems`, `joinStarItems` and `fakeGitHub.readStarItems`.
// So a change of the storage layout changes only these helpers and the tests about the layout itself.
func putStarItems(files map[string]string, content string) {
	lines := make([][]string, core.StarItemShardCount) // Item lines of each shard
	var raws []json.RawMessage                         // `json.RawMessage`: the bytes of each element as they are
	if content != "" {
		if err := json.Unmarshal([]byte(content), &raws); err != nil {
			for index := range core.StarItemShardCount {
				files[core.BuildStarItemShardPath(index)] = "[]\n"
			}
			files[core.BuildStarItemShardPath(0)] = content
			return
		}
	}
	for _, raw := range raws {
		var item struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(raw, &item)
		lines[shardIndexOf(item.ID)] = append(lines[shardIndexOf(item.ID)], string(raw))
	}
	for index := range core.StarItemShardCount {
		files[core.BuildStarItemShardPath(index)] = joinJSONLines(lines[index])
	}
}

// Returns the archive items of all shards in `files` as one JSON array, one item per line, shard by shard in index order.
// It is the form `putStarItems` takes.
// A shard that isn't a JSON array is put in as one line as it is.
func joinStarItems(files map[string]string) string {
	lines := []string{}
	for index := range core.StarItemShardCount {
		content := files[core.BuildStarItemShardPath(index)]
		var raws []json.RawMessage
		if err := json.Unmarshal([]byte(content), &raws); err != nil {
			lines = append(lines, content)
			continue
		}
		for _, raw := range raws {
			lines = append(lines, string(raw))
		}
	}
	return joinJSONLines(lines)
}

// Joins JSON values, one per line, into a JSON array in the data file format (`[]` and a line break if there are none)
func joinJSONLines(lines []string) string {
	if len(lines) == 0 {
		return "[]\n"
	}
	return "[\n" + strings.Join(lines, ",\n") + "\n]\n"
}

// Returns the index of the shard that holds the item with ID `id`, like the server (`id % core.StarItemShardCount`)
func shardIndexOf(id int64) int { return int(id % int64(core.StarItemShardCount)) }

// Reports whether the names that appear in `order` keep that order inside each shard of `archiveNames` (the archive in shard order).
// The shard of a name comes from its fake repository ID (`makeRepoID`). Names not in `order` are skipped
func isInShardOrder(archiveNames []string, order []string) bool {
	last := make([]int, core.StarItemShardCount) // Position in `order` of the last name seen in each shard
	for i := range last {
		last[i] = -1
	}
	for _, name := range archiveNames {
		position := slices.Index(order, name)
		if position < 0 {
			continue
		}
		shard := shardIndexOf(makeRepoID(name))
		if position < last[shard] {
			return false
		}
		last[shard] = position
	}
	return true
}

// Dashboard address that test requests arrive at.
// `call` sends requests to `http://example.test`.
// The server (`core.ReadAPIRequestSiteURL`) treats any address that isn't localhost as https, so this becomes the address
const testDashboardURL = "https://example.test"

// Starts a fake GitHub with a DB repository that finished setup, and sets the environment variables.
// On top of the repository from `setupWithoutGitHubApp`,
// the config file has the sign-in GitHub App (encrypted with the server token, owned by `testUserID`) and the dashboard address (`testDashboardURL`).
//
// Args:
//   - `starItems`: The archive items as one JSON array, put in with `putStarItems`. An empty string means no item
//   - `starLists`: Content of `db/star-lists.json`. An empty string means no file
//
// Returns:
//   - The fake GitHub. Tests change its fields to set GitHub's behavior, and check the results (files, commits, request counts)
func setup(t *testing.T, starItems string, starLists string) *fakeGitHub {
	gh := setupWithoutGitHubApp(t, starItems, starLists)
	gh.files[core.ConfigDBPath] = makeTestConfigFile(testDashboardURL, encodeTestGitHubApp(t, testToken, testUserID))
	return gh
}

// Number of fake GitHub servers started so far.
// It gives each test's server address its own path (`setupWithoutGitHubApp`)
var fakeGitHubServers atomic.Int64

// Starts a fake GitHub with a DB repository that has no sign-in GitHub App yet (before the setup tool creates the App), and sets the environment variables.
// The repository has the marker for this version (`.starkive`),
// a config file with the auto sync interval and a time turned on one hour ago, and a sync workflow file that is on.
// To test with an empty or foreign repository, change `files` or turn on `repoEmpty` after calling it.
//
// Args:
//   - `starItems`: The archive items as one JSON array, put in with `putStarItems`. An empty string means no item
//   - `starLists`: Content of `db/star-lists.json`. An empty string means no file
//
// Returns:
//   - The fake GitHub
//
// NOTE: When the test ends, it closes the fake server and checks the subject format of every commit made in that test (`checkCommitSubjects`).
// The environment variables are changed with `t.Setenv`, so they go back when the test ends.
func setupWithoutGitHubApp(t *testing.T, starItems string, starLists string) *fakeGitHub {
	gh := &fakeGitHub{files: map[string]string{}, head: "commit-0", snapshots: map[string]map[string]string{},
		trees: map[string]map[string]string{}, pending: map[string]fakeCommit{}, parents: map[string]string{},
		readmes: map[int64]fakeReadme{}, blobs: map[string]string{}, workflowState: "active"}
	putStarItems(gh.files, starItems)
	if starLists != "" {
		gh.files[core.StarListsDBPath] = starLists
	}
	gh.files[core.MarkerDBPath] = string(core.EncodeDBRepoMarker())
	gh.files[core.ConfigDBPath] = makeTestConfigFile("", "")
	gh.files[core.SyncWorkflowPath] = "name: starkive sync\n" // The server doesn't read the workflow content. It only checks the state with the Actions API
	// The server code caches values by the GitHub address and the token (`core/cache.go`), and every test uses the same token.
	// A closed test server's port can be given again to a later test (seen on Windows), which would then get the earlier test's cached values.
	// So each test's address gets its own path, which `http.StripPrefix` removes before the fake GitHub sees the request
	pathPrefix := fmt.Sprintf("/test%d", fakeGitHubServers.Add(1))
	srv := httptest.NewServer(http.StripPrefix(pathPrefix, gh))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { checkCommitSubjects(t, gh) }) // When the test ends, checks the format of the commit subjects made so far
	// Makes the server code call the fake GitHub.
	// `t.Setenv` restores the original value when the test ends
	t.Setenv("STARKIVE_GH_API_URL", srv.URL+pathPrefix)
	t.Setenv("STARKIVE_GH_WEB_URL", srv.URL+pathPrefix)
	t.Setenv(core.DBRepoAccessKeyEnv, core.FormatDBRepoAccessKey(testRepoID, testToken))
	return gh
}

// Format of DB repository commit subjects: `<who>(<file>): <summary>`.
// Who is `user` (a person's edit) or `sync` (a sync).
// File is the DB repository file that the commit changed, or the folder `readmes` (`core.ReadmesDBDir`)
var commitSubjectRule = regexp.MustCompile(`^(user|sync)\((star-items|star-lists|repo-meta|readmes|config|workflow)\): \S`)

// Reports a test failure for each commit made on the fake GitHub `gh` during the test whose subject breaks the format (`commitSubjectRule`).
// `setupWithoutGitHubApp` calls it when the test ends.
//
// NOTE: Each write path in the server sets its own commit subject.
// This check runs in every test, so if any path breaks the format, the tests that go through that path fail.
func checkCommitSubjects(t *testing.T, gh *fakeGitHub) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, subject := range gh.badCommitSubjects {
		t.Errorf("commit subject breaks the rule: %q", subject)
	}
}

// Makes the content of the config file (`config.json`).
// The auto sync interval is daily (1), and sync was turned on one hour ago (`enabled_since`).
//
// Args:
//   - `dashboardURL`: Dashboard address. Left out if empty
//   - `app`: Sign-in GitHub App settings (the `sign_in_app` value, made by `encodeTestGitHubApp` and others). Left out if empty
func makeTestConfigFile(dashboardURL string, app string) string {
	content := `{`
	if dashboardURL != "" {
		content += `"dashboard_url":"` + dashboardURL + `",`
	}
	content += `"sync":{"interval_days":1,"enabled_since":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`
	if app != "" {
		content += `,"sign_in_app":` + app
	}
	return content + "}\n"
}

// Makes sign-in GitHub App settings (the `sign_in_app` value) encrypted with the server token.
// The App owner is the token owner (`testUserID`).
//
// Args:
//   - `siteURL`: Dashboard address registered in the App (`site_url`)
func encodeTestGitHubAppAt(t *testing.T, siteURL string) string {
	content, err := core.EncodeGitHubAppConfigValue(&core.EnvConfig{DBRepoAccessToken: testToken},
		core.GitHubApp{AppID: 1, Slug: "starkive-test", OwnerID: testUserID, ClientID: testClientID, ClientSecret: testClientSecret, SiteURL: siteURL})
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// Makes sign-in GitHub App settings (the `sign_in_app` value).
// The address registered in the App is `testDashboardURL`.
//
// Args:
//   - `token`: Token used to encrypt the client secret. If it differs from the server token, the server can't decrypt the settings (as after a token change)
//   - `ownerID`: GitHub user ID of the App owner
func encodeTestGitHubApp(t *testing.T, token string, ownerID int64) string {
	content, err := core.EncodeGitHubAppConfigValue(&core.EnvConfig{DBRepoAccessToken: token},
		core.GitHubApp{AppID: 1, Slug: "starkive-test", OwnerID: ownerID, ClientID: testClientID, ClientSecret: testClientSecret, SiteURL: testDashboardURL})
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// Calls an API entry function directly without an HTTP server, and returns the response.
// The request goes to `http://example.test` with a same-site `Origin`.
//
// Args:
//   - `h`: API entry function (`api.HandleArchiveStarItems`, etc.)
//   - `target`: Path and query (e.g. `/api/archive-star-items?id=1`)
//   - `body`: Request body. No body if empty
//   - `cookie`: Value of the `Cookie` header. Not sent if empty
//
// Returns:
//   - The recorded response (status code, headers, body)
func call(h http.HandlerFunc, method string, target string, body string, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.test"+target, strings.NewReader(body))
	req.Header.Set("Origin", origin)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Imitates the whole GitHub sign-in and gets a session cookie for the token owner (`testUserID`).
// Stops the test on failure.
//
// Returns:
//   - The session cookie `session=<value>`. Pass it as `cookie` to `call`
func login(t *testing.T) string {
	state, stateCookie := startGitHubLogin(t)
	rec := callGitHubLoginCallback(fmt.Sprintf("code=user-%d&state=%s", testUserID, state), stateCookie)
	session := readSetCookie(rec, "session")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" || session == "" {
		t.Fatalf("login failed: %d %s %q", rec.Code, rec.Header().Get("Location"), rec.Header().Values("Set-Cookie"))
	}
	return session
}

// Starts GitHub sign-in (`POST /api/login-session`).
// Stops the test on failure.
//
// Returns:
//   - The state in the GitHub authorization address
//   - The state cookie the browser got (`github_login_state=<value>`)
func startGitHubLogin(t *testing.T) (string, string) {
	rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", "")
	var body struct {
		URL string `json:"url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	u, err := url.Parse(body.URL)
	if rec.Code != 200 || err != nil || u.Query().Get("state") == "" {
		t.Fatalf("start login: %d %s", rec.Code, rec.Body)
	}
	return u.Query().Get("state"), readSetCookie(rec, "github_login_state")
}

// Imitates the request after GitHub finishes sign-in and sends the browser back to the callback (`GET /api/login-session?...`).
//
// Args:
//   - `query`: Query of the callback address (`code=...&state=...` or `error=...`)
//   - `cookie`: The state cookie the browser sends. Not sent if empty
//
// Returns:
//   - The server's response (a 302 to the page, on success or failure)
func callGitHubLoginCallback(query string, cookie string) *httptest.ResponseRecorder {
	return call(api.HandleLoginSession, "GET", "/api/login-session?"+query, "", cookie)
}

// Finds the cookie named `name` with a value among the `Set-Cookie` headers of the response `rec`.
//
// Returns:
//   - `name=value`. An empty string if the cookie is missing, or if there is only a `Set-Cookie` with an empty value (one that deletes the cookie)
func readSetCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, header := range rec.Header().Values("Set-Cookie") {
		pair := strings.SplitN(header, ";", 2)[0]
		if strings.HasPrefix(pair, name+"=") && pair != name+"=" {
			return pair
		}
	}
	return ""
}

// Two archive items for tests, in the file format (one item per line).
// a/one belongs to Graphics (100), and b/two has no star list
const sampleStarItems = "[\n" +
	`{"id":1,"node_id":"R_1","name":"a/one","note":"","star_list_ids":[100],"starred_at":"2026-01-01T00:00:00Z"}` + ",\n" +
	`{"id":2,"node_id":"R_2","name":"b/two","note":"","star_list_ids":[],"starred_at":"2026-01-01T00:00:00Z"}` +
	"\n]\n"

// Two star lists for tests: Graphics (100), HPE (200)
const sampleStarLists = "[\n" +
	`{"id":100,"name":"Graphics","description":""}` + ",\n" +
	`{"id":200,"name":"HPE","description":"pose"}` +
	"\n]\n"

// Tests success and failure of GitHub sign-in (web flow).
//   - The authorization address has only the App's Client ID and the state.
//     It doesn't send permissions (`scope`) or a callback address (`redirect_uri`)
//   - For the token owner, a session cookie is made and the state cookie is deleted. The session can use the APIs
//   - These cases redirect to `/?login_error=<reason>` without a session:
//     another user, a different or missing state, a forged state cookie, an expired state, cancel on GitHub, a wrong code
//   - Sign-in can't be started from another site
func TestGitHubLogin(t *testing.T) {
	ctx := context.Background()
	setup(t, sampleStarItems, sampleStarLists)

	// Authorization address: only the App's Client ID and the state
	rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", "")
	var start struct {
		URL string `json:"url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &start)
	u, _ := url.Parse(start.URL)
	web, _ := url.Parse(os.Getenv("STARKIVE_GH_WEB_URL"))
	if u.Path != web.Path+"/login/oauth/authorize" || u.Query().Get("client_id") != testClientID || u.Query().Has("scope") || u.Query().Has("redirect_uri") {
		t.Errorf("authorize url = %s", start.URL)
	}

	// Allowed user: a session is made and can use the APIs.
	// The state cookie is deleted
	state, stateCookie := startGitHubLogin(t)
	rec = callGitHubLoginCallback(fmt.Sprintf("code=user-%d&state=%s", testUserID, state), stateCookie)
	session := readSetCookie(rec, "session")
	if rec.Header().Get("Location") != "/" || session == "" || !slices.ContainsFunc(rec.Header().Values("Set-Cookie"), func(h string) bool { return strings.HasPrefix(h, "github_login_state=;") }) {
		t.Fatalf("allowed user: %d %s %q", rec.Code, rec.Header().Get("Location"), rec.Header().Values("Set-Cookie"))
	}
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", session); rec.Code != 200 {
		t.Errorf("session from GitHub login: %d", rec.Code)
	}

	// Failing cases: never a session cookie.
	// Redirect to the page with a reason.
	// Expired state: the state and cookie of a sign-in started 11 minutes ago (the state cookie is valid for only 10 minutes)
	expiredState, expiredCookie := func() (string, string) {
		repo := openTestDBRepo(t)
		app, _, err := core.LoadGitHubApp(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		authorizeURL, cookie, _ := core.StartGitHubLogin(repo.EnvConfig(), app, time.Now().Add(-11*time.Minute))
		u, _ := url.Parse(authorizeURL)
		return u.Query().Get("state"), strings.SplitN(cookie, ";", 2)[0]
	}()
	freshState, freshCookie := startGitHubLogin(t)
	otherState, otherCookie := startGitHubLogin(t)
	tamperedCookie := "github_login_state=" + otherState + strings.TrimPrefix(freshCookie, "github_login_state="+freshState) // Forged cookie: another state with this cookie's expiry and signature
	cases := []struct {
		name   string
		query  string
		cookie string
		want   string
	}{
		{"other GitHub user", fmt.Sprintf("code=user-%d&state=%s", otherUserID, freshState), freshCookie, "not_allowed"},
		{"state differs from cookie", fmt.Sprintf("code=user-%d&state=%s", testUserID, otherState), freshCookie, "expired"},
		{"no state cookie", fmt.Sprintf("code=user-%d&state=%s", testUserID, freshState), "", "expired"},
		{"no state in callback", fmt.Sprintf("code=user-%d", testUserID), freshCookie, "expired"},
		{"forged state cookie", fmt.Sprintf("code=user-%d&state=%s", testUserID, otherState), tamperedCookie, "expired"},
		{"state cookie expired", fmt.Sprintf("code=user-%d&state=%s", testUserID, expiredState), expiredCookie, "expired"},
		{"cancelled on GitHub", "error=access_denied&state=" + otherState, otherCookie, "cancelled"},
		{"bad code", "code=nonsense&state=" + otherState, otherCookie, "failed"},
	}
	for _, c := range cases {
		rec := callGitHubLoginCallback(c.query, c.cookie)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/?login_error="+c.want || readSetCookie(rec, "session") != "" {
			t.Errorf("%s: %d %s %q", c.name, rec.Code, rec.Header().Get("Location"), rec.Header().Values("Set-Cookie"))
		}
	}

	// Another site can't start a sign-in
	req := httptest.NewRequest("POST", "http://example.test/api/login-session", nil)
	req.Header.Set("Origin", "https://evil.test")
	cross := httptest.NewRecorder()
	api.HandleLoginSession(cross, req)
	if cross.Code != 403 {
		t.Errorf("cross-origin start: %d", cross.Code)
	}
}

// Tests that nobody can sign in when the token owner can't be checked.
// There must be no path where "unknown" means "allow anyone".
//   - If looking up the token owner (`/user`) fails during the callback, it redirects with `failed` and no session
//   - The allow check (`core.IsGitHubLoginAllowed`) also allows nobody when the token owner is unknown (0)
func TestGitHubLoginFailsClosedWithoutTokenOwner(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	state, stateCookie := startGitHubLogin(t)
	gh.mu.Lock()
	gh.ownerDown = true
	gh.mu.Unlock()
	rec := callGitHubLoginCallback(fmt.Sprintf("code=user-%d&state=%s", testUserID, state), stateCookie)
	if rec.Header().Get("Location") != "/?login_error=failed" || readSetCookie(rec, "session") != "" {
		t.Errorf("owner lookup failed: %d %s %q", rec.Code, rec.Header().Get("Location"), rec.Header().Values("Set-Cookie"))
	}
	// The check also allows nobody when the token owner is unknown (0)
	if core.IsGitHubLoginAllowed(0, 0) || core.IsGitHubLoginAllowed(testUserID, otherUserID) || !core.IsGitHubLoginAllowed(testUserID, testUserID) {
		t.Error("IsGitHubLoginAllowed decided wrong")
	}
}

// Tests parsing of the environment variable `STARKIVE_DB_REPO_ACCESS_KEY` (`starkive-v1:<repository ID>:<token>`).
//   - With a valid value, it asks GitHub by ID and opens the repository with its current name and default branch.
//     It caches the result, so the next request doesn't ask again
//   - Leading and trailing spaces and newlines (picked up when copying) are ignored
//   - If it is missing or malformed, every API gives 500 (checked on two of them).
//     (Malformed: empty, only the token by mistake, another format version, a name instead of an ID, a negative number,
//     characters a token can't have, header injection)
//   - If the token can't see the repository (the token doesn't select it), it gives 409 and doesn't guess another repository
func TestDBRepoAccessKey(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo, err := core.OpenDBRepoFromEnv(ctx)
	if err != nil || repo.EnvConfig().DBRepoID != testRepoID || repo.EnvConfig().DBRepoAccessToken != testToken || repo.FullName() != "me/archive" || repo.Branch() != "main" {
		t.Fatalf("opened: %+v %v", repo, err)
	}
	cookie := login(t)
	before := gh.repoLookups
	for i := 0; i < 3; i++ {
		if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie); rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
	}
	if gh.repoLookups != before {
		t.Errorf("looked the DB repository up again: %d times", gh.repoLookups-before)
	}

	// Spaces and newlines picked up when copying the printed line are ignored
	for _, value := range []string{
		" starkive-v1:5150:token_4242",
		"starkive-v1:5150:token_4242 ",
		"starkive-v1:5150:token_4242\n",
		"\tstarkive-v1:5150:token_4242\r\n",
	} {
		t.Setenv(core.DBRepoAccessKeyEnv, value)
		if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie); rec.Code != 200 {
			t.Errorf("%q: %d %s", value, rec.Code, rec.Body)
		}
	}

	// Malformed values
	for _, value := range []string{
		"",
		testToken,
		"github_pat_11ABCDEF",
		"starkive-v1:5150",
		"starkive-v2:5150:token_4242",
		"starkive-v1:me/archive:token_4242",
		"starkive-v1:-1:token_4242",
		"starkive-v1:5150:token-4242",
		"starkive-v1:5150:token_4242\r\nX: y",
		"starkive-v1:5150:token_4242 extra", // Spaces inside the value aren't removed
	} {
		t.Setenv(core.DBRepoAccessKeyEnv, value)
		for _, rec := range []*httptest.ResponseRecorder{
			call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie),
			call(api.HandleLoginSession, "POST", "/api/login-session", "", ""),
		} {
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "missing or malformed") {
				t.Errorf("%q: %d %s", value, rec.Code, rec.Body)
			}
		}
	}

	// If the token can't see the repository (the token doesn't select it), it reports 409 and doesn't guess the repository
	gh = setup(t, sampleStarItems, sampleStarLists)
	gh.repoMissing = true
	if _, err := core.OpenDBRepoFromEnv(ctx); !errors.Is(err, core.ErrDBRepoNotFound) {
		t.Errorf("repository not visible: %v", err)
	}
}

// Tests that the server writes nothing to a repository that isn't a starkive repository
// (a foreign repository without the marker, or an empty repository not prepared yet).
// Opening the DB repository (`core.OpenDBRepoFromEnv`), the scheduled sync, Sync now, adding directly and turning off sync must all give 409,
// with the guard's message (`core.OpenDBRepo`), so none of them is refused for another reason.
// There must be no commits and no requests to turn the workflow on or off.
//
// NOTE: The test uses a live session.
// It checks that a session made before the token's repository selection changed can't write to the new repository.
func TestNothingIsWrittenToUnpreparedRepo(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		prepare func(gh *fakeGitHub)
		want    *core.HTTPError
	}{
		{"another repository", func(gh *fakeGitHub) { gh.files = map[string]string{"README.md": "# my project\n"} }, core.ErrDBRepoNotStarkive},
		{"empty repository", func(gh *fakeGitHub) { gh.files = map[string]string{}; gh.repoEmpty = true }, core.ErrDBRepoNotSetUp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := setupWithoutGitHubApp(t, "", "")
			gh.starred = []string{"a/one"}
			c.prepare(gh)
			// A live session.
			// This repository has no App settings, so sign-in isn't possible.
			// The session cookie is made directly, signed with the server token
			cookie := strings.SplitN(core.NewSessionCookie(&core.EnvConfig{DBRepoAccessToken: testToken}, time.Now()), ";", 2)[0]

			if _, err := core.OpenDBRepoFromEnv(ctx); !errors.Is(err, c.want) {
				t.Errorf("open: %v", err)
			}
			// Reports whether `rec` is the guard's answer
			isRefusedByGuard := func(rec *httptest.ResponseRecorder) bool {
				return rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), c.want.Message)
			}
			if rec := callScheduledSync(); !isRefusedByGuard(rec) {
				t.Errorf("scheduled sync: %d %s", rec.Code, rec.Body)
			}
			if rec := call(api.HandleSyncArchive, "POST", "/api/sync-archive", "", cookie); !isRefusedByGuard(rec) {
				t.Errorf("sync now: %d %s", rec.Code, rec.Body)
			}
			if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"a/one"}`, cookie); !isRefusedByGuard(rec) {
				t.Errorf("add: %d %s", rec.Code, rec.Body)
			}
			if rec := call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", `{"enabled":false}`, cookie); !isRefusedByGuard(rec) {
				t.Errorf("turn off sync: %d %s", rec.Code, rec.Body)
			}
			gh.mu.Lock()
			defer gh.mu.Unlock()
			if len(gh.commits) != 0 || gh.workflowToggles != 0 {
				t.Errorf("wrote to the repository: commits %q, workflow toggles %d", gh.commits, gh.workflowToggles)
			}
		})
	}
}

// Tests that when GitHub refuses the server token (401), the browser gets 502 and the session cookie is left alone.
//
// NOTE: The browser treats a 401 from our API as "session expired" and signs out.
// If GitHub's 401 were passed through, a token problem would look like a sign-out.
// So a GitHub failure is always 502, whatever its status code.
func TestGitHubRejectingTheTokenIsNotSignOut(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	gh.mu.Lock()
	gh.tokenRejected = true
	gh.mu.Unlock()
	// The session is live but GitHub refuses the token.
	// It must be 502, with no `Set-Cookie` that deletes the session cookie
	rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "GitHub 401: Bad credentials") || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Errorf("token rejected: %d %s %q", rec.Code, rec.Body, rec.Header().Values("Set-Cookie"))
	}
}

// Tests that the sign-in status still answers when the token owner can't be read, with an empty username.
// The username is shown only in the account menu, so it must not keep the dashboard from opening.
func TestSignInStatusWithoutUsername(t *testing.T) {
	setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	// A new fake GitHub has another address, which is part of the cache key.
	// So the token owner kept for the previous one isn't used, and it is read again
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.mu.Lock()
	gh.ownerDown = true
	gh.mu.Unlock()
	if rec := call(api.HandleLoginSession, "GET", "/api/login-session", "", cookie); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"repo":"me/archive","username":""}` {
		t.Errorf("owner can't be read: %d %s", rec.Code, rec.Body)
	}
}

// Tests the session and same-site checks.
// A request without a session cookie gets 401.
// After sign-in, the sign-in status has the DB repository name and the username.
// A write request from another site (`Origin`) gets 403
func TestRequiresSessionAndSameOrigin(t *testing.T) {
	setup(t, sampleStarItems, sampleStarLists)
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", ""); rec.Code != 401 {
		t.Errorf("no cookie: got %d", rec.Code)
	}
	cookie := login(t)
	if rec := call(api.HandleLoginSession, "GET", "/api/login-session", "", cookie); !strings.Contains(rec.Body.String(), `"repo":"me/archive","username":"user4242"`) {
		t.Errorf("session: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("DELETE", "http://example.test/api/archive-star-items?id=1", nil)
	req.Header.Set("Origin", "https://evil.test")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	api.HandleArchiveStarItems(rec, req)
	if rec.Code != 403 {
		t.Errorf("cross-origin: got %d", rec.Code)
	}
}

// Tests that the three archive file GETs (star items, star lists, snapshots) return the raw file, or `[]` if there is no file.
// It is personal data, so all must have `Cache-Control: no-store` and a JSON `Content-Type`.
// If the browser accepts gzip, the response is compressed.
func TestArchiveFilesGetReturnsFile(t *testing.T) {
	setup(t, sampleStarItems, "")
	cookie := login(t)
	for _, c := range []struct {
		handler http.HandlerFunc
		path    string
		want    string
	}{
		{api.HandleArchiveStarItems, "/api/archive-star-items", sampleStarItems},
		{api.HandleArchiveStarLists, "/api/archive-star-lists", "[]"},
		{api.HandleArchiveRepoMeta, "/api/archive-repo-meta", "[]"},
	} {
		rec := call(c.handler, "GET", c.path, "", cookie)
		if rec.Code != 200 || rec.Body.String() != c.want || rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %q %v", c.path, rec.Code, rec.Body, rec.Header())
		}
	}

	// If the browser says it accepts gzip, the response is compressed, and it equals the original when decompressed.
	// With `q=0` (not accepted), it isn't compressed
	for _, c := range []struct {
		acceptEncoding string
		wantGzip       bool
	}{
		{"gzip, deflate, br, zstd", true},
		{"br;q=1.0, GZIP;q=0.5", true},
		{"gzip;q=0", false},
		{"deflate, br", false},
	} {
		req := httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Accept-Encoding", c.acceptEncoding)
		rec := httptest.NewRecorder()
		api.HandleArchiveStarItems(rec, req)
		body := rec.Body.String()
		if c.wantGzip {
			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatalf("%q: not gzip: %v", c.acceptEncoding, err)
			}
			raw, _ := io.ReadAll(zr)
			body = string(raw)
		}
		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if rec.Code != 200 || gotGzip != c.wantGzip || body != sampleStarItems || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%q: %d gzip=%v %v %q", c.acceptEncoding, rec.Code, gotGzip, rec.Header(), body)
		}
	}
}

// Tests adding a repository directly (`repo_full_name_or_url` in `POST /api/archive-star-items`).
//   - It also accepts a GitHub address (including a trailing `/tree/...`).
//     It puts the repository in the chosen star list and adds it at the top of the archive
//   - Already there: 409. A missing star list: 400. Path tricks (`../user`): 400. A repository not on GitHub: 404
//   - The lookup runs while the files are read.
//     If reading the files fails, a repository not on GitHub still gives 404, and one that exists gives 502
func TestStarItemsAdd(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)

	rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"https://github.com/torvalds/linux/tree/master","star_list_id":200}`, cookie)
	var added core.StarItem
	json.Unmarshal(rec.Body.Bytes(), &added)
	if rec.Code != 200 || added.Name != "torvalds/linux" || len(added.StarListIDs) != 1 || added.StarListIDs[0] != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	firstLine := strings.Split(gh.readFile(core.BuildStarItemShardPath(shardIndexOf(2325298))), "\n")[1]
	if !strings.HasPrefix(firstLine, `{"id":2325298,"node_id":"R_2325298","name":"torvalds/linux","note":"","star_list_ids":[200],`) {
		t.Errorf("new entry should be first in its shard: %s", firstLine)
	}

	cases := []struct {
		body string
		want int
	}{
		{`{"repo_full_name_or_url":"torvalds/linux"}`, 409},                    // Already there
		{`{"repo_full_name_or_url":"torvalds/linux","star_list_id":999}`, 400}, // Missing star list
		{`{"repo_full_name_or_url":"../user"}`, 400},                           // Path trick
		{`{"repo_full_name_or_url":"nobody/missing-repo"}`, 404},               // Not on GitHub
	}
	for _, c := range cases {
		if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", c.body, cookie); rec.Code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.body, rec.Code, rec.Body, c.want)
		}
	}

	gh.mu.Lock()
	gh.contentsHangup = true
	gh.mu.Unlock()
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"repo_full_name_or_url":"nobody/missing-repo"}`, 404},
		{`{"repo_full_name_or_url":"torvalds/linux"}`, http.StatusBadGateway},
	} {
		if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", c.body, cookie); rec.Code != c.want {
			t.Errorf("files can't be read, %s: got %d %s, want %d", c.body, rec.Code, rec.Body, c.want)
		}
	}
}

// Tests editing an item's note and star lists, and deleting an item.
//   - Star list IDs are saved sorted by ID. `<` and Korean text in the note are saved as is
//   - Editing a missing item gives 404
//   - The commit subject says what was done (star list change, delete)
func TestStarItemsPatchAndDelete(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)

	// Star lists are sorted by ID (200, 100 -> 100, 200).
	// `<` and Korean text in the note are saved as is
	rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"메모 <중요>","star_list_ids":[200,100]}`, cookie)
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if line := strings.Split(gh.readFile(core.BuildStarItemShardPath(shardIndexOf(2))), "\n")[1]; !strings.Contains(line, `"note":"메모 <중요>","star_list_ids":[100,200]`) {
		t.Errorf("patched line: %s", line)
	}
	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=999", `{"note":"x"}`, cookie); rec.Code != 404 {
		t.Errorf("missing id: got %d", rec.Code)
	}
	if rec := call(api.HandleArchiveStarItems, "DELETE", "/api/archive-star-items?id=1", "", cookie); rec.Code != 200 || strings.Contains(gh.readStarItems(), `"id":1,`) {
		t.Errorf("delete: %d %s", rec.Code, gh.readStarItems())
	}
	want := []string{"user(star-items): change star lists of b/two", "user(star-items): remove a/one"}
	if strings.Join(gh.commits, "|") != strings.Join(want, "|") {
		t.Errorf("commits = %v, want %v", gh.commits, want)
	}
}

// Tests that both changes remain when a commit from another device cuts in during a save.
// The server applies the change again from the start on top of the new commit
func TestUpdateRetriesOnConflict(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	// Another device deleted a/one right before the note-saving commit
	gh.interfereOnce = func(files map[string]string) {
		putStarItems(files, strings.Replace(joinStarItems(files), strings.Split(sampleStarItems, "\n")[1]+"\n", "", 1))
	}
	rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"after conflict"}`, cookie)
	content := gh.readStarItems()
	if rec.Code != 200 || !strings.Contains(content, "after conflict") || strings.Contains(content, `"id":1,`) {
		t.Errorf("both changes should survive: %d\n%s", rec.Code, content)
	}
}

// Tests creating, renaming and deleting a star list.
//   - The ID is the creation time (milliseconds), and it grows in creation order
//   - A name that differs only in case is the same name, so it gives 409
//   - Deleting also removes the star list from items. Deleting the definition and cleaning up the items is one commit
func TestStarLists(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)

	before := time.Now().UnixMilli()
	var a core.StarList
	var b core.StarList
	json.Unmarshal(call(api.HandleArchiveStarLists, "POST", "/api/archive-star-lists", `{"name":"Rust","description":""}`, cookie).Body.Bytes(), &a)
	json.Unmarshal(call(api.HandleArchiveStarLists, "POST", "/api/archive-star-lists", `{"name":"Go","description":""}`, cookie).Body.Bytes(), &b)
	if a.ID < before || b.ID <= a.ID {
		t.Errorf("ids should be creation time and increasing: %d, %d (before %d)", a.ID, b.ID, before)
	}
	if rec := call(api.HandleArchiveStarLists, "POST", "/api/archive-star-lists", `{"name":"graphics"}`, cookie); rec.Code != 409 {
		t.Errorf("duplicate name: got %d", rec.Code)
	}
	if rec := call(api.HandleArchiveStarLists, "PATCH", fmt.Sprintf("/api/archive-star-lists?id=%d", a.ID), `{"name":"Rustlang","description":"x"}`, cookie); rec.Code != 200 {
		t.Errorf("rename: %d %s", rec.Code, rec.Body)
	}

	// Delete: removes the star list and removes it from items.
	// Clears the record to count commits from here
	gh.commits = nil
	if rec := call(api.HandleArchiveStarLists, "DELETE", "/api/archive-star-lists?id=100", "", cookie); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(gh.readStarItems(), "[100]") || strings.Contains(gh.readFile(core.StarListsDBPath), `"id":100,`) {
		t.Errorf("star list 100 should be gone everywhere")
	}
	// Deleting the star list and cleaning up the items is one commit
	if strings.Join(gh.commits, "|") != "user(star-lists): delete Graphics\n\nRemoved from 1 repo" {
		t.Errorf("delete commits: %q", gh.commits)
	}
}

// Tests the current repository info query (`POST /api/github-repo-meta`).
// Only repositories that were found go in `repos`.
// IDs that GitHub says it can't find go in `missing`, and IDs that aren't repositories are left out.
// A malformed ID gives 400
func TestRepoMetaDropsInaccessible(t *testing.T) {
	gh := setup(t, "", "")
	cookie := login(t)
	gh.gone = map[string]bool{"R_gone": true}
	rec := call(api.HandleGitHubRepoMeta, "POST", "/api/github-repo-meta", `{"ids":["R_1","R_gone","R_3"]}`, cookie)
	if strings.TrimSpace(rec.Body.String()) != `{"missing":["R_gone"],"repos":[{"id":"R_1","nameWithOwner":"a/one","stargazerCount":42}]}` {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if rec := call(api.HandleGitHubRepoMeta, "POST", "/api/github-repo-meta", `{"ids":["bad id!"]}`, cookie); rec.Code != 400 {
		t.Errorf("bad id: got %d", rec.Code)
	}
}

// Tests the sync (`core.SyncArchiveWithGitHub`).
//   - New Stars go at the top of their shards in GitHub's order. Existing items (including notes) aren't touched
//   - Metadata snapshots are made in archive order
//   - There are three commits: adding, refreshing metadata snapshots, and refreshing README snapshots.
//     The add commit's body lists the added repositories
//   - Running it again changes nothing, so there is no commit
func TestSync(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, "", "")
	gh.starred = []string{"new/first", "new/second", "new/third", "old/archived"}
	putStarItems(gh.files, fmt.Sprintf("["+"\n"+
		`{"id":%d,"node_id":"R_%d","name":"old/archived","note":"memo","star_list_ids":[],"starred_at":"x"}`+
		"\n"+"]"+"\n", makeRepoID("old/archived"), makeRepoID("old/archived")))

	repo := openTestDBRepo(t)
	result, err := core.SyncArchiveWithGitHub(ctx, repo, testLongStepsEndBy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result != (core.ArchiveSyncResult{NumStarredRepos: 4, NumAddedRepos: 3, NumMetaCheckedRepos: 4, NumUnavailableRepos: 0, NumCheckedReadmes: 4}) {
		t.Errorf("result = %+v", result)
	}
	var archive []core.StarItem
	json.Unmarshal([]byte(gh.readStarItems()), &archive)
	archiveNames := []string{} // Names in archive order (shard by shard)
	for _, s := range archive {
		archiveNames = append(archiveNames, s.Name)
	}
	// Three new Stars go at the top of their shards in GitHub's order.
	// Existing items (including notes) stay as they are
	if len(archive) != 4 || !strings.Contains(gh.readStarItems(), `"name":"old/archived","note":"memo"`) || !isInShardOrder(archiveNames, gh.starred) {
		t.Errorf("archive:"+"\n"+"%s", gh.readStarItems())
	}
	// Four snapshots are made, also in archive order
	metas := readMeta(t, gh)
	metaNames := []string{}
	for _, m := range metas {
		metaNames = append(metaNames, m.Name)
	}
	first := slices.IndexFunc(metas, func(m core.RepoMeta) bool { return m.Name == "new/first" })
	if len(metas) != 4 || !slices.Equal(metaNames, archiveNames) || first < 0 || metas[first].Description != "about new/first" || metas[first].License != "MIT" || metas[first].UpdatedAt == "" {
		t.Errorf("meta = %+v", metas)
	}

	// Commit message: the subject is `<who>(<file>): <summary>`, with a list in the body for several items
	want := []string{
		"sync(star-items): archive 3 stars\n\n- new/first\n- new/second\n- new/third",
		"sync(repo-meta): refresh 4 repos, 0 unavailable",
		"sync(readmes): update readmes of 4 repos\n\nNo README:\n- " + strings.Join(archiveNames, "\n- "), // In archive order
	}
	if strings.Join(gh.commits, "|") != strings.Join(want, "|") {
		t.Errorf("commits = %q", gh.commits)
	}

	// Running it again has nothing to add, no changed metadata and no changed README, so no commit is made.
	// The absent markers and the READMEs of today's group are asked again, and which items are in the group depends on the date, so that count isn't compared
	result, err = core.SyncArchiveWithGitHub(ctx, repo, testLongStepsEndBy(), false)
	result.NumCheckedReadmes = 0
	if err != nil || result != (core.ArchiveSyncResult{NumStarredRepos: 4, NumAddedRepos: 0, NumMetaCheckedRepos: 4, NumUnavailableRepos: 0}) {
		t.Errorf("second run: %+v %v", result, err)
	}
	if len(gh.commits) != 3 {
		t.Errorf("second run should not commit: %q", gh.commits[3:])
	}
}

// Tests snapshots of repositories that disappeared.
//   - If the query itself fails (an outage), nothing is recorded as gone.
//     It isn't an error when every batch failed with a failure that passes on its own (502),
//     and it is one when they failed with a failure that needs a person (401)
//   - If GitHub says it can't find a repository, the snapshot content stays and only the first time it was seen gone (`unavailable_since`) is added.
//     The next run keeps that time
//   - If it shows up again, the mark is removed
//   - With no time budget, nothing is checked
func TestSyncKeepsSnapshotOfVanishedRepos(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, "", "")
	gh.starred = []string{"a/kept", "a/vanishing"}
	repo := openTestDBRepo(t)
	if _, err := core.SyncArchiveWithGitHub(ctx, repo, testLongStepsEndBy(), false); err != nil {
		t.Fatal(err)
	}
	vanishingID := fmt.Sprintf("R_%d", makeRepoID("a/vanishing"))

	// If the query fails because of a GitHub outage, nothing is recorded as "gone"
	gh.graphqlDown = true
	if result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, time.Minute); err != nil || len(result.UnavailableRepoIDs) != 0 || result.NumCheckedRepos != 0 {
		t.Errorf("outage: %+v %v", result, err)
	}
	gh.graphqlDownStatus = http.StatusUnauthorized
	if result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, time.Minute); err == nil || len(result.UnavailableRepoIDs) != 0 || result.NumCheckedRepos != 0 {
		t.Errorf("every batch refused: %+v %v", result, err)
	}
	gh.graphqlDown, gh.graphqlDownStatus = false, 0

	// When deleted, the snapshot (description, etc.) stays and only the time is added
	gh.gone = map[string]bool{vanishingID: true}
	if result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, time.Minute); err != nil || len(result.UnavailableRepoIDs) != 1 {
		t.Errorf("vanished: %+v %v", result, err)
	}
	m := readMeta(t, gh)[1]
	if m.Name != "a/vanishing" || m.Description != "about a/vanishing" || m.UnavailableSince == "" {
		t.Errorf("vanished snapshot = %+v", m)
	}
	if last := gh.commits[len(gh.commits)-1]; last != "sync(repo-meta): refresh 2 repos, 1 unavailable\n\nUnavailable:\n- a/vanishing" {
		t.Errorf("vanish commit = %q", last)
	}
	since := m.UnavailableSince

	// The next run keeps the time it first disappeared
	core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, time.Minute)
	if got := readMeta(t, gh)[1].UnavailableSince; got != since {
		t.Errorf("unavailable_since changed: %s -> %s", since, got)
	}

	// If it shows up again (for example, private then public), the mark is removed
	gh.gone = nil
	core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, time.Minute)
	if got := readMeta(t, gh)[1]; got.UnavailableSince != "" {
		t.Errorf("revived snapshot = %+v", got)
	}
	if last := gh.commits[len(gh.commits)-1]; !strings.HasSuffix(last, "Available again:\n- a/vanishing") {
		t.Errorf("revive commit = %q", last)
	}

	// With no time, nothing is checked
	if result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, repo, 0); err != nil || result.NumCheckedRepos != 0 {
		t.Errorf("no budget: %+v %v", result, err)
	}
}

// Tests that reading all Stars stops at the first page that fails.
// Of 30 pages, the first is read alone, then 6 at a time, and page 3 and later fail.
// The goroutine that read page 2 may read one more before it sees the failure, so at most 1 + 6 + 1 pages are asked
func TestSyncStopsReadingStarsAtTheFirstFailedPage(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	for i := 1; i <= 3000; i++ {
		gh.starred = append(gh.starred, fmt.Sprintf("owner/starred%d", i))
	}
	gh.starredFailFrom = 3
	result, err := core.SyncArchiveWithGitHub(context.Background(), openTestDBRepo(t), testLongStepsEndBy(), false)
	var apiErr *core.GitHubAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || result.NumAddedRepos != 0 || gh.starredPageReads > 1+6+1 {
		t.Errorf("%+v %v, page reads %d", result, err, gh.starredPageReads)
	}
}

// Tests that the metadata sync step stops sending batches after a rate limit answer.
// 3,000 items are 30 batches of 100, asked 6 at a time.
//   - When the first answer is the rate limit, at most the first 6 batches (sent together) go out.
//     It isn't an error, because the limit passes on its own
//   - When it comes from the third answer on, the 2 batches answered before it are counted.
//     Each of those 2 goroutines may send one more batch before it sees the stop, so at most 6 + 2 go out
func TestSyncRepoMetaStopsAtRateLimit(t *testing.T) {
	ctx := context.Background()
	items := []string{}
	for id := 1; id <= 3000; id++ {
		items = append(items, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	for _, c := range []struct {
		from        int // The first query (counting from 1) that gets the rate limit answer
		wantChecked int
		maxQueries  int
	}{
		{1, 0, 6},
		{3, 200, 8},
	} {
		gh := setup(t, "[\n"+strings.Join(items, ",\n")+"\n]\n", sampleStarLists)
		gh.metaRateLimitFrom = c.from
		result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, openTestDBRepo(t), time.Minute)
		if err != nil || result.NumCheckedRepos != c.wantChecked || gh.metaQueries > c.maxQueries {
			t.Errorf("rate limit from query %d: %+v %v, queries %d", c.from, result, err, gh.metaQueries)
		}
	}
}

// Reads and decodes the metadata snapshot file (`db/repo-meta.json`) of the fake GitHub `gh`.
// Stops the test if it can't be read.
func readMeta(t *testing.T, gh *fakeGitHub) []core.RepoMeta {
	var metas []core.RepoMeta
	if err := json.Unmarshal([]byte(gh.readFile(core.RepoMetaDBPath)), &metas); err != nil {
		t.Fatalf("repo-meta.json: %v", err)
	}
	return metas
}

// Makes distinct fake Star names: `<prefix>/repo-0000`, `<prefix>/repo-0001`, ...
//
// Args:
//   - `prefix`: Name to use as the repository owner
//   - `n`: How many
func makeManyStars(prefix string, n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s/repo-%04d", prefix, i)
	}
	return names
}

// Fetches all Star pages the way the browser does.
// It gets the first page with `GET /api/github-stars` to learn the last page number, then gets the other pages and joins them.
//
// Args:
//   - `cookie`: Session cookie
//
// Returns:
//   - The Stars of all pages, in page order
func fetchAllStarred(t *testing.T, cookie string) []core.StarredRepo {
	var first struct {
		StarredRepos []core.StarredRepo `json:"starred_repos"`
		LastPage     int                `json:"last_page"`
	}
	rec := call(api.HandleGitHubStars, "GET", "/api/github-stars?page=1", "", cookie)
	json.Unmarshal(rec.Body.Bytes(), &first)
	all := first.StarredRepos
	for page := 2; page <= first.LastPage; page++ {
		var next struct {
			StarredRepos []core.StarredRepo `json:"starred_repos"`
		}
		json.Unmarshal(call(api.HandleGitHubStars, "GET", fmt.Sprintf("/api/github-stars?page=%d", page), "", cookie).Body.Bytes(), &next)
		all = append(all, next.StarredRepos...)
	}
	return all
}

// Tests the Stars page API (`GET /api/github-stars`).
// A page has 100 Stars, and the last page number (`last_page`) is given, so all 2,500 can be fetched over all pages.
// A wrong page number such as 0 gives 400
func TestGitHubStarsListsAllPages(t *testing.T) {
	gh := setup(t, sampleStarItems, "")
	gh.starred = makeManyStars("big", 2500)
	cookie := login(t)

	var first struct {
		StarredRepos []core.StarredRepo `json:"starred_repos"`
		LastPage     int                `json:"last_page"`
	}
	json.Unmarshal(call(api.HandleGitHubStars, "GET", "/api/github-stars?page=1", "", cookie).Body.Bytes(), &first)
	if len(first.StarredRepos) != 100 || first.LastPage != 25 || first.StarredRepos[0].Repo.StargazersCount != 7 {
		t.Fatalf("page 1: %d stars, last_page %d", len(first.StarredRepos), first.LastPage)
	}
	if all := fetchAllStarred(t, cookie); len(all) != 2500 {
		t.Errorf("all pages: %d stars, want 2500", len(all))
	}
	if rec := call(api.HandleGitHubStars, "GET", "/api/github-stars?page=0", "", cookie); rec.Code != 400 {
		t.Errorf("page 0: got %d", rec.Code)
	}
}

// Tests importing many Stars.
//   - The browser sends only the IDs of the chosen repositories. Even 2,500 is one request and one commit.
//     IDs that aren't my Stars are skipped
//   - The list in the commit body has at most 20 entries, and the rest is shortened to a count
//   - Sending the same request again changes nothing and makes no commit (idempotent)
//   - Items go at the top in GitHub's order, and the starred time is GitHub's value
//   - An empty list, or a body that mixes the two request shapes, gives 400
func TestStarItemsImportIsIdempotentAtScale(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = makeManyStars("big", 2500)
	cookie := login(t)
	all := fetchAllStarred(t, cookie)

	// The browser sends only the IDs of the chosen repositories.
	// The server reads the repository info from GitHub itself
	post := func(ids []int64) map[string]int {
		body, _ := json.Marshal(map[string]any{"starred_repo_ids": ids, "star_list_id": 200})
		rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie)
		var out map[string]int
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 {
			t.Fatalf("import: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	ids := []int64{}
	for _, s := range all {
		ids = append(ids, s.Repo.ID)
	}

	// Even 2500 is one request and one commit.
	// IDs that aren't my Stars (1, 999) are skipped
	if r := post(append(ids, 1, 999)); r["added"] != 2500 || r["skipped"] != 2 {
		t.Errorf("import: %v", r)
	}
	if len(gh.commits) != 1 {
		t.Errorf("one commit per import: %q", gh.commits)
	}
	// The body list has at most 20 entries, and the rest is shortened to a count
	lines := strings.Split(gh.commits[0], "\n")
	if lines[0] != "user(star-items): archive 2500 stars" || len(lines) != 23 || lines[22] != "… and 2480 more" {
		t.Errorf("import commit: %d lines, first %q, last %q", len(lines), lines[0], lines[len(lines)-1])
	}

	// Sending the same request again changes nothing (and makes no commit)
	if r := post(ids[:1000]); r["added"] != 0 || len(gh.commits) != 1 {
		t.Errorf("re-import: %v, commits %d", r, len(gh.commits))
	}

	var archive []core.StarItem
	json.Unmarshal([]byte(gh.readStarItems()), &archive)
	seen := map[int64]bool{}
	for _, s := range archive {
		if seen[s.ID] {
			t.Fatalf("duplicate id %d in archive", s.ID)
		}
		seen[s.ID] = true
	}
	// Items go at the top of their shards in GitHub's order, and the starred time is GitHub's value
	archiveNames := []string{}
	gitHubOrder := []string{}
	for _, s := range archive {
		archiveNames = append(archiveNames, s.Name)
	}
	for _, s := range all {
		gitHubOrder = append(gitHubOrder, s.Repo.FullName)
	}
	newest := slices.IndexFunc(archive, func(s core.StarItem) bool { return s.Name == all[0].Repo.FullName })
	if len(archive) != 2502 || newest < 0 || archive[newest].StarredAt != "2026-10-01T00:00:00Z" || archive[newest].StarListIDs[0] != 200 || !isInShardOrder(archiveNames, gitHubOrder) {
		t.Errorf("archive has %d entries (want 2500 + 2 existing), newest at %d", len(archive), newest)
	}

	// Bad input: empty, or both shapes mixed
	for _, body := range []string{`{"starred_repo_ids":[]}`, `{"repo_full_name_or_url":"a/b","starred_repo_ids":[1]}`} {
		if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", body, cookie); rec.Code != 400 {
			t.Errorf("%s: got %d", body, rec.Code)
		}
	}
}

// Tests that Import doesn't add a Star again when the sync archived it first while the dialog was open.
// Duplicates are checked against the archive at save time, not the browser's list
func TestStarItemsImportSkipsWhatAppearedMeanwhile(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, "")
	gh.starred = makeManyStars("race", 3)
	cookie := login(t)
	all := fetchAllStarred(t, cookie)

	// While the page was open, the daily sync archived the first repository first
	core.AddStarredReposToStarCatalog(ctx, openTestDBRepo(t), all[:1], []int64{}, "sync(star-items)")

	body, _ := json.Marshal(map[string]any{"starred_repo_ids": []int64{all[0].Repo.ID, all[1].Repo.ID, all[2].Repo.ID}})
	rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie)
	if !strings.Contains(rec.Body.String(), `"added":2`) || strings.Count(gh.readStarItems(), "race/repo-0000") != 1 {
		t.Errorf("got %s\n%s", rec.Body, gh.readStarItems())
	}
}

// Tests that Import and the sync map GitHub star list membership to archive star lists with the same rules.
//   - A GitHub star list name that differs only in leading or trailing spaces, case, or Korean normalization (NFD) is the same archive star list
//   - A name not in the archive (Rust) becomes a new star list. The new star list and the new items go in one commit
//   - Import adds both the star list chosen in the dialog and the GitHub membership. The sync gives the same result
//   - Star lists of items already in the archive aren't changed
func TestImportAndSyncKeepGitHubStarLists(t *testing.T) {
	ctx := context.Background()
	// Archive star lists: Graphics (100) and a Korean name (200).
	// The GitHub star lists with the same names are written with different spaces, case and Korean normalization
	nfdKorean := "\u1112\u1161\u11ab\u1100\u1173\u11af" // The Korean name split into jamo (NFD). A name typed on macOS can arrive like this
	gh := setup(t, sampleStarItems, "["+"\n"+`{"id":100,"name":"Graphics","description":""},`+"\n"+`{"id":200,"name":"한글","description":""}`+"\n"+"]"+"\n")
	gh.starred = []string{"a/one", "new/imported", "new/synced", "new/plain"}
	gh.githubStarLists = map[string][]string{
		" graphics ": {"a/one", "new/imported", "new/synced"}, // a/one is already in the archive
		nfdKorean:    {"new/imported", "new/synced"},
		"Rust":       {"new/imported", "new/synced"}, // A star list not in the archive -> created
	}
	cookie := login(t)

	// Import: new/imported is chosen, with Graphics (100) also chosen in the dialog
	var page struct {
		StarredRepos    []core.StarredRepo  `json:"starred_repos"`
		GitHubStarLists map[string][]string `json:"github_star_lists"`
	}
	json.Unmarshal(call(api.HandleGitHubStars, "GET", "/api/github-stars?page=1", "", cookie).Body.Bytes(), &page)
	if got := page.GitHubStarLists[fmt.Sprint(makeRepoID("new/imported"))]; len(got) != 3 {
		t.Errorf("github_star_lists for the dialog = %v", page.GitHubStarLists)
	}
	var imported core.StarredRepo
	for _, s := range page.StarredRepos {
		if s.Repo.FullName == "new/imported" {
			imported = s
		}
	}
	body, _ := json.Marshal(map[string]any{"starred_repo_ids": []int64{imported.Repo.ID}, "star_list_id": 100})
	if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	// The new star list and the new item go in one commit
	if strings.Join(gh.commits, "|") != "user(star-items): archive 1 star\n\n- new/imported\n\nNew star lists:\n- Rust" {
		t.Errorf("import commits = %q", gh.commits)
	}

	// Sync: adds the other new Stars.
	// For the same GitHub star list, they must go into the same star list as Import
	if _, err := core.SyncArchiveWithGitHub(ctx, openTestDBRepo(t), testLongStepsEndBy(), false); err != nil {
		t.Fatal(err)
	}
	var starLists []core.StarList
	json.Unmarshal([]byte(gh.readFile(core.StarListsDBPath)), &starLists)
	if len(starLists) != 3 || starLists[2].Name != "Rust" {
		t.Fatalf("star lists = %+v", starLists)
	}
	rust := starLists[2].ID
	var archive []core.StarItem
	json.Unmarshal([]byte(gh.readStarItems()), &archive)
	byName := map[string][]int64{}
	for _, s := range archive {
		byName[s.Name] = s.StarListIDs
	}
	want := map[string][]int64{
		"new/imported": {100, 200, rust}, // Graphics (dialog + GitHub), the Korean name, Rust
		"new/synced":   {100, 200, rust}, // The sync gives the same result
		"new/plain":    {},               // Not in a GitHub star list
		"a/one":        {100},            // An existing item stays as it is
	}
	for name, ids := range want {
		if !slices.Equal(byName[name], ids) {
			t.Errorf("%s star lists = %v, want %v", name, byName[name], ids)
		}
	}
}

// Tests the case where GitHub applied the branch move but the response was lost,
// and one more commit from another device landed before the check.
// The branch contains our commit, so adding directly, Import and deleting a star list must all succeed.
// The same change must not be applied twice (three commits).
func TestCommitAppliedButReplyLostWhileAnotherCommitLands(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"new/one"}
	cookie := login(t)
	// Every branch move is applied and then the response is lost.
	// Right after, one commit from another device lands
	gh.refLostReply, gh.otherAfterLostReply = true, true

	if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"torvalds/linux"}`, cookie); rec.Code != 200 {
		t.Errorf("add: %d %s", rec.Code, rec.Body)
	}
	body, _ := json.Marshal(map[string]any{"starred_repo_ids": []int64{makeRepoID("new/one")}})
	if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"added":1`) {
		t.Errorf("import: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api.HandleArchiveStarLists, "DELETE", "/api/archive-star-lists?id=100", "", cookie); rec.Code != 200 {
		t.Errorf("delete star list: %d %s", rec.Code, rec.Body)
	}
	if len(gh.commits) != 3 {
		t.Errorf("each change must be committed once: %q", gh.commits)
	}
}

// Tests the note length limit (`core.MaxNoteLength`).
// It counts characters, not bytes.
// Going over gives 400 and nothing is saved.
// Leading and trailing spaces are removed before counting
func TestNoteLengthLimit(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	patch := func(note string) int {
		body, _ := json.Marshal(map[string]string{"note": note})
		return call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", string(body), cookie).Code
	}
	// Counts characters.
	// A Korean character is 3 bytes in UTF-8, so counting bytes would go over even at 2,000 characters
	if code := patch(strings.Repeat("메", core.MaxNoteLength)); code != 200 {
		t.Errorf("note at the limit: %d", code)
	}
	if code := patch(strings.Repeat("메", core.MaxNoteLength+1)); code != 400 || len(gh.commits) != 1 {
		t.Errorf("note over the limit: %d, commits %d", code, len(gh.commits))
	}
	// Leading and trailing spaces are removed before counting
	if code := patch("  " + strings.Repeat("a", core.MaxNoteLength) + "  "); code != 200 {
		t.Errorf("note with surrounding spaces: %d", code)
	}
}

// Tests that the sync steps (adding new Stars, refreshing metadata and README snapshots) don't block each other.
// If adding fails, the snapshot refreshes still run to the end and commit.
// The error has the name of the failed step.
// When the outage ends, the next run fills in the missing Stars
func TestSyncStepsDoNotBlockEachOther(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"new/one"}
	gh.githubStarListsDown = true // Adding new Stars needs the GitHub star lists, and only that query fails

	// Adding new Stars fails, but the unrelated snapshot refreshes run to the end and commit.
	// The error comes back with the step name
	result, err := core.SyncArchiveWithGitHub(ctx, openTestDBRepo(t), testLongStepsEndBy(), false)
	if err == nil || !strings.Contains(err.Error(), "archive new stars") || strings.Contains(err.Error(), "refresh repo meta") {
		t.Errorf("sync error = %v", err)
	}
	if result.NumMetaCheckedRepos == 0 || result.NumCheckedReadmes == 0 || len(gh.commits) != 2 ||
		!strings.HasPrefix(gh.commits[0], "sync(repo-meta): ") || !strings.HasPrefix(gh.commits[1], "sync(readmes): ") {
		t.Errorf("meta refresh must still run: %+v, commits %q", result, gh.commits)
	}

	// When the outage ends, the next run fills in the missing Stars
	gh.githubStarListsDown = false
	if result, err := core.SyncArchiveWithGitHub(ctx, openTestDBRepo(t), testLongStepsEndBy(), false); err != nil || result.NumAddedRepos != 1 {
		t.Errorf("next sync: %+v %v", result, err)
	}
}

// Tests edge cases of Import.
//   - If the branch move was applied but only the response was lost, it still succeeds and the added count is correct
//   - A request that chooses a missing star list gives 400, even if all chosen repositories are already there
//   - Sending the same request again changes nothing
func TestImportChecksStarListEvenIfNothingIsNew(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"new/one"}
	cookie := login(t)
	ids := []int64{makeRepoID("new/one")}
	post := func(starListID int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"starred_repo_ids": ids, "star_list_id": starListID})
		return call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie)
	}
	// If GitHub applied the commit but only the response was lost, it still succeeds and the added count is correct
	gh.refLostReply = true
	rec := post(100)
	gh.refLostReply = false
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"added":1`) || len(gh.commits) != 1 {
		t.Fatalf("import with lost reply: %d %s, commits %q", rec.Code, rec.Body, gh.commits)
	}
	// A bad request that chooses a missing star list gives 400, even if everything chosen is already there
	if rec := post(999); rec.Code != 400 || len(gh.commits) != 1 {
		t.Errorf("unknown star list: %d %s, commits %q", rec.Code, rec.Body, gh.commits)
	}
	// Sending it again changes nothing
	if rec := post(100); rec.Code != 200 || len(gh.commits) != 1 {
		t.Errorf("re-import: %d %s, commits %q", rec.Code, rec.Body, gh.commits)
	}
}

// Tests what happens when the GitHub star lists can't be read.
// The list of Stars in the Import dialog is only for display, so it is still returned, with `github_star_lists_failed` added.
// Adding to the archive needs the membership, so it fails and writes nothing
func TestGitHubStarsWithoutGitHubStarLists(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"new/one"}
	gh.githubStarListsDown = true // Only the GitHub star list query fails. Other GraphQL requests (DB repository state, etc.) work
	cookie := login(t)

	// The list of Stars is for the dialog display, so it is returned as is with only the failure flag added
	rec := call(api.HandleGitHubStars, "GET", "/api/github-stars?page=1", "", cookie)
	var page map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &page)
	if _, ok := page["github_star_lists"]; rec.Code != 200 || ok || string(page["github_star_lists_failed"]) != "true" || !strings.Contains(string(page["starred_repos"]), "new/one") {
		t.Errorf("page 1: %d %s", rec.Code, rec.Body)
	}

	// Adding to the archive needs the star lists, so it fails and writes nothing
	body, _ := json.Marshal(map[string]any{"starred_repo_ids": []int64{makeRepoID("new/one")}})
	if rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", string(body), cookie); rec.Code == 200 || len(gh.commits) != 0 {
		t.Errorf("import without GitHub star lists: %d %s, commits %q", rec.Code, rec.Body, gh.commits)
	}
}

// Tests the case where another device deleted a star list right before a save that puts an item in it.
// The archive read again doesn't have that star list, so it gives 400,
// and no ID pointing to a missing star list is saved
func TestStarListDeletedWhileAssigning(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	// Right before the save that puts b/two in Graphics (100), another device deleted Graphics
	gh.interfereOnce = func(files map[string]string) {
		files[core.StarListsDBPath] = strings.Replace(files[core.StarListsDBPath], `{"id":100,"name":"Graphics","description":""},`+"\n", "", 1)
	}
	rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"star_list_ids":[100]}`, cookie)
	// The archive read again has no Graphics, so the save is refused, and no ID pointing to a missing star list is saved
	if rec.Code != 400 || strings.Contains(gh.readStarItems(), `"name":"b/two","note":"","star_list_ids":[100]`) {
		t.Errorf("got %d %s\n%s", rec.Code, rec.Body, gh.readStarItems())
	}
}

// Tests cleaning (`CleanStarListIDs`) and checking (`CheckStarListIDs`) the star list IDs saved on an item.
// Cleaning drops missing IDs, removes duplicates and sorts by ID.
// Checking refuses if even one ID is missing
func TestCleanStarListIDs(t *testing.T) {
	c := &core.StarCatalog{StarLists: []core.StarList{{ID: 300}, {ID: 100}, {ID: 200}}}
	// The missing ID (999) is dropped, duplicates become one, and the order is by ID (which is creation order)
	if got := c.CleanStarListIDs([]int64{200, 999, 100, 200}); !slices.Equal(got, []int64{100, 200}) {
		t.Errorf("got %v", got)
	}
	if _, err := c.CheckStarListIDs([]int64{100, 999}); err == nil {
		t.Errorf("unknown id should be rejected")
	}
}

// Tests when a save is retried.
// It retries only when the branch really moved.
//   - A failed tree creation isn't a conflict, so there is no retry. It gives 502 right away (a GitHub failure)
//   - If another device keeps committing first, it tries up to 5 times in all, then gives 409 "changed while saving"
//   - If the branch didn't move but the move is refused (no branch, empty repository, etc.), it doesn't retry, whatever the status code.
//     It returns GitHub's error as 502
func TestUpdateRetriesOnlyWhenBranchMoved(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)

	// A failed tree creation isn't a conflict, so it returns the error right away without a retry.
	// A GitHub failure is 502
	gh.treeStatus = http.StatusUnprocessableEntity
	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"x"}`, cookie); rec.Code != http.StatusBadGateway || gh.treeCalls != 1 {
		t.Errorf("tree failure: %d %s, tree calls %d", rec.Code, rec.Body, gh.treeCalls)
	}

	// If another device keeps committing first, it retries up to the limit, then reports 409
	gh.treeStatus = 0
	gh.treeCalls = 0
	gh.interfereAll = true
	rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"y"}`, cookie)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "changed while saving") || gh.treeCalls != 5 || len(gh.commits) != 0 {
		t.Errorf("branch always moved: %d %s, tree calls %d, commits %q", rec.Code, rec.Body, gh.treeCalls, gh.commits)
	}

	// If the branch didn't move but the move is refused (no branch, empty repository, etc.), it isn't a conflict, whatever the status code.
	// It doesn't retry and returns GitHub's error as 502.
	// GitHub's 409 isn't read as our save conflict (409 "changed while saving")
	gh.interfereAll = false
	for _, c := range []fakeRefFailure{{http.StatusUnprocessableEntity, "Reference does not exist"}, {http.StatusConflict, "Git Repository is empty."}} {
		gh.treeCalls = 0
		gh.refStatus, gh.refMessage = c.status, c.message
		rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"z"}`, cookie)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), fmt.Sprintf("GitHub %d: %s", c.status, c.message)) || gh.treeCalls != 1 {
			t.Errorf("ref %d: %d %s, tree calls %d", c.status, rec.Code, rec.Body, gh.treeCalls)
		}
	}
}

// One branch move failure to test
type fakeRefFailure struct {
	status  int    // GitHub's response status code
	message string // GitHub's message
}

// Tests that a save that doesn't touch a missing data file doesn't create that file.
// A missing file counts as the same state as an empty list
func TestUpdateSkipsUntouchedMissingFiles(t *testing.T) {
	gh := setup(t, sampleStarItems, "") // There is no `star-lists.json` or `repo-meta.json`
	cookie := login(t)
	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"x"}`, cookie); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	// A missing file is the same state as an empty list, so if it wasn't touched, it isn't in the commit
	for _, path := range []string{core.StarListsDBPath, core.RepoMetaDBPath} {
		if _, ok := gh.files[path]; ok {
			t.Errorf("%s was created by an unrelated change", path)
		}
	}
}

// Tests the snapshot of a repository that was already gone at the first check.
// The only thing known is the name saved in the archive,
// so the snapshot is made with that name and the time it was gone is recorded
func TestSnapshotOfRepoVanishedBeforeFirstCheck(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.gone = map[string]bool{"R_2": true} // A repository already gone at the first check
	if result, err := core.SyncRepoMetaSnapshotsWithGitHub(ctx, openTestDBRepo(t), time.Minute); err != nil || len(result.UnavailableRepoIDs) != 1 {
		t.Fatalf("refresh: %+v %v", result, err)
	}
	m := readMeta(t, gh)[1]
	if m.ID != 2 || m.Name != "b/two" || m.UnavailableSince == "" || m.UpdatedAt == "" {
		t.Errorf("snapshot = %+v", m)
	}
}
