// The sign-in GitHub App settings (`GitHubApp`): their format and encryption,
// and reading and saving them in the config file (`LoadGitHubApp`, `SaveGitHubApp`).
// Creating the GitHub App happens in the setup tool.
// This file only has the parts that the server and the tool share (settings format, encryption, reading, saving).
// The sign-in flow is in `sign_in.go`.
package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// GitHub App used to sign in to the dashboard.
// The setup tool creates it with the GitHub App manifest flow, and stores it encrypted in `sign_in_app` of the config file (`ConfigDBPath`).
// It has no permissions and no webhook, so it is only used for user sign-in (web flow)
type GitHubApp struct {
	AppID        int64  // GitHub App ID
	Slug         string // GitHub App address name (`github.com/settings/apps/<slug>`)
	OwnerID      int64  // User ID of the GitHub App owner. Must be the owner of the server token
	ClientID     string // Public value put in the sign-in address
	ClientSecret string // Secret used for the sign-in code exchange. Written to the file only as ciphertext
	SiteURL      string // Dashboard address registered in the App (the homepage, and the start of the sign-in callback). GitHub sends sign-in results to this address, so it is the canonical address of the dashboard
}

// Shape of the `sign_in_app` value in the config file.
// The field order is the key order in the file
type githubAppFile struct {
	AppID                 int64  `json:"app_id"`                  // `GitHubApp.AppID`
	Slug                  string `json:"slug"`                    // `GitHubApp.Slug`
	OwnerID               int64  `json:"owner_id"`                // `GitHubApp.OwnerID`
	ClientID              string `json:"client_id"`               // `GitHubApp.ClientID`
	ClientSecretEncrypted string `json:"client_secret_encrypted"` // Ciphertext of the client secret: base64url(12-byte nonce + AES-256-GCM ciphertext)
	SiteURL               string `json:"site_url"`                // `GitHubApp.SiteURL`
}

// State of the saved GitHub App settings
type GitHubAppStatus string

// States of the saved GitHub App settings
const (
	GitHubAppMissing    GitHubAppStatus = "missing"    // No settings. The App hasn't been created yet
	GitHubAppUnreadable GitHubAppStatus = "unreadable" // Settings exist but can't be used. The server token changed so they can't be decrypted, or values are missing. The setup tool replaces them with a new App
	GitHubAppReady      GitHubAppStatus = "ready"      // Can be used for sign-in
)

// Makes the AES-GCM that encrypts and decrypts the client secret.
// The key is 32 bytes made from the server token (`deriveKey(cfg, "starkive-github-app")`), so it is AES-256.
//
// NOTE: The key comes from the token, so git history only holds ciphertext, which can't be decrypted without the token.
// When the token changes, the settings can't be decrypted, and the setup tool creates a new App.
func newGitHubAppCipher(cfg *EnvConfig) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(cfg, "starkive-github-app"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Makes the additional authenticated data (AAD) that ties the ciphertext to the App with ID `appID` and client ID `clientID`.
//
// NOTE: It decrypts only with the same values.
// So if this ciphertext is copied into another App's settings, it won't decrypt.
func makeGitHubAppAAD(appID int64, clientID string) []byte {
	return []byte(strconv.FormatInt(appID, 10) + "\n" + clientID)
}

// Turns a GitHub App into the shape stored in the config file.
// The client secret is encrypted
func encodeGitHubApp(cfg *EnvConfig, app GitHubApp) (*githubAppFile, error) {
	gcm, err := newGitHubAppCipher(cfg)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(app.ClientSecret), makeGitHubAppAAD(app.AppID, app.ClientID)) // With the nonce as the first argument, the ciphertext is appended after it
	return &githubAppFile{
		AppID: app.AppID, Slug: app.Slug, OwnerID: app.OwnerID, ClientID: app.ClientID,
		ClientSecretEncrypted: base64.RawURLEncoding.EncodeToString(sealed), SiteURL: app.SiteURL,
	}, nil
}

// Turns a GitHub App into the `sign_in_app` value (JSON) of the config file
func EncodeGitHubAppConfigValue(cfg *EnvConfig, app GitHubApp) ([]byte, error) {
	file, err := encodeGitHubApp(cfg, app)
	if err != nil {
		return nil, err
	}
	return json.Marshal(file)
}

