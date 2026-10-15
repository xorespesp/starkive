// Server config (`Config`), the format of its source (the environment variable `STARKIVE_DB_REPO_ACCESS_KEY`),
// and the proof that a deployment is set up with that value (`SignDeploymentNonce`).
// The `core` package holds code shared by the API functions (`api/*.go`) and the setup tool (`tools/setup`).
// Finding the DB repository by its ID and judging its state is in `db_repo.go`.
// Checking the token owner is in `github_login.go`.
package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Name of the only environment variable that a person puts in Vercel. The setup tool makes and prints its value (`FormatDBRepoAccessKey`)
const DBRepoAccessKeyEnv = "STARKIVE_DB_REPO_ACCESS_KEY"

// Format tag at the start of the environment variable value.
// If the value format changes, change this tag too, so a value in the old format is seen as malformed.
const dbRepoAccessKeyPrefix = "starkive-v1"

// Server config.
// It is made from the environment variable on each request (`LoadEnvConfig`).
// After the DB repository is checked, its name and branch are filled in (`useDBRepo`).
// The user allowed to sign in and the secret keys come from the token.
// The sign-in GitHub App and the sync settings are in `config.json` in the DB repository.
type Config struct {
	DBRepoID          int64  // DB repository ID. It stays the same when the repository is renamed, so the repository is referred to by this ID
	DBRepoAccessToken string // Fine-grained token with read/write access to Contents, Workflows, Secrets and Actions of one DB repository, and read access to the account's Starring
	DBRepoFullName    string // Current name of the DB repository, `owner/repo`. Empty right after `LoadEnvConfig`. `ResolveDBRepo` fills it in
	DBRepoBranch      string // Default branch of the DB repository. `ResolveDBRepo` fills it in
	GitHubAPI         string // GitHub API address (`STARKIVE_GH_API_URL`, default https://api.github.com). Changed only when tests use a fake GitHub
	GitHubWeb         string // GitHub web address (`STARKIVE_GH_WEB_URL`, default https://github.com). Used for the sign-in page, the sign-in code exchange and App creation
}

// Makes the proof for a deployment check (`/api/check-deployment`) by signing the `nonce`.
// The proof is HMAC-SHA256(key = `deriveKey(token, "starkive-check-deployment")`, message = `<repository ID>:<nonce>`), written in base64url.
// The server answers with it, and the setup tool computes the same value to compare.
//
// Args:
//   - `nonce`: Random value that the setup tool makes for each check. It stops an old answer from being reused
//
// NOTE: If the values match, the deployment's environment variable is set to this repository ID and this token.
// The answer alone reveals neither the token nor the repository, so it is open without sign-in.
// The server and the setup tool must agree on the format, so it lives in this file.
// If it lived in two places, one side could change alone and the check would always fail.
func SignDeploymentNonce(cfg *Config, nonce string) string {
	mac := hmac.New(sha256.New, deriveKey(cfg, "starkive-check-deployment"))
	mac.Write([]byte(strconv.FormatInt(cfg.DBRepoID, 10) + ":" + nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Characters accepted in a deployment check `nonce` (characters of base64url and hex, 16 to 128 of them). Other characters are refused
var deploymentNoncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// Reports whether a string has a valid shape for a deployment check `nonce`
func IsValidDeploymentNonce(nonce string) bool {
	return deploymentNoncePattern.MatchString(nonce)
}

// Makes the value of the environment variable `STARKIVE_DB_REPO_ACCESS_KEY`, `starkive-v1:<repository ID>:<token>`, from the DB repository ID and the token.
//
// NOTE: The separator `:` never appears in the repository ID (digits) or the token (letters, digits, `_`).
// So the value splits into exactly three parts.
// Its format must match the reader, `parseDBRepoAccessKey`.
// So it lives in this file, even though only the setup tool uses it.
func FormatDBRepoAccessKey(repoID int64, token string) string {
	return dbRepoAccessKeyPrefix + ":" + strconv.FormatInt(repoID, 10) + ":" + token
}

// Characters in a GitHub token (letters, digits, `_`).
// The token goes into a request header as it is, so other characters (such as line breaks) are refused.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Reports whether a string has the shape of a GitHub token (only letters, digits and `_`)
func IsValidAccessToken(token string) bool {
	return tokenPattern.MatchString(token)
}

// Splits the value of the environment variable `STARKIVE_DB_REPO_ACCESS_KEY` into the repository ID and the token.
//
// Args:
//   - `value`: Environment variable value. Empty means the variable isn't set. Leading and trailing spaces and line breaks are ignored
//
// Returns:
//   - The repository ID and the token.
//     A 500 `HTTPError` if the value is missing or malformed (including when only a token was put in)
//
// NOTE: It reports one error and doesn't tell the causes apart (missing, token only, wrong shape).
// In every case the fix is the same: put in the value that the setup tool printed.
func parseDBRepoAccessKey(value string) (repoID int64, token string, err error) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 3)
	malformed := Errorf(http.StatusInternalServerError, "%s is missing or malformed. Use the value the starkive setup tool printed", DBRepoAccessKeyEnv)
	if len(parts) != 3 || parts[0] != dbRepoAccessKeyPrefix || !IsValidAccessToken(parts[2]) {
		return 0, "", malformed
	}
	repoID, parseErr := strconv.ParseInt(parts[1], 10, 64)
	if parseErr != nil || repoID <= 0 {
		return 0, "", malformed
	}
	return repoID, parts[2], nil
}

// Makes the server config from the environment variables only.
// It doesn't ask GitHub, so `DBRepoFullName` and `DBRepoBranch` are empty.
//
// Returns:
//   - The server config. A 500 `HTTPError` if the environment variable is missing or malformed
//
// NOTE: The sign-in cookie and the sync key are checked with this config alone.
// So a request without sign-in, or a scheduled call without the key, causes no GitHub request.
// Even if someone calls it again and again, it doesn't use the server token's rate limit.
// After the check, `useDBRepo` fills in the repository.
func LoadEnvConfig() (*Config, error) {
	repoID, token, err := parseDBRepoAccessKey(os.Getenv(DBRepoAccessKeyEnv))
	if err != nil {
		return nil, err
	}
	return &Config{
		DBRepoID:          repoID,
		DBRepoAccessToken: token,
		GitHubAPI:         strings.TrimRight(readEnv("STARKIVE_GH_API_URL", "https://api.github.com"), "/"),
		GitHubWeb:         strings.TrimRight(readEnv("STARKIVE_GH_WEB_URL", "https://github.com"), "/"),
	}, nil
}

// Reads the environment variable `key`, or returns `fallback` if it is missing or empty
func readEnv(key string, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
