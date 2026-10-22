// Tests for the Vercel deployment step: the commit to deploy to Vercel (remote address parsing, the GitHub check),
// the values in the Vercel Deploy link, the dashboard name, and checking in the background for the deployment to finish.
package main

import (
	"context"
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

// Guards the GitHub check of the commit to deploy to Vercel (fake GitHub).
// It passes if the public Vercel deploy repository has the commit.
// If the repository can't be seen (private or missing), the error says a public repository is needed.
// If the commit is missing (404, 422), the error says to push.
// For a release executable (a stamped repository address), both errors say to download the latest setup tool instead.
// Other failures give GitHub's error
func TestCheckVercelDeployCommit(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	cases := []struct {
		name    string
		release bool   // Whether it runs as a release executable (`stampedVercelDeployRepoURL` is set)
		repo    int    // Status code of the repository lookup
		commit  int    // Status code of the commit lookup
		wantErr string // Empty means it passes
	}{
		{"public repository with the commit", false, http.StatusOK, http.StatusOK, ""},
		{"private or missing repository", false, http.StatusNotFound, http.StatusOK, "run the setup tool in a clone"},
		{"unpushed commit", false, http.StatusOK, http.StatusNotFound, "git push"},
		{"commit GitHub doesn't know", false, http.StatusOK, http.StatusUnprocessableEntity, "git push"},
		{"GitHub failing", false, http.StatusServiceUnavailable, http.StatusOK, "503"},
		{"release: private or missing repository", true, http.StatusNotFound, http.StatusOK, "download the latest setup tool"},
		{"release: commit gone", true, http.StatusOK, http.StatusUnprocessableEntity, "Download the latest setup tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.release {
				stampedVercelDeployRepoURL = "https://github.com/me/starkive"
				t.Cleanup(func() { stampedVercelDeployRepoURL = "" }) // `t.Cleanup`: runs when this test case ends, so other cases see the default
			}
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
			err := checkVercelDeployCommit(context.Background(), github.URL, commitRef{url: "https://github.com/me/starkive", sha: sha})
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

// Guards the Vercel Deploy link.
// It fills in the code to copy (that commit of the repository), the copy repository name (which becomes the project name and address),
// and the environment variable name and description.
// It doesn't include `project-name`, which Vercel doesn't use, or the environment variable value
func TestBuildVercelDeployURL(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	u, err := url.Parse(buildVercelDeployURL(commitRef{url: "https://github.com/me/starkive", sha: sha}, "me-starkive"))
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
func TestWatchVercelDeployment(t *testing.T) {
	cfg := &core.Config{DBRepoID: 5150, DBRepoAccessToken: "token_4242"}
	dashboard := httptest.NewServer(http.HandlerFunc(api.HandleCheckDeployment))
	defer dashboard.Close()

	t.Setenv(core.DBRepoAccessKeyEnv, core.FormatDBRepoAccessKey(5150, "token_9999")) // Deployed with another token
	confirmed, stop := watchVercelDeployment(cfg, dashboard.URL)
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
	case <-time.After(vercelDeploymentPollInterval + 5*time.Second):
		t.Fatal("nothing passed on")
	}

	stopped, stopNow := watchVercelDeployment(cfg, "http://127.0.0.1:1") // An address nobody listens on
	stopNow()
	stopNow() // Calling it more than once is fine
	select {
	case site := <-stopped:
		t.Errorf("passed on after stopping: %s", site)
	case <-time.After(300 * time.Millisecond):
	}
}
