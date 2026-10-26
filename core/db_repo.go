// The DB repository: what is where (file layout and format version), which repository it is (found by ID),
// and whether it is safe to write to (state check and `OpenDBRepo`).
// How files are actually read and committed is in `db_repo_io.go`, and the config file `config.json` is in `db_config.go`.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// File layout of the DB repository.
// Together with the file format (one item per line, key order), this structure is set by the code, so it is not configurable.
// If the layout changes, raise the major version of `DBRepoFormat`.
//
//	.starkive                             marker of a starkive repository, and its format version
//	config.json                           settings and the sign-in GitHub App
//	db/                                   the archive
//	db/readmes/                           README snapshots, one file per repository (`ReadmesDBDir`)
//	.github/workflows/starkive-sync.yml   sync workflow (installed by the setup tool)
//	README.md                             readme (installed by the setup tool)
const (
	MarkerDBPath    = ".starkive"          // Content is `{"format":{"major":1,"minor":0}}` (`EncodeDBRepoMarker`). Only a repository with this file is used by starkive
	ConfigDBPath    = "config.json"        // Sync interval, dashboard address, sign-in GitHub App (`dbConfigFile`)
	StarItemsDBPath = "db/star-items.json" // Archive entries (`StarItem`)
	StarListsDBPath = "db/star-lists.json" // Star list definitions (`StarList`)
	RepoMetaDBPath  = "db/repo-meta.json"  // Snapshots of repository metadata (`RepoMeta`)
)

// Folder of the README snapshots.
// The files of a repository go in the subfolder named by the last two digits of its ID (`buildReadmeFolderPath`).
// The last two digits of real IDs spread evenly (measured), so a folder holds about 200 files at 20,000 repositories.
const ReadmesDBDir = "db/readmes"

// Folder of the archive items (`StarItem`).
// The items are split by ID into `StarItemShardCount` files (shards), and an item is only in the shard of `computeShardIndexOfStarItem`.
// A write then writes only the shards whose items changed, not the whole archive.
// All shards always exist, even when empty, so a missing shard is an error and never reads as an empty archive.
const StarItemsDBDir = "db/star-items"

// Number of archive item shards.
// At 20,000 items one shard is about 400KB, and writing it costs about 0.15 seconds more than a tiny file (measured).
// Changing it moves almost every item to another shard, so it is part of the format.
const StarItemShardCount = 8

// Builds the path of shard `shardIndex` (0 to `StarItemShardCount` - 1), like `db/star-items/03.json`.
func BuildStarItemShardPath(shardIndex int) string {
	return fmt.Sprintf("%s/%02d.json", StarItemsDBDir, shardIndex)
}

// Returns the index of the shard that holds the item with ID `id`.
// `id` must be positive, because Go's `%` keeps the sign of a negative number.
// The last digits of real repository IDs spread evenly (measured: 19,991 IDs, each shard within 3.5% of the mean).
func computeShardIndexOfStarItem(id int64) int {
	return int(id % StarItemShardCount)
}

// Version of the DB repository format (`format` in the marker file)
type DBRepoFormatVersion struct {
	Major int `json:"major"` // Raised by a change that other versions can't read or call correctly. A different value stops the server and the setup tool
	Minor int `json:"minor"` // Raised by a change that keeps the contract, such as a fix in the workflow script. A different value stops nothing
}

// Format version of the DB repository that this code reads and writes.
// If the repository has a different major version, both the server and the setup tool stop (`newDBRepoFormatError`).
//
// NOTE: This keeps different versions of the dashboard, setup tool and workflow from reading or writing one repository wrongly.
// It doesn't try to adapt or convert anything.
// Raise the major version when the file layout or format changes,
// or the contract between the workflow and the server changes (config keys, request and headers, response `status`).
// Raise the minor version for a change that installed workflows need but that keeps the contract, such as a fix for a bug in the workflow script.
// But for now, before the implementation is final, it stays at 1.0.
// After it is final, it is raised by this rule.
//
// History:
//   - 1.0 (2026-10-20): The first format
var DBRepoFormat = DBRepoFormatVersion{Major: 1, Minor: 0}

// Reports whether this code can read and write a DB repository of format version `v` (the major versions are the same).
func (v DBRepoFormatVersion) IsCompatible() bool {
	return v.Major == DBRepoFormat.Major
}

