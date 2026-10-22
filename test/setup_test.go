// Tests for the paths that are open before sign-in (starting sign-in, the call from the sync workflow, the deployment check).
// Also tests for saving the sign-in GitHub App settings (`core.SaveGitHubApp`), which the setup tool calls.
// Creating the App (manifest, code exchange, owner check) is tested by the unit tests of the setup tool package (`tools/setup`).
package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that API requests without sign-in, and calls from the sync workflow with a missing or wrong sync key, end with 401.
// They must not send a single request to GitHub.
//
// NOTE: Finding the DB repository requires asking GitHub.
// If these requests weren't refused before that, anyone could use up the server token's rate limit (5,000 per hour, docs) with repeated requests.
func TestUnauthenticatedRequestsDoNotCallGitHub(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", ""); rec.Code != 401 {
		t.Errorf("no session: %d", rec.Code)
	}
	if rec := call(api.HandleSyncArchive, "POST", "/api/sync-archive", "", ""); rec.Code != 401 {
		t.Errorf("sync now without session: %d", rec.Code)
	}
	if rec := call(api.HandleSyncStatus, "GET", "/api/sync-status", "", ""); rec.Code != 401 {
		t.Errorf("sync status without session: %d", rec.Code)
	}
	if rec := call(api.HandleLoginSession, "GET", "/api/login-session", "", ""); rec.Code != 401 {
		t.Errorf("sign-in status without session: %d", rec.Code)
	}
	for name, key := range map[string]string{"no key": "", "wrong key": "not-the-key"} {
		if rec := callScheduledSyncWithKey(key); rec.Code != 401 {
			t.Errorf("scheduled sync with %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if gh.requests != 0 { // The refusal happened before asking GitHub
		t.Errorf("unauthenticated requests reached GitHub %d times", gh.requests)
	}
}

// Tests the deployment check (`GET /api/check-deployment`).
//   - Without sign-in, it returns the format version and a proof.
//     The proof signs `nonce` with the repository ID and token in the environment variable (the setup tool computes the same value and compares)
//   - A different `nonce` gives a different proof, so an old response can't be reused. A malformed `nonce` gets 400
//   - It doesn't ask GitHub. It is open without sign-in, so repeated calls must not use the server token's rate limit
func TestCheckDeployment(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cfg := &core.EnvConfig{DBRepoID: testRepoID, DBRepoAccessToken: testToken}
	proofs := map[string]bool{}
	for _, nonce := range []string{"0123456789abcdef0123456789abcdef", "ffffffffffffffffffffffffffffffff"} {
		rec := call(api.HandleCheckDeployment, "GET", "/api/check-deployment?nonce="+nonce, "", "")
		var body struct {
			Format core.DBRepoFormatVersion `json:"format"`
			Proof  string                   `json:"proof"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 200 || body.Format != core.DBRepoFormat || body.Proof != core.SignDeploymentNonce(cfg, nonce) {
			t.Errorf("%s: %d %s", nonce, rec.Code, rec.Body)
		}
		proofs[body.Proof] = true
	}
	if len(proofs) != 2 {
		t.Errorf("different nonces gave the same proof")
	}
	for _, nonce := range []string{"", "short", "has space in it 0123456789", strings.Repeat("a", 129)} {
		if rec := call(api.HandleCheckDeployment, "GET", "/api/check-deployment?nonce="+url.QueryEscape(nonce), "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("nonce %q: %d %s", nonce, rec.Code, rec.Body)
		}
	}
	if gh.requests != 0 {
		t.Errorf("deployment check reached GitHub %d times", gh.requests)
	}
}

// Tests that sign-in starts only at the address registered in the App (the dashboard's canonical address).
//   - Starting at another address makes no state cookie, and returns 409 with the canonical address (`dashboard_url`).
//     The page then moves to that address
//   - At the canonical address, sign-in works to the end
//   - An address that differs only in upper case or the default port (`:443`) counts as the same address
//
// NOTE: GitHub sends the sign-in result back to the address registered in the App (docs).
// The browser sends the state cookie only to the address that set it.
// So a sign-in started at another address (such as another domain from Vercel) always fails.
func TestSignInStartsOnlyAtDashboardAddress(t *testing.T) {
	gh := setup(t, "", "")
	gh.files[core.ConfigDBPath] = makeTestConfigFile("https://starkive.example.com", encodeTestGitHubAppAt(t, "https://starkive.example.com"))
	// Starts at an address (`example.test`) other than the registered one.
	// If it went on, the callback would have no state cookie and fail, so it must not start
	rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", "")
	var body struct {
		DashboardURL string `json:"dashboard_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || readSetCookie(rec, "github_login_state") != "" || body.DashboardURL != "https://starkive.example.com" {
		t.Errorf("login from another address: %d %s %q", rec.Code, rec.Body, rec.Header().Values("Set-Cookie"))
	}

	setup(t, "", "") // Default settings, where the App's registered address is the test request's address (`testDashboardURL`)
	login(t)

	// The browser lowercases the host and drops the default port in its requests.
	// If the registered address were compared letter by letter,
	// the page would already be at that address with nowhere to move, and sign-in would be blocked forever
	gh = setup(t, "", "")
	gh.files[core.ConfigDBPath] = makeTestConfigFile("https://Example.TEST:443", encodeTestGitHubAppAt(t, "https://Example.TEST:443"))
	if rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", ""); rec.Code != http.StatusOK {
		t.Errorf("same address in another case: %d %s", rec.Code, rec.Body)
	}
}

// Tests that starting sign-in answers 409, asking to run the setup tool, when the App settings for sign-in can't be used.
// It checks three cases: no settings (before setup is done),
// settings encrypted with another token that can't be decrypted (after the token changed), and a broken config file.
// For a broken file, `core.LoadGitHubApp` reports `core.ErrDBConfigFileBroken`, so the setup tool can stop before it creates an App.
// (The sign-in response doesn't tell the causes apart.)
func TestSignInNeedsSetUpApp(t *testing.T) {
	ctx := context.Background()
	setupWithoutGitHubApp(t, "", "")
	if rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Sign-in isn't set up") {
		t.Errorf("no app: %d %s", rec.Code, rec.Body)
	}
	gh := setupWithoutGitHubApp(t, "", "")
	gh.files[core.ConfigDBPath] = makeTestConfigFile(testDashboardURL, encodeTestGitHubApp(t, "token-of-last-year", testUserID))
	if rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Sign-in isn't set up") {
		t.Errorf("unreadable app: %d %s", rec.Code, rec.Body)
	}
	gh = setupWithoutGitHubApp(t, "", "")
	gh.files[core.ConfigDBPath] = "{ broken"
	if rec := call(api.HandleLoginSession, "POST", "/api/login-session", "", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Sign-in isn't set up") {
		t.Errorf("broken config: %d %s", rec.Code, rec.Body)
	}
	if _, _, err := core.LoadGitHubApp(ctx, openTestDBRepo(t)); !errors.Is(err, core.ErrDBConfigFileBroken) {
		t.Errorf("broken config: %v", err)
	}
}

