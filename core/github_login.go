// Sign-in with a GitHub account (OAuth web flow), and the sign-in GitHub App settings: format, encryption, reading and saving.
// Only one person can enter the dashboard: the owner of the server token. The flow has three steps:
//  1. `StartGitHubLogin`: Makes an anti-forgery value (state), puts it in a short signed cookie, and returns the GitHub authorization address
//  2. When the user approves on GitHub, GitHub sends them back to the callback registered in the App (`/api/login-session`) with `code` and `state`
//  3. `FinishGitHubLogin`: Checks the state, exchanges `code` for a user token, and checks whether that user's ID is the token owner
//
// The server stores nothing.
// The browser carries the state in a signed cookie, and the sign-in result in the session cookie (`session.go`).
// Creating the GitHub App happens in the setup tool.
// This file only has the parts that the server and the tool share (settings format, encryption, reading, saving).
package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Name, lifetime and attributes of the sign-in state cookie
const (
	githubLoginStateCookie = "github_login_state" // Cookie name
	githubLoginStateTTL    = 10 * time.Minute     // How long to wait between starting sign-in and coming back from GitHub
	// Cookie attributes. `Path` is narrowed to the sign-in API, so the cookie is not sent with other requests.
	// `SameSite=Lax`: the navigation back from GitHub comes from another site, so a `Strict` cookie would not be sent.
	// A `Lax` cookie is sent with this kind of GET navigation
	githubLoginStateAttrs = "Path=/api/login-session; HttpOnly; Secure; SameSite=Lax"
)

// `Set-Cookie` value that deletes the state cookie.
// The callback always sends it, whatever the result, so opening the same callback address again doesn't work
const ClearGitHubLoginStateCookie = githubLoginStateCookie + "=; Max-Age=0; " + githubLoginStateAttrs

// Reason a GitHub sign-in failed.
// The callback passes it to the page as `/?login_error=<reason>`, and the page picks the matching text
type GitHubLoginFailure string

// Reasons a GitHub sign-in failed
const (
	GitHubLoginCancelled  GitHubLoginFailure = "cancelled"   // The user didn't approve on the GitHub page
	GitHubLoginExpired    GitHubLoginFailure = "expired"     // The state is missing, different or expired (10 minutes passed, or a forged request)
	GitHubLoginNotAllowed GitHubLoginFailure = "not_allowed" // The signed-in GitHub account is not the token owner
	GitHubLoginFailed     GitHubLoginFailure = "failed"      // Talking to GitHub failed (including when the token owner couldn't be checked)
)

// Starts a GitHub sign-in: makes a state, puts it in a cookie, and builds the GitHub authorization address for the browser to go to.
//
// Args:
//   - `app`: The sign-in GitHub App (its `client_id` is used)
//   - `now`: The current time. The state expiry is counted from here
//
// Returns:
//   - `authorizeURL`: The GitHub authorization address for the browser to go to
//   - `setCookie`: The `Set-Cookie` value of the state cookie, to add to the response
//
// NOTE: The callback address (`redirect_uri`) is not sent.
// GitHub sends the user back to the callback address registered in the App, so the request's `Host` header doesn't need to be trusted.
func StartGitHubLogin(cfg *Config, app GitHubApp, now time.Time) (authorizeURL string, setCookie string, err error) {
	state, cookieValue, err := newLoginState(cfg, now.Add(githubLoginStateTTL))
	if err != nil {
		return "", "", err
	}
	cookie := fmt.Sprintf("%s=%s; Max-Age=%d; %s", githubLoginStateCookie, cookieValue, int(githubLoginStateTTL.Seconds()), githubLoginStateAttrs)
	query := url.Values{"client_id": {app.ClientID}, "state": {state}, "allow_signup": {"false"}}
	return cfg.GitHubWeb + "/login/oauth/authorize?" + query.Encode(), cookie, nil
}

