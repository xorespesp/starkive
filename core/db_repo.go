// The DB repository: what is where (file layout and format version), which repository it is (found by ID),
// whether it is safe to write to (state check and `useDBRepo`), and the shape, reading and writing of the config file `config.json`.
// How files are actually read and committed is in `store.go`.
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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
//	.github/workflows/starkive-sync.yml   auto sync workflow (installed by the setup tool)
//	README.md                             readme (installed by the setup tool)
const (
	MarkerDBPath    = ".starkive"          // Content is `{"format":{"major":1,"minor":0}}` (`EncodeMarker`). Only a repository with this file is used by starkive
	ConfigDBPath    = "config.json"        // Sync interval, dashboard address, sign-in GitHub App (`dbConfigFile`)
	StarItemsDBPath = "db/star-items.json" // Archive entries (`StarItem`)
	StarListsDBPath = "db/star-lists.json" // Star list definitions (`StarList`)
	RepoMetaDBPath  = "db/repo-meta.json"  // Snapshots of repository metadata (`RepoMeta`)
)

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

// Reads the environment variables, fills in the DB repository, and builds the server config.
// Used when the repository is needed without a session.
// Signed-in requests start with `Authorize`, and scheduled syncs with `AuthorizeScheduledSync`.
//
// Returns:
//   - Server config with the DB repository filled in.
//     500 if the environment variables are wrong, 409 `HTTPError` if the DB repository can't be used
func LoadConfig() (*Config, error) {
	cfg, err := LoadEnvConfig()
	if err != nil {
		return nil, err
	}
	if err := useDBRepo(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// State of the DB repository
type DBRepoState int

const (
	DBRepoEmpty    DBRepoState = iota // No commits at all. The setup tool sets it up by writing the marker
	DBRepoStarkive                    // Has the marker, and its major format version matches this code. It is a repository that starkive uses
	DBRepoForeign                     // Has commits but no marker. starkive doesn't use it
)

// DB repository found by ID. The first two fields come from the GitHub repository response. The state is decided by looking inside the repository
type dbRepo struct {
	FullName string      `json:"full_name"`      // Current name `owner/repo`. If the user renamed the repository, this is the new name
	Branch   string      `json:"default_branch"` // Default branch. GitHub gives a name even for an empty repository with no commits (measured: "main")
	State    DBRepoState `json:"-"`              // Set by `inspectDBRepo`. The struct tag `json:"-"` means the field is left out of JSON
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

// Cache of the result of finding the DB repository by ID (`ResolveDBRepo`). The key is `makeEnvCacheKey`.
//
// NOTE: The name and the state are kept as one unit.
// So a kept name is always used together with the state checked for that name.
var dbRepoCache cache[dbRepoResult]

// Result kept in the cache: the found repository, or an error
type dbRepoResult struct {
	repo dbRepo // The found repository. Zero value if `err` is set
	err  error  // Why it wasn't found
}

// How long to keep a result
const (
	dbRepoReadyTTL    = 10 * time.Minute // Result confirmed as a starkive repository. Every API goes through it, so it is kept long
	dbRepoNotReadyTTL = 10 * time.Second // Anything else (empty repository, foreign repository, error). Kept short, so a fix by a person shows up soon
)

// Asks GitHub for the DB repository by ID, fills its current name and default branch into `cfg`, and returns the repository's state.
// It also accepts an empty or foreign repository.
// The server must not call it directly.
// The server goes through `useDBRepo` instead, which allows only a starkive repository.
//
// Returns:
//   - The state of the repository.
//     `ErrDBRepoNotFound` if the token can't see it. The error from `newDBRepoFormatError` if the major format version differs
//
// NOTE: The name is the current name that GitHub returns, so it follows the user's renames.
// The result is kept in `dbRepoCache`.
func ResolveDBRepo(cfg *Config) (DBRepoState, error) {
	key := makeEnvCacheKey(cfg)
	result, found := dbRepoCache.get(key)
	if !found {
		result.repo, result.err = findDBRepo(cfg)
		ttl := dbRepoNotReadyTTL
		if result.err == nil && result.repo.State == DBRepoStarkive {
			ttl = dbRepoReadyTTL
		}
		dbRepoCache.put(key, result, ttl)
	}
	if result.err != nil {
		return 0, result.err
	}
	cfg.DBRepoFullName, cfg.DBRepoBranch = result.repo.FullName, result.repo.Branch
	return result.repo.State, nil
}

// Fills the DB repository into `cfg`, but allows it only when it is a repository that starkive uses.
//
// Returns:
//   - `nil` if allowed.
//     `ErrDBRepoNotSetUp` for an empty repository, `ErrDBRepoNotStarkive` for a repository with no marker,
//     otherwise the error from `ResolveDBRepo`
//
// NOTE: Every path where the server reaches the DB repository (`Authorize`, `LoadConfig`, `AuthorizeScheduledSync`) passes through this function.
// So there can't be a write path that skips the check.
// The server writes nothing to a foreign repository or to an empty repository that hasn't been set up.
// The token has permission to write workflows.
// This guard keeps starkive files from being committed to the wrong repository (e.g. the user's code repository).
// Only the setup tool writes to an empty repository for the first time.
func useDBRepo(cfg *Config) error {
	state, err := ResolveDBRepo(cfg)
	if err != nil {
		return err
	}
	switch state {
	case DBRepoEmpty:
		return ErrDBRepoNotSetUp
	case DBRepoForeign:
		return ErrDBRepoNotStarkive
	}
	return nil
}

// Asks GitHub for the DB repository by ID, and decides its state by looking inside it.
// It doesn't look at the cache (`dbRepoCache`).
//
// Returns:
//   - The found repository.
//     `ErrDBRepoNotFound` if it can't be seen (404). A 502 `HTTPError` if GitHub returns an unusable name or an empty branch
func findDBRepo(cfg *Config) (dbRepo, error) {
	res, err := CallGitHubAPI(cfg, fmt.Sprintf("/repositories/%d", cfg.DBRepoID), GitHubAPIRequest{})
	if err != nil {
		return dbRepo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return dbRepo{}, ErrDBRepoNotFound
	}
	if err := CheckGitHubAPI(res); err != nil {
		return dbRepo{}, err
	}
	var found dbRepo
	if err := json.NewDecoder(res.Body).Decode(&found); err != nil {
		return dbRepo{}, err
	}
	// The name goes into every API path after this, so its shape is checked even though GitHub returned it
	if !IsValidRepoFullName(found.FullName) || found.Branch == "" {
		return dbRepo{}, Errorf(http.StatusBadGateway, "GitHub returned an unusable archive repository")
	}
	located := *cfg // Struct copy. The caller's `cfg` is not changed until the result is in the cache. The found repository is inspected through this copy
	located.DBRepoFullName, located.DBRepoBranch = found.FullName, found.Branch
	if found.State, err = inspectDBRepo(&located); err != nil {
		return dbRepo{}, err
	}
	return found, nil
}

// GraphQL query that asks, in one go, whether the DB repository is empty and what the marker file at the tip of the default branch is.
// `$marker` is `HEAD:<marker path>`
const dbRepoStateQuery = `query($owner: String!, $name: String!, $marker: String!) {
  repository(owner: $owner, name: $name) {
    isEmpty
    marker: object(expression: $marker) { ... on Blob { text } }
  }
}`

// Decides the state of the DB repository by looking inside it.
// No commits means an empty repository. No marker means a foreign repository. With a marker, the format version is checked.
//
// Returns:
//   - The state of the repository.
//     The error from `newDBRepoFormatError` if the marker's major format version differs from this code, or the marker can't be read
//
// NOTE: Whether it is empty is decided by a value that GitHub defines (GraphQL `Repository.isEmpty`, docs).
// If this check wrongly said "empty", the setup tool would write a marker into a foreign repository.
// Emptiness and the marker are read in one query, at the same point. `HEAD` is the default branch.
// A repository with only a README and no marker is also treated as foreign.
// There is no exception like "allow it if it only has the initial files GitHub made". This keeps the rule simple.
func inspectDBRepo(cfg *Config) (DBRepoState, error) {
	owner, name, _ := strings.Cut(cfg.DBRepoFullName, "/")
	var data struct {
		Repository *struct {
			IsEmpty bool `json:"isEmpty"`
			Marker  *struct {
				Text *string `json:"text"` // `nil` if it is not a file (a folder) or can't be read as text
			} `json:"marker"` // `nil` if there is no marker file
		} `json:"repository"` // `nil` if the repository can't be seen
	}
	vars := map[string]any{"owner": owner, "name": name, "marker": "HEAD:" + MarkerDBPath}
	if err := callGraphQL(cfg, dbRepoStateQuery, vars, &data); err != nil {
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
// Only the setup tool writes the marker, but this lives in `core` so its shape matches what `inspectDBRepo` reads.
func EncodeMarker() []byte {
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

// Shape of the config file `config.json` at the root of the DB repository.
// One file holds the values a person changes in Settings (whether auto sync is on, and the interval),
// the value the setup tool records (the dashboard address), and the sign-in GitHub App settings.
// The field order is the key order in the file.
//
// NOTE: It is written as indented multi-line JSON (`encodeConfig`), so when one value changes, only that line shows in the diff.
// The key `dashboard_url` is a contract with the workflow in the DB repository.
// Renaming it requires raising the major version of `DBRepoFormat`.
// Each writing function (`UpdateSyncSettings`, `RecordSyncEnabled`, `SetDashboardURL`, `SaveGitHubApp`) changes only its own field.
// So changing one value never resets another value to a default or changes it.
type dbConfigFile struct {
	DashboardURL string         `json:"dashboard_url,omitempty"` // Dashboard address for the workflow to call. The setup tool records the address registered in the GitHub App (`GitHubApp.SiteURL`)
	Sync         syncConfig     `json:"sync"`                    // Auto sync settings
	SignInApp    *githubAppFile `json:"sign_in_app,omitempty"`   // Sign-in GitHub App (the client secret is ciphertext). A pointer, so it is `nil` if there is none yet, and the key is not written
}

// Auto sync settings in the config file
type syncConfig struct {
	IntervalDays int        `json:"interval_days,omitempty"` // Sync interval (days). If missing or not a valid choice, `defaultSyncIntervalDays` is used (`intervalDays`)
	EnabledSince *time.Time `json:"enabled_since,omitempty"` // When a person turned auto sync on (UTC). A pointer: `nil` (no key) means the state set by the person is off
}

// Auto sync intervals that can be chosen (days). The page's `SYNC_INTERVALS` (`public/app.js`) uses the same values
var syncIntervalChoices = []int{1, 2, 3, 7}

// Sync interval when the config file has none.
// The point of this tool is "keep it before it disappears", so it is daily
const defaultSyncIntervalDays = 1

// Returns the sync interval (days).
// If it is missing or not a valid choice, it returns the default (so one config value can't stop the sync)
func (c syncConfig) intervalDays() int {
	if slices.Contains(syncIntervalChoices, c.IntervalDays) {
		return c.IntervalDays
	}
	return defaultSyncIntervalDays
}

// 409 error for a config file (`ConfigDBPath`) that can't be decoded as JSON.
var ErrConfigBroken = Errorf(http.StatusConflict, "%s in the archive repository is broken. Fix or delete it, then run the starkive setup tool", ConfigDBPath)

// Decodes file content into a `dbConfigFile`.
//
// Args:
//   - `content`: The file content. `nil` (no file) gives empty settings
//
// Returns:
//   - The config file. `ErrConfigBroken` if it can't be decoded as JSON
func decodeConfig(content []byte) (dbConfigFile, error) {
	var file dbConfigFile
	if content == nil {
		return file, nil
	}
	if err := json.Unmarshal(content, &file); err != nil {
		return dbConfigFile{}, ErrConfigBroken
	}
	return file, nil
}

// Turns the config file into file content (JSON indented by two spaces, with a newline at the end)
func encodeConfig(file dbConfigFile) ([]byte, error) {
	content, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// Reads the config file from one commit, changes it with `change`, and writes it. It works on top of `UpdateFiles`.
// Every write to the config file goes through this function.
//
// Args:
//   - `change`: Changes only the fields it owns in the given config file, and returns a commit message.
//     If another commit gets in, it is called again with the new content
//
// Returns:
//   - The 409 error from `decodeConfig` if the file can't be read (nothing is written).
//     Otherwise an error from `change` or `UpdateFiles`
//
// NOTE: The file before and after `change` is written in the same shape (`encodeConfig`) and compared.
// If they are the same, nothing is committed.
// It doesn't write to a file it can't read, because writing a new one would erase the App settings inside it.
// The check and the save happen on the file right before the commit.
// So even when a settings write and an App write overlap, neither erases the other's values.
func updateConfig(cfg *Config, change func(file *dbConfigFile) (CommitMessage, error)) error {
	return UpdateFiles(cfg, []string{ConfigDBPath}, func(filesRead map[string][]byte) (FileChangesCommit, error) {
		file, err := decodeConfig(filesRead[ConfigDBPath])
		if err != nil {
			return FileChangesCommit{}, err
		}
		before, err := encodeConfig(file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		message, err := change(&file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		content, err := encodeConfig(file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		if bytes.Equal(content, before) {
			return FileChangesCommit{}, nil
		}
		return FileChangesCommit{FilesToWrite: map[string][]byte{ConfigDBPath: content}, Message: message}, nil
	})
}

// Reads the auto sync settings from the config file in the DB repository.
//
// Returns:
//   - The auto sync settings.
//     Empty settings (default interval, off) if the config file is missing or can't be read as JSON.
//     An error if the file can't be fetched
//
// NOTE: A file that can't be decoded is read as empty settings with no error, so a broken config file doesn't stop the scheduled sync.
// `updateConfig` blocks writes to such a file.
func readSyncConfig(cfg *Config) (syncConfig, error) {
	content, err := ReadFile(cfg, ConfigDBPath)
	if err != nil {
		return syncConfig{}, err
	}
	file, err := decodeConfig(content)
	if err != nil {
		return syncConfig{}, nil
	}
	return file.Sync, nil
}

// Records the dashboard address for the workflow to call (`dashboard_url`).
// Other values in the config file are not touched, and nothing is committed if the values stay the same.
//
// Args:
//   - `siteURL`: The canonical address of the dashboard, registered in the GitHub App (`GitHubApp.SiteURL`)
func SetDashboardURL(cfg *Config, siteURL string) error {
	return updateConfig(cfg, func(file *dbConfigFile) (CommitMessage, error) {
		file.DashboardURL = siteURL
		return NewCommitMessage("user(config)", "set dashboard address to "+siteURL), nil
	})
}
