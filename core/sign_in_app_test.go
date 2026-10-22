// Unit tests for encrypting and decoding the sign-in GitHub App settings (`sign_in_app.go`)
package core

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Tests the encryption of the client secret in the GitHub App settings.
//   - The client secret is not left as plain text in the file content.
//     Decoding with the same token gives the original App
//   - Another token can't decrypt it, but the public value (`Slug`) is still read
//   - Settings with the same ciphertext but a changed `client_id` don't decrypt (this blocks swapping in another App's values)
//   - Settings with an empty `site_url`, no settings, and unreadable JSON are unreadable, missing and unreadable, in that order
func TestGitHubAppFileEncryption(t *testing.T) {
	cfg := &EnvConfig{DBRepoAccessToken: "token-a"}
	app := GitHubApp{AppID: 7, Slug: "starkive-me-abc123", OwnerID: 42, ClientID: "Iv23client", ClientSecret: "very-secret-value", SiteURL: "https://starkive.example.com"}
	content, err := EncodeGitHubAppConfigValue(cfg, app)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte(app.ClientSecret)) {
		t.Fatalf("file content = %s", content)
	}

	if got, status := decodeAppJSON(cfg, content); status != GitHubAppReady || got != app {
		t.Errorf("round trip: %+v %s", got, status)
	}
	if got, status := decodeAppJSON(&EnvConfig{DBRepoAccessToken: "token-b"}, content); status != GitHubAppUnreadable || got.ClientSecret != "" || got.Slug != app.Slug {
		t.Errorf("other token: %+v %s", got, status)
	}

	var file map[string]any
	json.Unmarshal(content, &file)
	file["client_id"] = "Iv23other" // Keeps the ciphertext and changes only the App
	swapped, _ := json.Marshal(file)
	if _, status := decodeAppJSON(cfg, swapped); status != GitHubAppUnreadable {
		t.Errorf("swapped client id: %s", status)
	}
	file["client_id"] = app.ClientID // Restores the changed `client_id` and empties only `site_url`
	file["site_url"] = ""            // Without the canonical address, sign-in can't start, so these settings are unusable
	noSite, _ := json.Marshal(file)
	if _, status := decodeAppJSON(cfg, noSite); status != GitHubAppUnreadable {
		t.Errorf("no site url: %s", status)
	}
	if _, status := decodeAppJSON(cfg, nil); status != GitHubAppMissing {
		t.Errorf("missing file: %s", status)
	}
	if _, status := decodeAppJSON(cfg, []byte("not json")); status != GitHubAppUnreadable {
		t.Errorf("broken file: %s", status)
	}
}

// Decodes the `sign_in_app` value (JSON) of the config file and passes it to `decodeGitHubApp`.
//
// Args:
//   - `content`: JSON of the `sign_in_app` value. `nil` means there are no settings
//
// Returns:
//   - The decoded App and its state. Unreadable JSON gives `GitHubAppUnreadable`
func decodeAppJSON(cfg *EnvConfig, content []byte) (GitHubApp, GitHubAppStatus) {
	if content == nil {
		return decodeGitHubApp(cfg, nil)
	}
	var file githubAppFile
	if err := json.Unmarshal(content, &file); err != nil {
		return GitHubApp{}, GitHubAppUnreadable
	}
	return decodeGitHubApp(cfg, &file)
}