// Checks the callback request from GitHub and decides whether to allow the sign-in.
// Order of checks: refusal on the GitHub page -> state (forgery, expiry) -> exchange `code` for a user token -> that user's ID -> compare with the token owner.
//
// Args:
//   - `app`: The sign-in GitHub App (the code exchange uses its `client_id` and client secret)
//   - `r`: The callback request. Reads `code`, `state` and `error` from the address, and the state cookie
//   - `now`: The current time. Used to check expiry of the state
//
// Returns:
//   - The failure reason. Empty if the user is the token owner
//   - Details for the server log (when the reason alone is not enough). Never sent to the page
func FinishGitHubLogin(cfg *Config, app GitHubApp, r *http.Request, now time.Time) (GitHubLoginFailure, error) {
	ctx := r.Context() // The deadline that `Respond` set for this request
	query := r.URL.Query()
	if query.Get("error") != "" {
		return GitHubLoginCancelled, nil
	}
	if !isValidLoginState(cfg, r, query.Get("state"), now) {
		return GitHubLoginExpired, nil
	}
	token, err := exchangeGitHubLoginCode(ctx, cfg, app, query.Get("code"))
	if err != nil {
		return GitHubLoginFailed, err
	}
	userID, err := fetchGitHubUserID(ctx, cfg, token)
	if err != nil {
		return GitHubLoginFailed, err
	}
	owner, err := FetchTokenOwner(ctx, cfg)
	if err != nil {
		return GitHubLoginFailed, err
	}
	if !IsGitHubLoginAllowed(owner.ID, userID) {
		return GitHubLoginNotAllowed, fmt.Errorf("GitHub user %d is not the token owner %d", userID, owner.ID)
	}
	return "", nil
}

// Owner (GitHub account) of the server token. The only person who can sign in to the dashboard
type GitHubUser struct {
	ID    int64  `json:"id"`    // User ID. Names can change, so the sign-in check uses the ID
	Login string `json:"login"` // Username
}

// In-memory cache of the token owner, per instance. The key is `makeEnvCacheKey` (the same token gives the same key)
var tokenOwnerCache cache[GitHubUser]

// How long to keep the token owner. The owner of a token doesn't change, so it is kept for a long time
const tokenOwnerTTL = 24 * time.Hour

// Reads the owner of the server token from GitHub (`GET /user`).
// Only a successful result is kept in `tokenOwnerCache`. After a failure, it asks again next time.
//
// Returns:
//   - The token owner. A 502 `HTTPError` if GitHub returns a user with no ID or name
//
// NOTE: The owner of a token stays the same until the token changes.
// So it is kept, and GitHub is not asked again on every call.
// The owner's username can still change, so a kept owner can have an old username for up to `tokenOwnerTTL`.
func FetchTokenOwner(ctx context.Context, cfg *Config) (GitHubUser, error) {
	if owner, found := tokenOwnerCache.get(makeEnvCacheKey(cfg)); found {
		return owner, nil
	}
	res, err := CallGitHubAPI(ctx, cfg, "/user", GitHubAPIRequest{})
	if err != nil {
		return GitHubUser{}, err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return GitHubUser{}, err
	}
	var owner GitHubUser
	if err := DecodeGitHubAnswer(res, &owner); err != nil {
		return GitHubUser{}, err
	}
	if owner.ID <= 0 || owner.Login == "" {
		return GitHubUser{}, Errorf(http.StatusBadGateway, "GitHub returned no user for the token")
	}
	tokenOwnerCache.put(makeEnvCacheKey(cfg), owner, tokenOwnerTTL)
	return owner, nil
}

// Decides whether a GitHub user may enter the dashboard (whether they are the owner of the server token).
//
// Args:
//   - `tokenOwnerID`: User ID of the server token owner. 0 if unknown
//   - `githubUserID`: ID of the user who signed in on GitHub
//
// NOTE: The sign-in check is made only in this function.
// If the token owner is unknown (0), nobody is allowed.
// There must be no path where "unknown" turns into "anyone is allowed".
func IsGitHubLoginAllowed(tokenOwnerID int64, githubUserID int64) bool {
	return tokenOwnerID > 0 && githubUserID == tokenOwnerID
}