// Returns the version as `<major>.<minor>`, as people see it (for example "1.0").
// `fmt` uses this method for `%s` (the `fmt.Stringer` interface).
func (v DBRepoFormatVersion) String() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// Reads the environment variables and opens the DB repository (`OpenDBRepo`).
// Used when the repository is needed without a session.
// Signed-in requests start with `Authorize`, and calls from the sync workflow with `AuthorizeSyncWorkflowCall`.
//
// Returns:
//   - The opened DB repository.
//     500 if the environment variables are wrong, 409 `HTTPError` if the DB repository can't be used,
//     502 if GitHub fails or doesn't answer
func OpenDBRepoFromEnv(ctx context.Context) (*DBRepo, error) {
	cfg, err := LoadEnvConfig()
	if err != nil {
		return nil, err
	}
	return OpenDBRepo(ctx, cfg)
}

// State of the DB repository
type DBRepoState int

const (
	DBRepoEmpty    DBRepoState = iota // No commits at all. The setup tool sets it up by writing the marker
	DBRepoStarkive                    // Has the marker, and its major format version matches this code. It is a repository that starkive uses
	DBRepoForeign                     // Has commits but no marker. starkive doesn't use it
)

// A DB repository found by its ID, with the config used to reach it.
// Only this package makes it (`FindDBRepo`), so a value always holds the name and branch that GitHub gave for that ID.
// The functions that read or write the DB repository take it, so none of them can run before the repository is found.
//
// NOTE: The fields are private, so other packages can't fill them in (like a C++ class whose constructor is private).
// A zero value made elsewhere has a `nil` config, so using it panics before any request is sent.
type DBRepo struct {
	cfg      *EnvConfig  // Token and API address
	fullName string      // Current name `owner/repo`. If the user renamed the repository, this is the new name
	branch   string      // Default branch. GitHub gives a name even for an empty repository with no commits (measured: "main")
	state    DBRepoState // State when it was found (`readDBRepoState`)
}

// Returns the config (token and GitHub address) used to reach the repository.
// Requests that take it can be about this repository or not (Stars, sign-in)
func (repo *DBRepo) EnvConfig() *EnvConfig { return repo.cfg }

// Returns the current name `owner/repo`
func (repo *DBRepo) FullName() string { return repo.fullName }

// Returns the default branch
func (repo *DBRepo) Branch() string { return repo.branch }

// Returns the state when it was found
func (repo *DBRepo) State() DBRepoState { return repo.state }

// Builds the path of a GitHub API under this repository, `/repos/<owner/repo>` followed by `suffix` (e.g. `/git/trees`)
func (repo *DBRepo) BuildAPIPath(suffix string) string { return "/repos/" + repo.fullName + suffix }

// Builds the Contents API path of one file in the repository (`/repos/<owner/repo>/contents/<path>`).
//
// Args:
//   - `filePath`: File path in the repository. Characters not allowed in a path are encoded
func (repo *DBRepo) BuildContentsAPIPath(filePath string) string {
	return repo.BuildAPIPath("/contents/" + (&url.URL{Path: filePath}).EscapedPath())
}

// Errors for a DB repository that can't be used (all 409)
var (
	ErrDBRepoNotFound    = Errorf(http.StatusConflict, "The access token can't see the archive repository. Select it under the token's Repository access, or run the starkive setup tool again") // The token can't see the repository
	ErrDBRepoNotSetUp    = Errorf(http.StatusConflict, "The archive repository isn't set up yet. Run the starkive setup tool")                                                                   // The repository is empty
	ErrDBRepoNotStarkive = Errorf(http.StatusConflict, "The archive repository already has other files. starkive only uses an empty repository or one it set up before")                         // The repository has no marker
)

// Makes the 409 error for a DB repository whose format version this code can't use.
//
// Args:
//   - `found`: Format version written in the repository's marker. `nil` if the marker can't be read
func newDBRepoFormatError(found *DBRepoFormatVersion) error {
	foundText := "unknown"
	if found != nil {
		foundText = found.String()
	}
	return Errorf(http.StatusConflict, "The archive repository uses starkive format %s, but this version of starkive uses format %s. "+
		"Use the starkive version that set up the repository, or set up a new repository with the setup tool of this version", foundText, DBRepoFormat)
}

// Cache of the result of finding the DB repository by ID (`FindDBRepo`).
// The key is `buildEnvCacheKey`.
//
// NOTE: The name and the state are kept as one unit.
// So a kept name is always used together with the state checked for that name.
var dbRepoCache cache[dbRepoResult]

