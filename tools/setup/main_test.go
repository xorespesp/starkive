// Unit tests for the setup tool: archive name parsing, the repository creation and token links,
// the dashboard address check, secret encryption, the App name and manifest, the owner check in the code exchange,
// the permission checks, and the contract between the workflow and the server.
// A fake server from `httptest` stands in for GitHub.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Guards the cleanup of the dashboard address.
// It accepts only https (http only for a local development server).
// It removes a trailing `/`, makes the host lower case, and drops the default port,
// so the address has the same shape as what the browser requests.
// If they differ, the server's comparison with the request address (`core.ReadSiteURL`) at sign-in always fails, and sign-in is blocked
func TestNormalizeDashboardURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://starkive.vercel.app":       "https://starkive.vercel.app",
		"https://starkive.vercel.app/":      "https://starkive.vercel.app",
		"HTTPS://Starkive-You.Vercel.App":   "https://starkive-you.vercel.app",
		"https://starkive.vercel.app:443":   "https://starkive.vercel.app",
		"https://starkive.example.com:8443": "https://starkive.example.com:8443",
		"http://localhost:3000":             "http://localhost:3000",
		"http://LocalHost:3000":             "http://localhost:3000",
	} {
		if got, err := normalizeDashboardURL(input); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"starkive.vercel.app", "http://starkive.vercel.app", "https://starkive.vercel.app/app", "https://x.vercel.app?a=1"} {
		if got, err := normalizeDashboardURL(input); err == nil {
			t.Errorf("%q: got %q, want an error", input, got)
		}
	}
}

// Guards the cleanup of the archive name.
// It trims spaces at both ends and makes the name lower case.
// It accepts only lower-case letters and digits joined by single hyphens.
// (It rejects `.` and `_`, which can't be in one part of an address, and `-` at either end, and `--`.)
// It accepts up to 39 characters, the limit of a GitHub username
func TestNormalizeArchiveName(t *testing.T) {
	for input, want := range map[string]string{
		"me":                    "me",
		" Me ":                  "me",
		"work-2":                "work-2",
		strings.Repeat("a", 39): strings.Repeat("a", 39),
	} {
		if got, err := normalizeArchiveName(input); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-me", "me-", "a--b", "my_stars", "my.stars", "a b", strings.Repeat("a", 40)} {
		if got, err := normalizeArchiveName(input); err == nil {
			t.Errorf("%q: got %q, want an error", input, got)
		}
	}
}