// Decodes the `sign_in_app` value of the config file into a GitHub App.
//
// Args:
//   - `file`: The value read from the config file. A pointer: `nil` if there are no settings
//
// Returns:
//   - The App. Even when it can't be decrypted, the public values (`Slug`, `SiteURL`, etc.) are filled in.
//     So the previous App can still be named, and its canonical address kept, when it is replaced
//   - The state. `GitHubAppUnreadable` if the ciphertext can't be decrypted, or `client_id` or `site_url` is empty
func decodeGitHubApp(cfg *EnvConfig, file *githubAppFile) (GitHubApp, GitHubAppStatus) {
	if file == nil {
		return GitHubApp{}, GitHubAppMissing
	}
	app := GitHubApp{AppID: file.AppID, Slug: file.Slug, OwnerID: file.OwnerID, ClientID: file.ClientID, SiteURL: file.SiteURL}
	sealed, err := base64.RawURLEncoding.DecodeString(file.ClientSecretEncrypted)
	if err != nil {
		return app, GitHubAppUnreadable
	}
	gcm, err := newGitHubAppCipher(cfg)
	if err != nil || len(sealed) < gcm.NonceSize() {
		return app, GitHubAppUnreadable
	}
	secret, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], makeGitHubAppAAD(file.AppID, file.ClientID))
	if err != nil || file.ClientID == "" || file.SiteURL == "" {
		return app, GitHubAppUnreadable
	}
	app.ClientSecret = string(secret)
	return app, GitHubAppReady
}

// In-memory cache of the GitHub App settings, per instance.
// The key is `buildDBRepoCacheKey` (the same DB repository and token give the same key)
//
// NOTE: Sign-in start and callback are open before sign-in, so anyone can call them again and again.
// Keeping the settings means GitHub is not read each time, and the server token's rate limit (5,000 per hour, docs) isn't used up.
var githubAppCache cache[githubAppRecord]

// Value kept in `githubAppCache`: the GitHub App settings and their state
type githubAppRecord struct {
	app    GitHubApp       // App settings
	status GitHubAppStatus // State of the settings
}

// How long to keep the GitHub App settings
const (
	githubAppReadyTTL    = 10 * time.Minute // Usable settings. The setup tool doesn't change them, and a new token gives a new cache key, so they are read fresh right away
	githubAppNotReadyTTL = 10 * time.Second // Missing or unusable settings. Kept short, so a fix by the setup tool shows up soon
)

// Puts the GitHub App settings and their state in `githubAppCache`.
// Usable settings are kept long, others short
func cacheGitHubApp(repo *DBRepo, app GitHubApp, status GitHubAppStatus) {
	ttl := githubAppNotReadyTTL
	if status == GitHubAppReady {
		ttl = githubAppReadyTTL
	}
	githubAppCache.put(buildDBRepoCacheKey(repo), githubAppRecord{app: app, status: status}, ttl)
}

// Reads the GitHub App settings from the config file in the DB repository.
// The result is kept in `githubAppCache` (`cacheGitHubApp`).
//
// Returns:
//   - The App and its state
//   - The read error if reading the file fails. `ErrDBConfigFileBroken` if the config file can't be decoded as JSON. Errors are not kept
func LoadGitHubApp(ctx context.Context, repo *DBRepo) (GitHubApp, GitHubAppStatus, error) {
	if record, found := githubAppCache.get(buildDBRepoCacheKey(repo)); found {
		return record.app, record.status, nil
	}
	content, err := ReadDBRepoFile(ctx, repo, ConfigDBPath)
	if err != nil {
		return GitHubApp{}, "", err
	}
	file, err := decodeDBConfigFile(content)
	if err != nil {
		return GitHubApp{}, "", err
	}
	app, status := decodeGitHubApp(repo.cfg, file.SignInApp)
	cacheGitHubApp(repo, app, status)
	return app, status, nil
}

// 409 error for when usable GitHub App settings already exist, so the new settings were not written
var ErrGitHubAppExists = Errorf(http.StatusConflict, "A GitHub App is already set up for this dashboard")

// Writes the settings of the newly created GitHub App `app` to `sign_in_app` in the config file of the DB repository.
// It writes only when there are no settings, or they can't be decrypted.
// Other values in the config file are left as they are.
//
// Returns:
//   - `ErrGitHubAppExists` if usable settings already exist
//
// NOTE: The "already exists" check is made inside `change` of `updateDBConfigFile`, that is, on the file right before the commit.
// So even if two setup tools run at once, only one gets in.
// When it replaces settings that can't be decrypted, the commit body says to delete the previous App.
// The private key is not received, so the server and the tool can't delete the App.
func SaveGitHubApp(ctx context.Context, repo *DBRepo, app GitHubApp) error {
	err := updateDBConfigFile(ctx, repo, func(file *dbConfigFile) (CommitMessage, error) {
		previous, status := decodeGitHubApp(repo.cfg, file.SignInApp)
		if status == GitHubAppReady {
			return CommitMessage{}, ErrGitHubAppExists
		}
		section, err := encodeGitHubApp(repo.cfg, app)
		if err != nil {
			return CommitMessage{}, err
		}
		file.SignInApp = section
		replaced := ""
		if status != GitHubAppMissing && previous.Slug != "" {
			replaced = "Replaced " + previous.Slug + ". Delete that app on GitHub."
		}
		return NewCommitMessage("user(config)", "set up sign-in app "+app.Slug, replaced), nil
	})
	if err != nil {
		return err
	}
	cacheGitHubApp(repo, app, GitHubAppReady) // Puts the new settings in the cache, so the read right after doesn't see the old state
	return nil
}