// Result kept in the cache: the found repository, or an error
type dbRepoResult struct {
	repo DBRepo // The found repository, without a config. Zero value if `err` is set. `FindDBRepo` gives each caller a copy with its own config
	err  error  // Why it wasn't found
}

// How long to keep a result
const (
	dbRepoReadyTTL    = 10 * time.Minute // Result confirmed as a starkive repository. Every API goes through it, so it is kept long
	dbRepoNotReadyTTL = 10 * time.Second // Anything else (empty repository, foreign repository, error). Kept short, so a fix by a person shows up soon
)

// Finds the DB repository by ID (`cfg.DBRepoID`), with its current name, default branch and state.
// It also accepts an empty or foreign repository.
// The server must not call it directly.
// The server goes through `OpenDBRepo` instead, which allows only a starkive repository.
//
// Returns:
//   - The found repository.
//     `ErrDBRepoNotFound` if the token can't see it. The error from `newDBRepoFormatError` if the major format version differs
//
// NOTE: The name is the current name that GitHub returns, so it follows the user's renames.
// The result is kept in `dbRepoCache`, unless the deadline of `ctx` cut the request.
func FindDBRepo(ctx context.Context, cfg *EnvConfig) (*DBRepo, error) {
	key := buildEnvCacheKey(cfg)
	result, found := dbRepoCache.get(key)
	if !found {
		timingStep := beginServerTimingStep(ctx, "db-repo") // Shows only when the repository wasn't in the cache
		result.repo, result.err = fetchDBRepoByID(ctx, cfg)
		timingStep.end()
		ttl := dbRepoNotReadyTTL
		if result.err == nil && result.repo.state == DBRepoStarkive {
			ttl = dbRepoReadyTTL
		}
		// NOTE: A request cut at its own deadline says nothing about the repository.
		// Keeping that error would make the other requests fail for a while too
		if ctx.Err() == nil { // Told by `ctx`, because a single slow request (`githubTimeout`) gives the same error and is worth keeping
			kept := result
			kept.repo.cfg = nil // The config holds the token, so it isn't kept longer than the request
			dbRepoCache.put(key, kept, ttl)
		}
	}
	if result.err != nil {
		return nil, result.err
	}
	repo := result.repo // Struct copy, so the kept value is never changed
	repo.cfg = cfg
	return &repo, nil
}

// Finds the DB repository and allows it only when it is a repository that starkive uses.
//
// Returns:
//   - The opened repository.
//     `ErrDBRepoNotSetUp` for an empty repository, `ErrDBRepoNotStarkive` for a repository with no marker,
//     otherwise the error from `FindDBRepo`
//
// NOTE: Every path where the server reaches the DB repository (`Authorize`, `OpenDBRepoFromEnv`, `AuthorizeSyncWorkflowCall`) passes through this function.
// So there can't be a write path that skips the check.
// The server writes nothing to a foreign repository or to an empty repository that hasn't been set up.
// The token has permission to write workflows.
// This guard keeps starkive files from being committed to the wrong repository (e.g. the user's code repository).
// Only the setup tool writes to an empty repository for the first time.
func OpenDBRepo(ctx context.Context, cfg *EnvConfig) (*DBRepo, error) {
	repo, err := FindDBRepo(ctx, cfg)
	if err != nil {
		return nil, err
	}
	switch repo.state {
	case DBRepoEmpty:
		return nil, ErrDBRepoNotSetUp
	case DBRepoForeign:
		return nil, ErrDBRepoNotStarkive
	}
	return repo, nil
}

// Asks GitHub for the DB repository by ID, and decides its state by looking inside it.
// It doesn't look at the cache (`dbRepoCache`).
//
// Returns:
//   - The found repository.
//     `ErrDBRepoNotFound` if it can't be seen (404). A 502 `HTTPError` if GitHub returns an unusable name or an empty branch
func fetchDBRepoByID(ctx context.Context, cfg *EnvConfig) (DBRepo, error) {
	res, err := CallGitHubAPI(ctx, cfg, fmt.Sprintf("/repositories/%d", cfg.DBRepoID), GitHubAPIRequest{})
	if err != nil {
		return DBRepo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return DBRepo{}, ErrDBRepoNotFound
	}
	if err := CheckGitHubResponse(res); err != nil {
		return DBRepo{}, err
	}
	var answer struct {
		FullName string `json:"full_name"`      // Current name `owner/repo`
		Branch   string `json:"default_branch"` // Default branch
	}
	if err := DecodeGitHubResponse(res, &answer); err != nil {
		return DBRepo{}, err
	}
	// The name goes into every API path after this, so its shape is checked even though GitHub returned it
	if !IsValidRepoFullName(answer.FullName) || answer.Branch == "" {
		return DBRepo{}, Errorf(http.StatusBadGateway, "GitHub returned an unusable archive repository")
	}
	repo := DBRepo{cfg: cfg, fullName: answer.FullName, branch: answer.Branch}
	if repo.state, err = readDBRepoState(ctx, &repo); err != nil {
		return DBRepo{}, err
	}
	return repo, nil
}

