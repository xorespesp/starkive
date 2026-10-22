// A cache that keeps values read from GitHub in the memory of a serverless instance for a short time, and its keys.
package core

import (
	"strconv"
	"sync"
	"time"
)

// Cache that keeps values for a set time.
// The zero value (`var c cache[T]`) can be used right away, and it is safe when many requests (goroutines) use it at once.
// `[V any]` is a type parameter, like a C++ template, so a separate cache is made for each value type, like `cache[GitHubUser]`.
//
// NOTE: It keeps the value that every API goes through (the DB repository) from being asked of GitHub every time.
// It also keeps requests that are open before sign-in (sign-in start and callback) from using the server token's rate limit (5,000 per hour, docs),
// even when they repeat.
// Serverless instances can start fresh at any time, and they don't share memory.
// So it only holds values that may be a little stale.
// The state and the sync key are checked without this cache.
// The allowed user (the token owner) is kept in it, but the token is part of the key, and the owner of a token never changes.
type cache[V any] struct {
	entries sync.Map // Key -> `cacheEntry[V]`. `sync.Map` is a map that many goroutines can use without a separate lock
}

// One value kept in the cache
type cacheEntry[V any] struct {
	value   V         // The kept value
	expires time.Time // After this time, it counts as missing
}

// Gets a kept value.
//
// Args:
//   - `key`: Cache key (`buildEnvCacheKey`, `buildDBRepoCacheKey`)
//
// Returns:
//   - The kept value and `true`. The zero value of `V` and `false` if it is missing or expired
func (c *cache[V]) get(key string) (V, bool) {
	if v, ok := c.entries.Load(key); ok {
		if entry := v.(cacheEntry[V]); time.Now().Before(entry.expires) { // `.(T)`: gets a value stored as `any` back as its original type
			return entry.value, true
		}
	}
	var zero V
	return zero, false
}

// Keeps `value` under `key` for `ttl`.
// Replaces any value with the same key
func (c *cache[V]) put(key string, value V, ttl time.Duration) {
	c.entries.Store(key, cacheEntry[V]{value: value, expires: time.Now().Add(ttl)})
}

// Makes the cache key for values that depend only on the environment variables (token owner, DB repository found by ID).
// The key is the GitHub address, the token and the repository ID.
//
// NOTE: In production, an instance's environment variables don't change.
// But tests and the local dev server run in one process while changing the fake GitHub address and the environment variables.
// All these values go into the key, so a value kept under other settings is never used.
func buildEnvCacheKey(cfg *EnvConfig) string {
	return cfg.GitHubAPI + "\n" + cfg.DBRepoAccessToken + "\n" + strconv.FormatInt(cfg.DBRepoID, 10)
}

// Makes the cache key for values read from a file in the DB repository (the sign-in GitHub App settings).
// The key is the GitHub address, the repository name, the branch and the token.
//
// NOTE: The token is in the key because the App's client secret is decrypted with a key made from the token.
// When the token changes, the result changes even for the same file.
func buildDBRepoCacheKey(repo *DBRepo) string {
	return repo.cfg.GitHubAPI + "\n" + repo.fullName + "\n" + repo.branch + "\n" + repo.cfg.DBRepoAccessToken
}