// Tests saving the App settings (`core.SaveGitHubApp`), which the setup tool calls.
//   - Settings made with another token (can't be decrypted) are replaced with the new App.
//     The commit body says to delete the previous App
//   - Other values in the config file (dashboard address, sync interval) stay as they are. The client secret isn't stored in plain text
//   - If usable settings exist, it returns `core.ErrGitHubAppExists` and writes nothing
//   - The check uses the file right before the commit.
//     If another run saved an App first in the meantime, it doesn't overwrite it
func TestSaveGitHubApp(t *testing.T) {
	ctx := context.Background()
	newApp := core.GitHubApp{AppID: 99, Slug: "starkive-user4242-abc123", OwnerID: testUserID, ClientID: testClientID, ClientSecret: testClientSecret, SiteURL: testDashboardURL}

	gh := setupWithoutGitHubApp(t, "", "")
	gh.files[core.ConfigDBPath] = makeTestConfigFile(testDashboardURL, encodeTestGitHubApp(t, "token-of-last-year", testUserID))
	repo := openTestDBRepo(t)
	if err := core.SaveGitHubApp(ctx, repo, newApp); err != nil {
		t.Fatal(err)
	}
	file := gh.readFile(core.ConfigDBPath)
	if strings.Contains(file, testClientSecret) || !strings.Contains(file, `"slug": "starkive-user4242-abc123"`) ||
		!strings.Contains(file, `"site_url": "`+testDashboardURL+`"`) || !strings.Contains(file, `"dashboard_url": "`+testDashboardURL+`"`) || !strings.Contains(file, `"interval_days": 1`) {
		t.Errorf("saved file = %s", file)
	}
	if !slices.ContainsFunc(gh.commits, func(c string) bool { return strings.Contains(c, "Replaced starkive-test") }) {
		t.Errorf("commits = %q", gh.commits)
	}
	login(t) // The saved App can really be used to sign in
	another := newApp
	another.Slug = "starkive-user4242-def456"
	if err := core.SaveGitHubApp(ctx, repo, another); !errors.Is(err, core.ErrGitHubAppExists) || !strings.Contains(gh.readFile(core.ConfigDBPath), `"slug": "starkive-user4242-abc123"`) {
		t.Errorf("second save: %v", err)
	}

	gh = setupWithoutGitHubApp(t, "", "")
	first := makeTestConfigFile(testDashboardURL, encodeTestGitHubApp(t, testToken, testUserID))
	// After the settings were read and right before the commit, another run saved a usable App first
	gh.mu.Lock()
	gh.interfereOnce = func(files map[string]string) { files[core.ConfigDBPath] = first }
	gh.mu.Unlock()
	if err := core.SaveGitHubApp(ctx, openTestDBRepo(t), newApp); !errors.Is(err, core.ErrGitHubAppExists) || gh.readFile(core.ConfigDBPath) != first {
		t.Errorf("app that appeared meanwhile: %v, file %q", err, gh.readFile(core.ConfigDBPath))
	}
}
