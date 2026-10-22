// The parts of the sign-in GitHub App manifest flow that talk to GitHub.
// Building the manifest and the App name, the address of the App creation page, and turning the returned code into App data.
// The localhost server paths that drive this flow are added by `createGitHubApp` in `app.go`.
// The storage format and encryption of the created App are in `core.SaveGitHubApp`, shared with the server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xorespesp/starkive/core"
)

// App manifest sent to GitHub.
// The App is only for sign-in, so it is a private App with no permissions and no webhook
type githubAppManifest struct {
	Name               string            `json:"name"`                // App name (`newGitHubAppName`)
	URL                string            `json:"url"`                 // Homepage. It is the dashboard address, and comes back as `external_url` in the creation response (measured)
	Description        string            `json:"description"`         // Description shown on the App's GitHub page
	RedirectURL        string            `json:"redirect_url"`        // Address GitHub sends the browser back to with a code after creating the App (this tool's localhost server)
	CallbackURLs       []string          `json:"callback_urls"`       // Sign-in callback (the dashboard's `/api/login-session`)
	Public             bool              `json:"public"`              // `false`: a private App that can be installed only on the account that created it (docs)
	DefaultPermissions map[string]string `json:"default_permissions"` // Empty: no repository or account permissions
}

// Longest GitHub App name (GitHub's limit, docs)
const maxGitHubAppNameLength = 34

// Builds the name of the sign-in GitHub App for the DB repository `dbFullName` (`owner/repo`): `<repository name>-<6 random hex characters>`.
// It starts with the repository name, like the token name (`buildTokenURL`).
// So even with several archives, the GitHub App list shows which App belongs to which repository.
//
// NOTE: A GitHub App name is unique across all of GitHub and can be up to 34 characters (docs).
// A random suffix is added every time, so names don't clash when the App is made again or someone else took the name first.
// The repository name is cut at 27 characters, leaving room for the 7-character suffix ("-xxxxxx").
func newGitHubAppName(dbFullName string) (string, error) {
	suffix := make([]byte, 3) // 3 bytes = 6 hex characters
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	_, repo, _ := strings.Cut(dbFullName, "/")
	label := strings.TrimRight(repo[:min(len(repo), maxGitHubAppNameLength-7)], "-") // If the cut ends in a hyphen, removes it so the name doesn't get `--`
	return label + "-" + hex.EncodeToString(suffix), nil
}

// Builds the GitHub App manifest as a JSON string.
//
// Args:
//   - `dbFullName`: The DB repository (`owner/repo`) of the dashboard that signs in with this App. Used in the App name and description
//   - `site`: The dashboard address. It becomes the App's homepage, and the sign-in callback is `/api/login-session` under it
//   - `redirectURL`: Address GitHub sends the browser back to with a code after creating the App (this tool's localhost server)
func newGitHubAppManifest(dbFullName string, site string, redirectURL string) (string, error) {
	name, err := newGitHubAppName(dbFullName)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(githubAppManifest{
		Name: name, URL: site, Description: "Sign-in for the starkive dashboard of " + dbFullName + " at " + site,
		RedirectURL: redirectURL, CallbackURLs: []string{site + "/api/login-session"},
		Public: false, DefaultPermissions: map[string]string{},
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// Builds the address of GitHub's App creation page, where the browser form-POSTs the manifest.
//
// Args:
//   - `state`: A value GitHub adds back unchanged when it sends the browser back after creating the App.
//     Used to check that the returning request belongs to this run
func buildGitHubAppCreateURL(cfg *core.EnvConfig, state string) string {
	return cfg.GitHubWeb + "/settings/apps/new?" + url.Values{"state": {state}}.Encode()
}

// Error for an App created under an account other than the token owner.
// The dashboard lets only the token owner sign in, so nobody could sign in with another account's App
var errGitHubAppWrongOwner = errors.New("the GitHub App was created under a different GitHub account than the owner of the token. " +
	"Delete it on GitHub, sign in to GitHub as the token owner, and try again")

// HTTP client for the code exchange request.
// The default client has no timeout, so the tool would hang if GitHub didn't answer.
// This one has a 20-second limit
var conversionClient = &http.Client{Timeout: 20 * time.Second}

// Turns the code GitHub returned after creating the App into App data (`POST /app-manifests/{code}/conversions`).
//
// Args:
//   - `code`: The one-time code GitHub added to `redirect_url`
//
// Returns:
//   - The created App. `errGitHubAppWrongOwner` if the App's owner isn't the token owner.
//     An error if the response lacks a needed value
//
// NOTE: This request needs no authentication (docs), so no token is sent.
// That's why it builds the request itself instead of using `core.CallGitHubAPI`, which always adds the token.
// The private key (`pem`) and webhook secret in the response aren't used, so they aren't read.
// The homepage (`external_url`) is put in `SiteURL` in the shape the server compares with the request address (`core.NormalizeSiteURL`).
func createGitHubAppFromCode(ctx context.Context, cfg *core.EnvConfig, code string) (core.GitHubApp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GitHubAPI+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return core.GitHubApp{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "starkive-setup")
	res, err := conversionClient.Do(req)
	if err != nil {
		return core.GitHubApp{}, err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubResponse(res); err != nil {
		return core.GitHubApp{}, err
	}
	var created struct {
		ID           int64  `json:"id"`            // App ID
		Slug         string `json:"slug"`          // App address name (`github.com/settings/apps/<slug>`)
		ClientID     string `json:"client_id"`     // Public value put in the sign-in address
		ClientSecret string `json:"client_secret"` // Secret used for the sign-in code exchange
		ExternalURL  string `json:"external_url"`  // The manifest's `url` (homepage) (measured)
		Owner        struct {
			ID int64 `json:"id"` // User ID of the account that created the App
		} `json:"owner"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		return core.GitHubApp{}, err
	}
	site := core.NormalizeSiteURL(created.ExternalURL)
	if created.ID <= 0 || created.ClientID == "" || created.ClientSecret == "" || created.Owner.ID <= 0 || site == "" {
		return core.GitHubApp{}, errors.New("GitHub returned an incomplete app")
	}
	owner, err := core.FetchTokenOwner(ctx, cfg)
	if err != nil {
		return core.GitHubApp{}, err
	}
	if created.Owner.ID != owner.ID {
		return core.GitHubApp{}, errGitHubAppWrongOwner
	}
	return core.GitHubApp{AppID: created.ID, Slug: created.Slug, OwnerID: created.Owner.ID, ClientID: created.ClientID,
		ClientSecret: created.ClientSecret, SiteURL: site}, nil
}