// Guards the repository creation link.
// It is the page that creates a private repository with that name in the signed-in account (`@me`)
func TestBuildNewDBRepoURL(t *testing.T) {
	u, err := url.Parse(buildNewDBRepoURL("me-starkive-db"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "github.com" || u.Path != "/new" || q.Get("owner") != "@me" || q.Get("name") != "me-starkive-db" || q.Get("visibility") != "private" {
		t.Errorf("url: %s", u)
	}
}

// Guards the token link.
// The name is the repository name, cut at GitHub's limit (40 characters), and the description has the repository name.
// The needed permissions and the expiration (none) are filled in.
// The resource owner (`target_name`) isn't included
func TestBuildTokenURL(t *testing.T) {
	u, err := url.Parse(buildTokenURL("me-starkive-db"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"name": "me-starkive-db", "description": "starkive dashboard: me-starkive-db and stars", "expires_in": "none",
		"contents": "write", "workflows": "write", "secrets": "write", "actions": "write", "starring": "read",
	}
	for key, value := range want {
		if q.Get(key) != value {
			t.Errorf("%s: got %q, want %q", key, q.Get(key), value)
		}
	}
	if u.Host != "github.com" || u.Path != "/settings/personal-access-tokens/new" || q.Has("target_name") || len(q) != len(want) {
		t.Errorf("url: %s", u)
	}
	long, _ := url.Parse(buildTokenURL(strings.Repeat("a", 39) + dbRepoSuffix))
	if name := long.Query().Get("name"); len(name) != maxTokenNameLength {
		t.Errorf("long name: %q", name)
	}
}

// Guards the dashboard check.
// The deployment side runs the real server code (`api.HandleCheckDeployment`) with only the environment variable changed.
// So it also tests that the server's answer and the setup tool's check keep the same contract.
//   - It passes if deployed with the repository ID and token being set up
//   - It rejects a deployment with another token or another repository with `errDashboardOtherValue`.
//     It rejects a missing environment variable (showing that text as is) and a different format version with other errors
//   - It rejects a redirect to the Vercel sign-in page, another page, and an old dashboard without this API
func TestCheckDashboard(t *testing.T) {
	cfg := &core.Config{DBRepoID: 5150, DBRepoAccessToken: "token_4242"} // Repository and token being set up
	cases := []struct {
		name    string
		env     string           // The deployment's environment variable `STARKIVE_DB_REPO_ACCESS_KEY`. Used only in cases answered by the real server code
		handler http.HandlerFunc // What answers at the dashboard address
		wantErr string
		other   bool // Whether it must be `errDashboardOtherValue` (deployed with another value)
	}{
		{"this setup", core.FormatDBRepoAccessKey(5150, "token_4242"), api.HandleCheckDeployment, "", false},
		{"another token", core.FormatDBRepoAccessKey(5150, "token_9999"), api.HandleCheckDeployment, "deployed with a different", true},
		{"another repository", core.FormatDBRepoAccessKey(6000, "token_4242"), api.HandleCheckDeployment, "deployed with a different", true},
		{"missing environment variable", "", api.HandleCheckDeployment, "missing or malformed", false},
		{"another format", "", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"format":99,"proof":"x"}`))
		}, "format 99", false},
		{"behind Vercel sign-in", "", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://vercel.com/login", http.StatusTemporaryRedirect)
		}, "redirects", false},
		{"another site", "", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html></html>"))
		}, "doesn't look like", false},
		{"older dashboard", "", http.NotFound, "doesn't look like", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(core.DBRepoAccessKeyEnv, c.env) // Goes back to the original value when this case ends
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			err := checkDashboard(cfg, srv.URL)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("got %v, want an error with %q", err, c.wantErr)
			case errors.Is(err, errDashboardOtherValue) != c.other:
				t.Errorf("got %v, deployed with another value: %v", err, c.other)
			}
		})
	}
}

// A `ui` that gives prepared answers in order.
// To a waiting prompt, it always answers that it will type the value in.
// When the answers run out, it returns an error, as if the input ended
type scriptedUI struct {
	answers []string // Answers not given yet
	said    []string // Lines shown
}

// Methods that only show things. Only `say` lines are kept in `said`. The rest are dropped
func (s *scriptedUI) step(n int)                                    {}
func (s *scriptedUI) say(line string)                               { s.said = append(s.said, line) }
func (s *scriptedUI) note(line string)                              {}
func (s *scriptedUI) value(name string, value string)               {}
func (s *scriptedUI) link(label string, target string)              {}
func (s *scriptedUI) open(label string, target string)              {}
func (s *scriptedUI) done(text string, label string, target string) {}

// Gives the next answer, like `ask`
func (s *scriptedUI) askSecret(prompt string) (string, error) { return s.ask(prompt, "") }

// Answers right away, without waiting, that it will type the value in
func (s *scriptedUI) waitForValue(waiting string, prompt string, received <-chan string) (string, waitResult, error) {
	return "", switchedToTyping, nil
}

// Gives the next answer. An error if the answers ran out
func (s *scriptedUI) ask(prompt string, hint string) (string, error) {
	if len(s.answers) == 0 {
		return "", errors.New("no more input")
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

// Guards the path where the dashboard address is typed in.
// The registered address is deployed with another value.
// The typed address is a dashboard that confirms this setup's value.
//   - Without a registered canonical address, it checks the typed address and accepts it
//   - With a registered canonical address, it doesn't accept the typed address even if it confirms, and shows why.
//     (If it accepted it, an unconfirmed canonical address would be recorded.)
func TestAskDashboardURLKeepsRegisteredSite(t *testing.T) {
	cfg := &core.Config{DBRepoID: 5150, DBRepoAccessToken: "token_4242"}
	t.Setenv(core.DBRepoAccessKeyEnv, core.FormatDBRepoAccessKey(5150, "token_4242"))
	typed := httptest.NewServer(http.HandlerFunc(api.HandleCheckDeployment)) // Another dashboard, deployed with this setup's value
	defer typed.Close()
	other := &core.Config{DBRepoID: 5150, DBRepoAccessToken: "token_9999"}
	registered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // The registered dashboard, deployed with another token
		core.WriteJSON(w, http.StatusOK, map[string]any{"format": core.DBRepoFormat, "proof": core.SignDeploymentNonce(other, r.URL.Query().Get("nonce"))})
	}))
	defer registered.Close()

	u := &scriptedUI{answers: []string{typed.URL}}
	if site, err := askDashboardURL(u, cfg, registered.URL, false); err != nil || site != typed.URL {
		t.Errorf("without a registered site: got %q %v, want %q", site, err, typed.URL)
	}

	u = &scriptedUI{answers: []string{typed.URL}}
	if site, err := askDashboardURL(u, cfg, registered.URL, true); err == nil {
		t.Errorf("with a registered site: took %q", site)
	}
	if len(u.said) != 1 || !strings.Contains(u.said[0], "registered for "+registered.URL) {
		t.Errorf("said: %q", u.said)
	}
}

// Guards secret encryption.
// A value sealed with the public key opens to the original value with the matching private key.
// (This is what GitHub does when it gives a secret to a workflow.)
// It rejects a public key that isn't 32 bytes
func TestSealSecret(t *testing.T) {
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealSecret(base64.StdEncoding.EncodeToString(public[:]), "sync-key-value")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	opened, ok := box.OpenAnonymous(nil, raw, public, private)
	if !ok || string(opened) != "sync-key-value" {
		t.Errorf("opened %q %v", opened, ok)
	}
	if _, err := sealSecret("not-a-key", "x"); err == nil {
		t.Error("accepted a bad public key")
	}
}

// Guards the App name rule.
//   - The name is the DB repository name with a random suffix.
//     So even with several archives, you can see which App belongs to which repository
//   - A GitHub App name is unique across GitHub and up to 34 characters.
//     So a long repository name is cut to fit in 34 characters, and a hyphen at the cut end doesn't create `--`.
//     The suffix makes the name different each time
func TestGitHubAppName(t *testing.T) {
	for repo, prefix := range map[string]string{
		"me/me-starkive-db":                            "me-starkive-db-",
		"me/work-starkive-db":                          "work-starkive-db-",
		"me/" + strings.Repeat("a", 39) + dbRepoSuffix: strings.Repeat("a", 27) + "-",
		"me/abcdefghijklmnopqrstuvwxyz-starkive-db":    "abcdefghijklmnopqrstuvwxyz-",
	} {
		name, err := newGitHubAppName(repo)
		if err != nil || len(name) > 34 || !strings.HasPrefix(name, prefix) || strings.Contains(name, "--") {
			t.Errorf("%q: %q %v", repo, name, err)
		}
	}
	a, _ := newGitHubAppName("me/me-starkive-db")
	b, _ := newGitHubAppName("me/me-starkive-db")
	if a == b {
		t.Errorf("two names are the same: %s", a)
	}
}

// Guards the manifest.
// The homepage is the dashboard address, and the sign-in callback is `/api/login-session` under it.
// The address to return to after creation is this tool's localhost.
// It is a private App with no permissions and no webhook. The description has the DB repository
func TestGitHubAppManifest(t *testing.T) {
	encoded, err := newGitHubAppManifest("me/me-starkive-db", "https://starkive.example.com", "http://localhost:5000/callback")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(encoded), &m)
	callbacks, _ := m["callback_urls"].([]any)
	if m["url"] != "https://starkive.example.com" || m["redirect_url"] != "http://localhost:5000/callback" ||
		len(callbacks) != 1 || callbacks[0] != "https://starkive.example.com/api/login-session" ||
		m["public"] != false || len(m["default_permissions"].(map[string]any)) != 0 || m["hook_attributes"] != nil ||
		!strings.Contains(m["description"].(string), "me/me-starkive-db") {
		t.Errorf("manifest = %s", encoded)
	}
}

// Guards the code exchange.
// The request needs no authentication, so no token is sent.
// It accepts the App only if the App owner is the token owner.
// It keeps the homepage registered in the App (`external_url`) in `SiteURL`, without a trailing `/`
func TestCreateGitHubAppFromCode(t *testing.T) {
	var conversionAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"id":4242,"login":"me"}`))
		case strings.HasPrefix(r.URL.Path, "/app-manifests/"):
			conversionAuth = append(conversionAuth, r.Header.Get("Authorization"))
			owner := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app-manifests/owner-"), "/conversions")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":99,"slug":"starkive-me-abc123","external_url":"https://starkive.example.com/","client_id":"Iv23x","client_secret":"s","pem":"k","owner":{"id":%s}}`, owner)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &core.Config{DBRepoAccessToken: "token-4242", GitHubAPI: srv.URL, GitHubWeb: srv.URL}

	app, err := createGitHubAppFromCode(cfg, "owner-4242")
	if err != nil || app.OwnerID != 4242 || app.SiteURL != "https://starkive.example.com" || app.ClientSecret != "s" {
		t.Errorf("token owner's app: %+v %v", app, err)
	}
	if _, err := createGitHubAppFromCode(cfg, "owner-777"); !errors.Is(err, errGitHubAppWrongOwner) {
		t.Errorf("another account's app: %v", err)
	}
	for _, auth := range conversionAuth {
		if auth != "" {
			t.Errorf("sent a token with the code exchange: %q", auth)
		}
	}
}

// Guards the Workflows permission check.
// It only tries to create a tree with the workflow, and sends no request that moves the branch (PATCH). There is no commit.
// Without the permission, it returns GitHub's 403 as an error with the needed permission (`workflows=write`)
func TestCheckWorkflowsPermission(t *testing.T) {
	for _, denied := range []bool{false, true} {
		var methods []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method+" "+r.URL.Path)
			switch r.URL.Path {
			case "/repos/me/archive/git/ref/heads/main":
				w.Write([]byte(`{"object":{"sha":"c1"}}`))
			case "/repos/me/archive/git/commits/c1":
				w.Write([]byte(`{"tree":{"sha":"t1"}}`))
			case "/repos/me/archive/git/trees":
				if denied {
					w.Header().Set("X-Accepted-GitHub-Permissions", "contents=write; contents=write,workflows=write")
					http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"sha":"t2"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		cfg := &core.Config{DBRepoAccessToken: "token_4242", DBRepoFullName: "me/archive", DBRepoBranch: "main", GitHubAPI: srv.URL}
		err := checkWorkflowsPermission(cfg)
		srv.Close()
		if denied != (err != nil) || (denied && !strings.Contains(err.Error(), "workflows=write")) {
			t.Errorf("denied=%v: %v", denied, err)
		}
		if strings.Contains(strings.Join(methods, "|"), "PATCH") || len(methods) != 3 {
			t.Errorf("denied=%v: requests %q", denied, methods)
		}
	}
}

// Guards the initial files of the DB repository (fake GitHub).
//   - Missing files (the README and the empty file that keeps `db/`) are added in one commit, `feat: Add initial files`
//   - A README that is already there isn't overwritten. Only the missing file is added
//   - If nothing is missing, it sends no write request (no commit)
func TestAddInitialFiles(t *testing.T) {
	for _, c := range []struct {
		name     string
		existing []string          // Files already in the repository
		want     map[string]string // Files the commit must write: path -> content. Empty means no commit
	}{
		{"new repository", nil, map[string]string{dbRepoReadmePath: dbRepoReadmeContent, dbFolderKeepPath: ""}},
		{"README kept", []string{dbRepoReadmePath}, map[string]string{dbFolderKeepPath: ""}},
		{"nothing missing", []string{dbRepoReadmePath, dbFolderKeepPath}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var written map[string]string // Files in the tree that was made: path -> content
			var message string            // Message of the commit that was made
			var writes int                // Number of write requests (POST, PATCH)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				switch {
				case r.URL.Path == "/repos/me/archive/git/ref/heads/main":
					w.Write([]byte(`{"object":{"sha":"c1"}}`))
				case r.URL.Path == "/repos/me/archive/git/commits/c1":
					w.Write([]byte(`{"tree":{"sha":"t1"}}`))
				case strings.HasPrefix(r.URL.Path, "/repos/me/archive/contents/"):
					for _, path := range c.existing {
						if r.URL.Path == "/repos/me/archive/contents/"+path {
							w.Write([]byte("existing"))
							return
						}
					}
					http.NotFound(w, r)
				case r.URL.Path == "/repos/me/archive/git/trees":
					var body struct {
						Tree []struct {
							Path    string `json:"path"`
							Content string `json:"content"`
						} `json:"tree"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					written = map[string]string{}
					for _, entry := range body.Tree {
						written[entry.Path] = entry.Content
					}
					w.Write([]byte(`{"sha":"t2"}`))
				case r.URL.Path == "/repos/me/archive/git/commits":
					var body struct {
						Message string `json:"message"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					message = body.Message
					w.Write([]byte(`{"sha":"c2"}`))
				case r.URL.Path == "/repos/me/archive/git/refs/heads/main":
					w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg := &core.Config{DBRepoAccessToken: "token_4242", DBRepoFullName: "me/archive", DBRepoBranch: "main", GitHubAPI: srv.URL}
			if err := addInitialFiles(cfg); err != nil {
				t.Fatal(err)
			}
			if c.want == nil {
				if writes != 0 {
					t.Errorf("sent %d write requests with nothing missing", writes)
				}
				return
			}
			if fmt.Sprint(written) != fmt.Sprint(c.want) || message != "feat: Add initial files" {
				t.Errorf("wrote %q with message %q, want %q", written, message, c.want)
			}
		})
	}
}

// Guards the contract between the workflow and the server.
// Only the server's scheduled response `status` (the return values of `core.RunScheduledSync`) counts as success.
// It sends the key from the `core.SyncKeySecretName` secret in `Authorization: Bearer`, and reads `dashboard_url` from `config.json`.
// There is no way to end in success without the dashboard's answer (`exit 0`)
func TestSyncWorkflowContract(t *testing.T) {
	for _, status := range []string{"not_due", "synced"} {
		if !strings.Contains(syncWorkflowContent, `"200 `+status+`"`) {
			t.Errorf("workflow doesn't accept %q", status)
		}
	}
	if !strings.Contains(syncWorkflowContent, "${{ secrets."+core.SyncKeySecretName+" }}") || !strings.Contains(syncWorkflowContent, `"Authorization: Bearer $SYNC_KEY"`) {
		t.Errorf("workflow doesn't send the sync key from the %s secret", core.SyncKeySecretName)
	}
	if !strings.Contains(syncWorkflowContent, "contents/"+core.ConfigDBPath) || !strings.Contains(syncWorkflowContent, ".dashboard_url") {
		t.Errorf("workflow doesn't read dashboard_url from %s", core.ConfigDBPath)
	}
	if strings.Contains(syncWorkflowContent, "exit 0") {
		t.Errorf("workflow has a path that ends in success without the dashboard's answer")
	}
}

// Reports that the workflow content changed.
// It pins the SHA-256 of the content, so any change fails here.
// If the change alters the contract with the server (config key, secret name, request shape, the `status` that counts as success),
// raise `core.DBRepoFormat`.
// (Then a repository with a server and a workflow of different versions stops with a clear error.)
// After that, update the value below to the new hash
func TestSyncWorkflowChangeIsReviewed(t *testing.T) {
	const reviewed = "f5a5d731966d118d902f431ec1a308eaaf765f8c17032cd80f8294ab0615782c"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(syncWorkflowContent))); got != reviewed {
		t.Errorf("the workflow changed (sha256 %s). Decide whether core.DBRepoFormat must go up, then update the hash in this test", got)
	}
}
