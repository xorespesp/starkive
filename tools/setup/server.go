// HTTP server that the setup tool opens only on this computer (`localServer`).
// Only one runs per run of the tool.
// The code that uses it adds its own paths (`handle`).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HTTP server that can be reached only from this computer
type localServer struct {
	port   int            // Port the operating system chose
	base   string         // Start of the address opened in the browser, `http://localhost:<port>`
	mux    *http.ServeMux // Handlers per path. Added with `handle`
	server *http.Server   // The listening server. `close` stops it
}

// Starts a `localServer` that listens on a free port of `127.0.0.1`.
// It also listens on `::1` with the same port, for browsers that look up `localhost` as IPv6.
// (If that fails, it uses only IPv4.)
//
// NOTE: Every request goes through `isLocalHost`.
// In a DNS rebinding attack, another site points its own domain name to `127.0.0.1` to read this server.
// The `Host` header still has that domain name, so the attack is blocked here.
func startLocalServer() (*localServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0") // `:0`: the operating system picks a free port
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port // `.(*net.TCPAddr)`: gets the concrete type out of an interface value (like `dynamic_cast` in C++)
	listeners := []net.Listener{listener}
	if v6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
		listeners = append(listeners, v6)
	}
	s := &localServer{port: port, base: fmt.Sprintf("http://localhost:%d", port), mux: http.NewServeMux()}
	s.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.isLocalHost(r.Host) {
				http.Error(w, "Open this page at "+s.base, http.StatusMisdirectedRequest)
				return
			}
			s.mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, l := range listeners {
		go s.server.Serve(l) // `go`: runs it in a goroutine (a lightweight thread), so the setup goes on while it listens
	}
	return s, nil
}

// Reports whether the request's `Host` header `host` calls this server by this computer's address
// (`localhost`, `127.0.0.1` or `::1`, with this port)
func (s *localServer) isLocalHost(host string) bool {
	for _, name := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if host == fmt.Sprintf("%s:%d", name, s.port) {
			return true
		}
	}
	return false
}

// Reports whether the request's `Origin` header `origin` is a page of this server.
// Used to check that a write request didn't come from another site
func (s *localServer) isLocalOrigin(origin string) bool {
	for _, name := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if origin == fmt.Sprintf("http://%s:%d", name, s.port) {
			return true
		}
	}
	return false
}

// Adds the handler `handler` for the path `pattern`.
// Go's `http.ServeMux` stops (panics) if the same path is added twice, so call this only once per path
func (s *localServer) handle(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

// Stops the server.
// Waits up to 3 seconds for requests in progress
func (s *localServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel() // `defer`: runs when this function ends (like a C++ destructor, so the cleanup isn't forgotten)
	s.server.Shutdown(ctx)
}