// Purpose label added in front of the signed data when signing the state cookie
//
// NOTE: The state cookie is signed with the same key as the session cookie (`sign`).
// The label makes the signed data different, so a state cookie signature can't be used as a session cookie.
const loginStatePurpose = "github-login-state:"

// Makes a new anti-forgery value (state).
//
// Args:
//   - `expires`: When the state expires
//
// Returns:
//   - `state`: Random value to add to the GitHub authorization address (32 bytes written as base64url)
//   - `cookieValue`: Value for the state cookie, `<state>.<expiry (Unix seconds)>.<signature>`
func newLoginState(cfg *Config, expires time.Time) (state string, cookieValue string, err error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(random)
	exp := strconv.FormatInt(expires.Unix(), 10)
	return state, state + "." + exp + "." + sign(loginStatePurpose+state+"."+exp, cfg), nil
}

// Decides whether the state in the callback request belongs to a sign-in started in this browser.
// It checks the signature and expiry of the state cookie (`<state>.<expiry>.<signature>`), and whether it matches the state in the callback address.
//
// Args:
//   - `r`: The callback request. The state cookie is read from it
//   - `state`: The state that came with the callback address
//   - `now`: The current time. Used to check expiry
//
// NOTE: This blocks attacks like login CSRF.
// In such an attack, someone makes you open the callback address of a sign-in they started, so their sign-in result sticks to your browser.
func isValidLoginState(cfg *Config, r *http.Request, state string, now time.Time) bool {
	cookie, err := r.Cookie(githubLoginStateCookie)
	if err != nil || state == "" {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return false
	}
	saved, exp, sig := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(sign(loginStatePurpose+saved+"."+exp, cfg))) { // `hmac.Equal`: compares in constant time, so the signature can't be guessed from the time it takes
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expires <= now.Unix() {
		return false
	}
	return hmac.Equal([]byte(saved), []byte(state))
}

