// The config file `config.json` in the DB repository: its shape (`dbConfigFile`), reading and writing it (`updateDBConfigFile`),
// the sync settings in it (`syncConfig`), and the dashboard address (`SaveDashboardURL`).
// The functions that write the sync settings are in `sync_schedule.go`, and the one that writes the sign-in GitHub App settings is in `sign_in_app.go`.
// Writing goes through the storage engine (`UpdateDBRepoFiles` in `db_repo_io.go`).
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

// Shape of the config file `config.json` at the root of the DB repository.
// One file holds the values a person changes in Settings (whether sync is on, and the auto sync interval),
// the value the setup tool records (the dashboard address), and the sign-in GitHub App settings.
// The field order is the key order in the file.
//
// NOTE: It is written as indented multi-line JSON (`encodeDBConfigFile`), so when one value changes, only that line shows in the diff.
// The key `dashboard_url` is a contract with the workflow in the DB repository.
// Renaming it requires raising the major version of `DBRepoFormat`.
// Each writing function (`UpdateSyncSettings`, `RecordSyncEnabled`, `SaveDashboardURL`, `SaveGitHubApp`) changes only its own field.
// So changing one value never resets another value to a default or changes it.
type dbConfigFile struct {
	DashboardURL string         `json:"dashboard_url,omitempty"` // Dashboard address for the workflow to call. The setup tool records the address registered in the GitHub App (`GitHubApp.SiteURL`)
	Sync         syncConfig     `json:"sync"`                    // Sync settings
	SignInApp    *githubAppFile `json:"sign_in_app,omitempty"`   // Sign-in GitHub App (the client secret is ciphertext). A pointer, so it is `nil` if there is none yet, and the key is not written
}

// Sync settings in the config file
type syncConfig struct {
	IntervalDays *int       `json:"interval_days,omitempty"` // Auto sync interval (days), or `manualOnlyIntervalDays`. A pointer, so a missing key (`nil`) differs from 0. A missing or invalid value reads as `defaultSyncIntervalDays` (`intervalDays`)
	EnabledSince *time.Time `json:"enabled_since,omitempty"` // When a person turned sync on (UTC). A pointer: `nil` (no key) means the state set by the person is off
}

// Auto sync interval value meaning "Manual only": no scheduled sync, and only a run started by hand (Sync now) syncs
const manualOnlyIntervalDays = 0

// Auto sync intervals that can be chosen (days), and `manualOnlyIntervalDays`.
// The page's `SYNC_INTERVALS` (`public/app.js`) uses the same values
var syncIntervalChoices = []int{manualOnlyIntervalDays, 1, 2, 3, 7}

// Auto sync interval when the config file has none.
// The point of this tool is "keep it before it disappears", so it is daily
const defaultSyncIntervalDays = 1

// Returns the auto sync interval (days), or `manualOnlyIntervalDays`.
// If it is missing or not a valid choice, it returns the default (so one broken config value can't stop auto sync)
func (c syncConfig) intervalDays() int {
	if c.IntervalDays != nil && slices.Contains(syncIntervalChoices, *c.IntervalDays) {
		return *c.IntervalDays
	}
	return defaultSyncIntervalDays
}

// 409 error for a config file (`ConfigDBPath`) that can't be decoded as JSON.
var ErrDBConfigFileBroken = Errorf(http.StatusConflict, "%s in the archive repository is broken. Fix or delete it, then run the starkive setup tool", ConfigDBPath)

// Decodes file content into a `dbConfigFile`.
//
// Args:
//   - `content`: The file content. `nil` (no file) gives empty settings
//
// Returns:
//   - The config file. `ErrDBConfigFileBroken` if it can't be decoded as JSON
func decodeDBConfigFile(content []byte) (dbConfigFile, error) {
	var file dbConfigFile
	if content == nil {
		return file, nil
	}
	if err := json.Unmarshal(content, &file); err != nil {
		return dbConfigFile{}, ErrDBConfigFileBroken
	}
	return file, nil
}

// Turns the config file into file content (JSON indented by two spaces, with a newline at the end)
func encodeDBConfigFile(file dbConfigFile) ([]byte, error) {
	content, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// Reads the config file from one commit, changes it with `change`, and writes it.
// It works on top of `UpdateDBRepoFiles`.
// Every write to the config file goes through this function.
//
// Args:
//   - `change`: Changes only the fields it owns in the given config file, and returns a commit message.
//     If another commit gets in, it is called again with the new content
//
// Returns:
//   - The 409 error from `decodeDBConfigFile` if the file can't be read (nothing is written).
//     Otherwise an error from `change` or `UpdateDBRepoFiles`
//
// NOTE: The file before and after `change` is written in the same shape (`encodeDBConfigFile`) and compared.
// If they are the same, nothing is committed.
// It doesn't write to a file it can't read, because writing a new one would erase the App settings inside it.
// The check and the save happen on the file right before the commit.
// So even when a settings write and an App write overlap, neither erases the other's values.
func updateDBConfigFile(ctx context.Context, repo *DBRepo, change func(file *dbConfigFile) (CommitMessage, error)) error {
	return UpdateDBRepoFiles(ctx, repo, []string{ConfigDBPath}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		file, err := decodeDBConfigFile(filesRead[ConfigDBPath])
		if err != nil {
			return FileChangesCommit{}, err
		}
		before, err := encodeDBConfigFile(file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		message, err := change(&file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		content, err := encodeDBConfigFile(file)
		if err != nil {
			return FileChangesCommit{}, err
		}
		if bytes.Equal(content, before) {
			return FileChangesCommit{}, nil
		}
		return FileChangesCommit{FilesToWrite: map[string][]byte{ConfigDBPath: content}, Message: message}, nil
	})
}

// Reads the sync settings from the config file in the DB repository.
//
// Returns:
//   - The sync settings.
//     Empty settings (default interval, off) if the config file is missing or can't be read as JSON.
//     An error if the file can't be fetched
//
// NOTE: A file that can't be decoded is read as empty settings with no error, so a broken config file doesn't stop the scheduled sync.
// `updateDBConfigFile` blocks writes to such a file.
func readSyncConfig(ctx context.Context, repo *DBRepo) (syncConfig, error) {
	content, err := ReadDBRepoFile(ctx, repo, ConfigDBPath)
	if err != nil {
		return syncConfig{}, err
	}
	file, err := decodeDBConfigFile(content)
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
func SaveDashboardURL(ctx context.Context, repo *DBRepo, siteURL string) error {
	return updateDBConfigFile(ctx, repo, func(file *dbConfigFile) (CommitMessage, error) {
		file.DashboardURL = siteURL
		return NewCommitMessage("user(config)", "set dashboard address to "+siteURL), nil
	})
}
