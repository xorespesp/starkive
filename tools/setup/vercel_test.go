// Tests for the Vercel deployment step: the commit to deploy (remote address parsing, the GitHub check),
// the values in the Deploy link, the dashboard name, and checking in the background for the deployment to finish.
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Guards parsing of git remote addresses.
// The GitHub address shapes that `git clone` and SSH settings leave become owner/repo.
// These shapes are https, https with a user name, the `git@` short form, `ssh://`, `ssh.github.com` on port 443,
// an upper-case host, and a trailing `.git` or `/`.
// It rejects other hosts, local paths, addresses with a path inside the repository, and names that climb up the path (`me/..`)
func TestParseGitRemoteURL(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/me/starkive":               "me/starkive",
		"https://github.com/me/starkive.git\n":         "me/starkive",
		"https://me@github.com/me/starkive/":           "me/starkive",
		"git@github.com:me/starkive.git":               "me/starkive",
		"ssh://git@github.com/me/starkive.git":         "me/starkive",
		"ssh://git@ssh.github.com:443/me/starkive.git": "me/starkive",
		"https://GitHub.com/me/starkive":               "me/starkive",
	} {
		if got, err := parseGitRemoteURL(remote); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", remote, got, err, want)
		}
	}
	for _, remote := range []string{"", "https://gitlab.com/me/starkive", `C:\src\starkive`, "/home/me/starkive", "https://github.com/me/starkive/tree/main", "https://github.com/me/.."} {
		if got, err := parseGitRemoteURL(remote); err == nil {
			t.Errorf("%q: got %q, want an error", remote, got)
		}
	}
}

// Guards the GitHub check of the commit to deploy (fake GitHub).
// It passes if the public source repository has the commit.
// If the repository can't be seen (private or missing), the error says a public repository is needed.
// If the commit is missing (404, 422), the error says to push.
// Other failures give GitHub's error
func TestCheckSourceRepoCommit(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	cases := []struct {
		name    string
		repo    int    // Status code of the repository lookup
		commit  int    // Status code of the commit lookup
		wantErr string // Empty means it passes
	}{
		{"public repository with the commit", http.StatusOK, http.StatusOK, ""},
		{"private or missing repository", http.StatusNotFound, http.StatusOK, "public repository"},
		{"unpushed commit", http.StatusOK, http.StatusNotFound, "git push"},
		{"commit GitHub doesn't know", http.StatusOK, http.StatusUnprocessableEntity, "git push"},
		{"GitHub failing", http.StatusServiceUnavailable, http.StatusOK, "503"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Errorf("sent credentials to %s", r.URL.Path)
				}
				switch r.URL.Path {
				case "/repos/me/starkive":
					w.WriteHeader(c.repo)
				case "/repos/me/starkive/commits/" + sha:
					w.WriteHeader(c.commit)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				w.Write([]byte(`{"message":"x"}`))
			}))
			defer github.Close()
			err := checkSourceRepoCommit(github.URL, sourceRepoCommit{url: "https://github.com/me/starkive", sha: sha})
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("got %v, want an error with %q", err, c.wantErr)
			}
		})
	}
}

// Guards the dashboard name.
//   - If the archive name equals the username, it is `<name>-starkive`.
//     Otherwise it is `<username>-<archive name>-starkive`, so it doesn't clash with other users
//   - The username is made lower case
//   - Over 63 characters (the limit of one part of an address), the archive name part is cut.
//     A hyphen at the cut end doesn't create `--`
func TestBuildDashboardName(t *testing.T) {
	for _, c := range []struct {
		owner   string // Username of the token owner
		archive string // Archive name
		want    string // Dashboard name
	}{
		{"me", "me", "me-starkive"},
		{"Me", "me", "me-starkive"},
		{"me", "work", "me-work-starkive"},
	} {
		if got := buildDashboardName(c.owner, c.archive); got != c.want {
			t.Errorf("%s, %s: got %q, want %q", c.owner, c.archive, got, c.want)
		}
	}
	longOwner := strings.Repeat("a", 39)
	if got := buildDashboardName(longOwner, longOwner); got != longOwner+"-starkive" {
		t.Errorf("longest username: %q", got)
	}
	got := buildDashboardName(longOwner, "abcdefghijklm-opqrstuvwxyz")
	if len(got) > maxDashboardNameLength || !strings.HasPrefix(got, longOwner+"-abcdefghijklm") || !strings.HasSuffix(got, "-starkive") || strings.Contains(got, "--") {
		t.Errorf("long names: %q", got)
	}
}

// Guards the Deploy link.
// It fills in the code to copy (that commit of the repository), the copy repository name (which becomes the project name and address),
// and the environment variable name and description.
// It doesn't include `project-name`, which Vercel doesn't use, or the environment variable value
func TestBuildDeployURL(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	u, err := url.Parse(buildDeployURL(sourceRepoCommit{url: "https://github.com/me/starkive", sha: sha}, "me-starkive"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"repository-url": "https://github.com/me/starkive/tree/" + sha, "repository-name": "me-starkive", "env": core.DBRepoAccessKeyEnv,
	}
	for key, value := range want {
		if q.Get(key) != value {
			t.Errorf("%s: got %q, want %q", key, q.Get(key), value)
		}
	}
	if u.Host != "vercel.com" || u.Path != "/new/clone" || q.Get("envDescription") == "" || len(q) != len(want)+1 {
		t.Errorf("url: %s", u)
	}
}

// Guards the background check of the deployment. The dashboard side is the real server code (`api.HandleCheckDeployment`).
//   - While the dashboard is deployed with another value, it passes on nothing.
//     Once it is redeployed with this setup's value, it passes on that address
//   - After it is told to stop, it checks no more and passes on nothing
func TestWatchDeployment(t *testing.T) {
	cfg := &core.Config{DBRepoID: 5150, DBRepoAccessToken: "token_4242"}
	dashboard := httptest.NewServer(http.HandlerFunc(api.HandleCheckDeployment))
	defer dashboard.Close()

	t.Setenv(core.DBRepoAccessKeyEnv, core.FormatDBRepoAccessKey(5150, "token_9999")) // Deployed with another token
	confirmed, stop := watchDeployment(cfg, dashboard.URL)
	defer stop()
	select {
	case site := <-confirmed:
		t.Fatalf("passed on a dashboard deployed with another token: %s", site)
	case <-time.After(300 * time.Millisecond):
	}
	t.Setenv(core.DBRepoAccessKeyEnv, core.FormatDBRepoAccessKey(5150, "token_4242")) // Redeployed with this setup's value
	select {
	case site := <-confirmed:
		if site != dashboard.URL {
			t.Errorf("site: %q, want %q", site, dashboard.URL)
		}
	case <-time.After(deploymentPollInterval + 5*time.Second):
		t.Fatal("nothing passed on")
	}

	stopped, stopNow := watchDeployment(cfg, "http://127.0.0.1:1") // An address nobody listens on
	stopNow()
	stopNow() // Calling it more than once is fine
	select {
	case site := <-stopped:
		t.Errorf("passed on after stopping: %s", site)
	case <-time.After(300 * time.Millisecond):
	}
}