// Exchanges the callback `code` for a GitHub user token (`POST <GitHubWeb>/login/oauth/access_token`).
//
// Args:
//   - `app`: The sign-in GitHub App (sends its `client_id` and client secret)
//   - `code`: The one-time code that came with the callback address
//
// Returns:
//   - The user token. It is used once to check that user's ID, then thrown away
//
// NOTE: Even when the exchange fails, GitHub answers 200 and puts the reason in `error` in the body (e.g. `bad_verification_code`, docs).
// So failure is detected by an empty token.
func exchangeGitHubLoginCode(ctx context.Context, cfg *Config, app GitHubApp, code string) (string, error) {
	if code == "" {
		return "", fmt.Errorf("callback has no code")
	}
	form := url.Values{"client_id": {app.ClientID}, "client_secret": {app.ClientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GitHubWeb+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // Without it, GitHub answers in form encoding (docs)
	res, err := httpClient.Do(req)
	if err != nil {
		return "", newGitHubUnreachableError(err)
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return "", err
	}
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := DecodeGitHubAnswer(res, &body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("GitHub token exchange failed: %s", body.Error)
	}
	return body.AccessToken, nil
}

// Reads the GitHub ID of a user with that user's token (`GET /user`).
//
// Args:
//   - `token`: The user token from `exchangeGitHubLoginCode`.
//     The sign-in App has no permissions, so this token can only read the public profile
func fetchGitHubUserID(ctx context.Context, cfg *Config, token string) (int64, error) {
	res, err := CallGitHubAPI(ctx, cfg, "/user", GitHubAPIRequest{Token: token})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return 0, err
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := DecodeGitHubAnswer(res, &user); err != nil {
		return 0, err
	}
	return user.ID, nil
}

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

// Shape of the `sign_in_app` value in the config file. The field order is the key order in the file
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
// The key is 32 bytes made from the server token (`deriveKey(token, "starkive-github-app")`), so it is AES-256.
//
// NOTE: The key comes from the token, so git history only holds ciphertext, which can't be decrypted without the token.
// When the token changes, the settings can't be decrypted, and the setup tool creates a new App.
func newGitHubAppCipher(cfg *Config) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(cfg, "starkive-github-app"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Makes the additional authenticated data (AAD) that ties the ciphertext to one App.
//
// Args:
//   - `appID`, `clientID`: ID and client ID of the App that the ciphertext belongs to
//
// NOTE: It decrypts only with the same values.
// So if this ciphertext is copied into another App's settings, it won't decrypt.
func makeGitHubAppAAD(appID int64, clientID string) []byte {
	return []byte(strconv.FormatInt(appID, 10) + "\n" + clientID)
}

// Turns a GitHub App into the shape stored in the config file. The client secret is encrypted
func encodeGitHubApp(cfg *Config, app GitHubApp) (*githubAppFile, error) {
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
func EncodeGitHubApp(cfg *Config, app GitHubApp) ([]byte, error) {
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
//     The setup tool uses them to point to the previous App
//   - The state. `GitHubAppUnreadable` if the ciphertext can't be decrypted, or `client_id` or `site_url` is empty
func decodeGitHubApp(cfg *Config, file *githubAppFile) (GitHubApp, GitHubAppStatus) {
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

// In-memory cache of the GitHub App settings, per instance. The key is `makeRepoCacheKey` (the same DB repository and token give the same key)
//
// NOTE: Sign-in start and callback are open before sign-in, so anyone can call them again and again.
// Keeping the settings means GitHub is not read each time.
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

// Puts the GitHub App settings and their state in `githubAppCache`. Usable settings are kept long, others short
func storeGitHubApp(cfg *Config, app GitHubApp, status GitHubAppStatus) {
	ttl := githubAppNotReadyTTL
	if status == GitHubAppReady {
		ttl = githubAppReadyTTL
	}
	githubAppCache.put(makeRepoCacheKey(cfg), githubAppRecord{app: app, status: status}, ttl)
}

// Reads the GitHub App settings from the config file in the DB repository. The result is kept in `githubAppCache` for a short time.
//
// Returns:
//   - The App and its state
//   - The read error if reading the file fails. `ErrConfigBroken` if the config file can't be decoded as JSON. Errors are not kept
func LoadGitHubApp(ctx context.Context, cfg *Config) (GitHubApp, GitHubAppStatus, error) {
	if record, found := githubAppCache.get(makeRepoCacheKey(cfg)); found {
		return record.app, record.status, nil
	}
	content, err := ReadFile(ctx, cfg, ConfigDBPath)
	if err != nil {
		return GitHubApp{}, "", err
	}
	file, err := decodeConfig(content)
	if err != nil {
		return GitHubApp{}, "", err
	}
	app, status := decodeGitHubApp(cfg, file.SignInApp)
	storeGitHubApp(cfg, app, status)
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
// NOTE: The "already exists" check is made inside `change` of `updateConfig`, that is, on the file right before the commit.
// So even if two setup tools run at once, only one gets in.
// When it replaces settings that can't be decrypted, the commit body says to delete the previous App.
// The private key is not received, so the server and the tool can't delete the App.
func SaveGitHubApp(ctx context.Context, cfg *Config, app GitHubApp) error {
	err := updateConfig(ctx, cfg, func(file *dbConfigFile) (CommitMessage, error) {
		previous, status := decodeGitHubApp(cfg, file.SignInApp)
		if status == GitHubAppReady {
			return CommitMessage{}, ErrGitHubAppExists
		}
		section, err := encodeGitHubApp(cfg, app)
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
	storeGitHubApp(cfg, app, GitHubAppReady) // Puts the new settings in the cache, so the read right after doesn't see the old state
	return nil
}
