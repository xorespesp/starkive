// Cache of the DB repository files that one request has read: what it holds, and how it rides on the `ctx` of the request.
// How a read uses it (a conditional request with `If-None-Match`) is in `readDBRepoFileAtRef` (`db_repo_io.go`).
package core

import (
	"context"
	"sync"
)

// Files of the DB repository that one request has read, by path, each with the `ETag` GitHub gave for it.
// A later read of the same path sends that `ETag`, and GitHub answers 304 if the content is the same (`readDBRepoFileAtRef`).
// So a kept content is used only when GitHub says it is still the content, and it can't be stale.
// It lives only as long as the request (`ServeAPIRequest`), so nothing has to be dropped.
// A `nil` cache keeps nothing and finds nothing, so code without a request (a test, a tool) needs no check.
type dbRepoFileCache struct {
	mu          sync.Mutex                // Lock for `filesByPath`. Goroutines of one request read files at the same time
	filesByPath map[string]keptDBRepoFile // Path -> the content last read and its `ETag`
}

// One file kept in `dbRepoFileCache`
type keptDBRepoFile struct {
	etag    string // `ETag` GitHub gave with the content (for a file, its git blob sha in quotes, measured)
	content []byte // The content
}

// Returns the file kept for `path`, and `true` if there is one.
func (cache *dbRepoFileCache) get(path string) (keptDBRepoFile, bool) {
	if cache == nil {
		return keptDBRepoFile{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	kept, ok := cache.filesByPath[path]
	return kept, ok
}

// Keeps `content` for `path` with its `etag`.
func (cache *dbRepoFileCache) put(path string, etag string, content []byte) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.filesByPath == nil {
		cache.filesByPath = map[string]keptDBRepoFile{}
	}
	cache.filesByPath[path] = keptDBRepoFile{etag: etag, content: content}
}

// Keeps in the cache of the request in `ctx` the files that `changes` just wrote, so a later read in the request downloads nothing.
// Their `ETag` is made from the content, as GitHub makes it for a file: its git blob sha in quotes (measured).
// If GitHub made it differently, the later read would only download the file again.
func keepCommittedDBRepoFiles(ctx context.Context, changes FileChangesCommit) {
	cache := readDBRepoFileCache(ctx)
	for path, content := range changes.FilesToWrite {
		cache.put(path, `"`+computeGitBlobSHA(content)+`"`, content)
	}
}

// Key of the cache in a `ctx`.
// It is a type of its own, so no key of another package can be the same (Go's `context` convention).
type dbRepoFileCacheKey struct{}

// Returns `ctx` with a new, empty cache.
func withDBRepoFileCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, dbRepoFileCacheKey{}, &dbRepoFileCache{})
}

// Returns the cache of the request in `ctx`, or `nil` if `ctx` has none.
func readDBRepoFileCache(ctx context.Context) *dbRepoFileCache {
	cache, _ := ctx.Value(dbRepoFileCacheKey{}).(*dbRepoFileCache) // Type assertion (like `dynamic_cast`). Without the value, `cache` is `nil`
	return cache
}
