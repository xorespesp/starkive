// Values stamped into the setup tool executable at build time, and printing them as the version (`printVersion`).
// The release workflow (`.github/workflows/release.yml`) stamps the repository address and the release tag with `-ldflags -X`.
// Go stamps the commit it builds from by itself (`readStampedVCS`).
// The commit to deploy to Vercel is picked from these values in `resolveVercelDeployCommit`.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// Address of the Vercel deploy repository, stamped into a release executable at build time (`https://github.com/<owner>/<repo>`).
// It is the repository that the Vercel Deploy link copies into the user's account (`buildVercelDeployURL`).
// Empty in a build without it (`go run`, a plain `go build`).
//
// NOTE: The release workflow stamps it with `go build -ldflags "-X main.stampedVercelDeployRepoURL=..."`,
// so the variable name must match `.github/workflows/release.yml`.
// `-X` sets the starting value of a package-level `string` variable at link time.
// If the name is wrong, Go doesn't report an error and the value stays empty.
var stampedVercelDeployRepoURL string

// Release tag (like `v0.1.0`), stamped into a release executable at build time.
// Empty in a build without it.
// The release workflow stamps it the same way as `stampedVercelDeployRepoURL`
var stampedReleaseTag string

// Reads the git information that Go stamped into this executable when it was built.
//
// Returns:
//   - `revision`: The commit it was built from. Empty if Go stamped no git commit
//   - `modified`: Whether the build had uncommitted changes
//   - `commitTime`: The time of that commit (not the build time). Empty if there is none
//
// NOTE: When `go build` runs in a git folder, Go stamps the commit it builds from (`vcs.revision`),
// whether there were uncommitted changes (`vcs.modified`), and the commit time (`vcs.time`).
// `go run` doesn't stamp them (measured).
// `go version -m <file>` shows them, and `go help build` describes this under `-buildvcs`.
func readStampedVCS() (revision string, modified bool, commitTime string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, ""
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["vcs"] != "git" || !isValidCommitSHA(settings["vcs.revision"]) {
		return "", false, ""
	}
	return settings["vcs.revision"], settings["vcs.modified"] == "true", settings["vcs.time"]
}

// Prints the version of this setup tool: the release tag, the code it deploys to Vercel (`resolveVercelDeployCommit`), the commit time, and the Go build.
// If the code to deploy to Vercel is unknown, it prints the reason in its place and returns the error.
//
// NOTE: The release workflow runs the setup tool with `-version` and looks for the address and the tag in this output (`.github/workflows/release.yml`).
// So keep printing both.
// The build time isn't stamped, so two builds of the same commit with the same Go version and flags give the same file and the same checksum.
func printVersion() error {
	tag := stampedReleaseTag
	if tag == "" {
		tag = "development build"
	}
	fmt.Println("starkive setup tool " + tag)
	vercelDeployCommit, hasUncommittedChanges, err := resolveVercelDeployCommit()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Vercel deploy source: unknown ("+err.Error()+")")
		return err
	}
	fmt.Println("Vercel deploy source: " + vercelDeployCommit.buildTreeURL())
	if _, _, commitTime := readStampedVCS(); commitTime != "" {
		fmt.Println("Commit time: " + commitTime)
	}
	if hasUncommittedChanges {
		fmt.Println("Uncommitted changes: yes (not deployed to Vercel)")
	}
	fmt.Println("Built with: " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH)
	return nil
}