// Decides the state of the DB repository by looking inside it.
// No commits means an empty repository.
// No marker means a foreign repository.
// With a marker, the format version is checked.
//
// Returns:
//   - The state of the repository.
//     The error from `newDBRepoFormatError` if the marker's major format version differs from this code, or the marker can't be read
//
// NOTE: Whether it is empty is decided by a value that GitHub defines (GraphQL `Repository.isEmpty`, docs).
// If this check wrongly said "empty", the setup tool would write a marker into a foreign repository.
// Emptiness and the marker are read in one query, at the same point.
// `HEAD` is the default branch.
// A repository with only a README and no marker is also treated as foreign.
// There is no exception like "allow it if it only has the initial files GitHub made".
// This keeps the rule simple.
func readDBRepoState(ctx context.Context, repo *DBRepo) (DBRepoState, error) {
	owner, name, _ := strings.Cut(repo.fullName, "/")
	// Asks, in one go, whether the DB repository is empty and what the marker file at the tip of the default branch is.
	// `$marker` is `HEAD:<marker path>`
	const query = `query($owner: String!, $name: String!, $marker: String!) {
		repository(owner: $owner, name: $name) {
			isEmpty
			marker: object(expression: $marker) { ... on Blob { text } }
		}
	}`
	var data struct {
		Repository *struct {
			IsEmpty bool `json:"isEmpty"`
			Marker  *struct {
				Text *string `json:"text"` // `nil` if it is not a file (a folder) or can't be read as text
			} `json:"marker"` // `nil` if there is no marker file
		} `json:"repository"` // `nil` if the repository can't be seen
	}
	vars := map[string]any{"owner": owner, "name": name, "marker": "HEAD:" + MarkerDBPath}
	if err := callGraphQL(ctx, repo.cfg, query, vars, &data); err != nil {
		return 0, err
	}
	switch {
	case data.Repository == nil:
		return 0, ErrDBRepoNotFound
	case data.Repository.IsEmpty:
		return DBRepoEmpty, nil
	case data.Repository.Marker == nil:
		return DBRepoForeign, nil
	}
	var marker dbRepoMarker
	text := data.Repository.Marker.Text
	if text == nil || json.Unmarshal([]byte(*text), &marker) != nil || marker.Format == nil {
		return 0, newDBRepoFormatError(nil)
	}
	if !marker.Format.IsCompatible() {
		return 0, newDBRepoFormatError(marker.Format)
	}
	return DBRepoStarkive, nil
}

// Content of the marker file (`MarkerDBPath`)
type dbRepoMarker struct {
	Format *DBRepoFormatVersion `json:"format"` // A pointer: `nil` if the key is missing, so a marker without a version isn't read as 0.0
}

// Makes the content of the marker file, `{"format":{"major":1,"minor":0}}`.
// Only the setup tool writes the marker, but this lives in `core` so its shape matches what `readDBRepoState` reads.
func EncodeDBRepoMarker() []byte {
	content, _ := json.Marshal(dbRepoMarker{Format: &DBRepoFormat}) // A struct of numbers always encodes, so the error is ignored
	return append(content, '\n')
}

// Shape of a GitHub `owner/repo`.
// The username is letters, digits and hyphens, up to 39 characters (the first character is not a hyphen).
// The repository name is letters, digits, `.`, `_` and `-`, up to 100 characters (docs)
var repoFullNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// Decides whether a string has the shape of a GitHub `owner/repo`.
//
// NOTE: The repository names `.` and `..` fit the shape but are blocked.
// In an API path, they would climb up the path and could call another GitHub API.
func IsValidRepoFullName(fullName string) bool {
	if !repoFullNamePattern.MatchString(fullName) {
		return false
	}
	_, name, _ := strings.Cut(fullName, "/")
	return name != "." && name != ".."
}
