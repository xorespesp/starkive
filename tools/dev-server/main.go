// Development server that runs the dashboard locally.
// Like Vercel, it serves `public/` as static files and connects `/api/*` to the entry functions in the `api` package.
// It also adds the same response headers (CSP, etc.) as `vercel.json`.
// Use it to check server changes in the page before deploying.
//
//	go run ./tools/dev-server            http://localhost:3000
//	go run ./tools/dev-server -addr :8080
//
// Run it from the repository root (it reads `public/`, `vercel.json` and `.env.local` from there).
// Environment variables come from the real environment or from `.env.local` (KEY=VALUE lines, not committed to git).
// A variable that is already set wins over the file.
// Without a fake GitHub (`STARKIVE_GH_API_URL`), it really commits to the DB repository the environment variable points to.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	api "github.com/xorespesp/starkive/api"
)

// Reads a file of KEY=VALUE lines and sets only the environment variables that aren't set yet.
// It skips empty lines and lines that start with `#`, and removes quotes around values.
//
// Args:
//   - `path`: Path of the file to read. If the file doesn't exist, it does nothing
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close() // Closes the file when the function ends (like a scope guard in C++)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set { // Doesn't overwrite a value that is already set (from the shell)
			os.Setenv(k, v)
		}
	}
	log.Printf("loaded %s", path)
}

// Reads from `vercel.json` (`path`) the response headers added to every path (`"source": "/(.*)"`).
//
// Returns:
//   - Header name -> value. An empty map if the file is missing or can't be read
func readVercelHeaders(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var cfg struct {
		Headers []struct {
			Source  string `json:"source"` // Path pattern the headers apply to
			Headers []struct {
				Key   string
				Value string
			} `json:"headers"`
		} `json:"headers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return out
	}
	for _, h := range cfg.Headers {
		if h.Source == "/(.*)" {
			for _, kv := range h.Headers {
				out[kv.Key] = kv.Value
			}
		}
	}
	return out
}

// Starts the development server. `-addr` changes the listen address (default `localhost:3000`).
//
// NOTE: When you add a new API file to `api/`, register its path here too.
// Vercel makes a function for each file on its own, but this server keeps its own list.
func main() {
	addr := flag.String("addr", "localhost:3000", "listen address")
	flag.Parse()

	loadEnvFile(".env.local")
	headers := readVercelHeaders("vercel.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login-session", api.HandleLoginSession)
	mux.HandleFunc("/api/archive-star-items", api.HandleArchiveStarItems)
	mux.HandleFunc("/api/archive-star-lists", api.HandleArchiveStarLists)
	mux.HandleFunc("/api/github-repo-meta", api.HandleGitHubRepoMeta)
	mux.HandleFunc("/api/github-stars", api.HandleGitHubStars)
	mux.HandleFunc("/api/sync-archive", api.HandleSyncArchive)
	mux.HandleFunc("/api/sync-status", api.HandleSyncStatus)
	mux.HandleFunc("/api/archive-repo-meta", api.HandleArchiveRepoMeta)
	mux.HandleFunc("/api/check-deployment", api.HandleCheckDeployment)
	mux.Handle("/", http.FileServer(http.Dir("public")))

	// Adds the `vercel.json` headers to every response first, then passes it to the routes above
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		mux.ServeHTTP(w, r)
	})

	log.Printf("dashboard on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}
