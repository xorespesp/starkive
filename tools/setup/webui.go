// The `ui` that runs on the browser setup page (`webUI`, the default mode).
// The server keeps what the step logic shows, and the page (`web/`) fetches it at short intervals and draws it.
// The page shows the steps (`setupSteps`) as a vertical list, and expands only the step in progress.
// While no prompt is waiting for an answer, it shows that the tool is working.
// When the setup ends, it shows a result card (`done`).
// Prompts go to an input box on the page, and the step logic waits until the answer arrives.
// While waiting for a value that is checked in the background, the page shows a waiting sentence and a link to enter it by hand, instead of an input box.
// When the link is clicked (`POST /switch-to-typing`), the step logic is told.
//
// This page handles the token and the environment variable value, so only the person who started this run can use it:
//   - The server listens only on this computer, and rejects requests whose `Host` header isn't this computer's address (`localServer`)
//   - Each run makes a random key and puts it only in the first address to open (`?key=`).
//     Only the browser that opened that address gets a cookie with the key, and every path of the setup page checks the cookie
//   - Requests that send an answer or switch to typing must have this server as `Origin`.
//     The cookie is `SameSite=Strict`, so it isn't sent with requests from other sites
//   - The page uses CSP to block scripts and connections outside this server
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Files of the setup page.
// `//go:embed` tells the build to put these files inside the executable (there are no separate files to ship)
//
//go:embed web/index.html web/setup.js web/setup.css
var webFiles embed.FS

// Name of the cookie that holds this run's key
const setupCookieName = "starkive_setup"

// CSP added to the setup page responses.
// It uses only scripts and styles from this server, and connects only to this server
const setupPageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// One line to show on the setup page
type webLine struct {
	Kind  string `json:"kind"`  // `step` (step start), `say` (guidance), `note` (guidance not to miss), `value` (a value to copy), `link` (a link to open if wanted), `open` (a link that must be opened to go on), `answer` (what the person answered)
	Text  string `json:"text"`  // The guidance sentence, the value, the link, or the prompt that was answered
	Label string `json:"label"` // The label of `link` and `open`, the name of `value`, or the answer of `answer`
	Step  int    `json:"step"`  // The step number of `step` (from 1)
}

// One prompt shown on the page.
// Usually an input box.
// If `Waiting` is set, it is a waiting prompt instead of an input box (`waitForValue`)
type webPrompt struct {
	ID          int           `json:"id"`           // Prompt number. Tells whether an answer or a switch request is for the current prompt
	Text        string        `json:"text"`         // The prompt. On a waiting prompt, it goes in front of the received value when that value is added as a line
	Hint        string        `json:"hint"`         // A faint example shown inside the input box. Empty string if none
	Secret      bool          `json:"secret"`       // If `true`, the input box hides the characters, and the answer isn't added as a line
	Waiting     string        `json:"waiting"`      // The sentence shown while waiting. An empty string means a normal input box
	ManualEntry string        `json:"manual_entry"` // On a waiting prompt, the label of the link to enter it by hand
	answer      chan string   // Channel that passes the answer. A lowercase field, so it isn't put in JSON
	typing      chan struct{} // Channel that passes the request to enter it by hand
}

// What the page's result card shows when the setup ends with success (`done`)
type webResult struct {
	Text   string `json:"text"`   // The last message
	Label  string `json:"label"`  // Label of the next link to open
	Target string `json:"target"` // The next link to open
}

// The progress that the page fetches (`GET /state`)
type webState struct {
	Steps    []string   `json:"steps"`    // Step names (`setupSteps`)
	Current  int        `json:"current"`  // The step number in progress (from 1). 0 if not started yet
	Lines    []webLine  `json:"lines"`    // Lines from the requested number (`since`)
	Total    int        `json:"total"`    // Number of lines so far. The page uses it as `since` in the next request
	Prompt   *webPrompt `json:"prompt"`   // The prompt waiting for an answer. `null` if none
	Finished bool       `json:"finished"` // Whether the setup ended
	Failed   bool       `json:"failed"`   // If it ended, whether it failed. If it failed, the step in progress is the failed step
	Result   *webResult `json:"result"`   // The result card content on success. `null` if not set yet
	Version  string     `json:"version"`  // Version of this setup tool (`readVersionLabel`)
}

// The `ui` that runs on the setup page
type webUI struct {
	server     *localServer  // The server that serves the page
	key        string        // This run's random key. It is only in the first address and in the cookie
	startURL   string        // The first link to open (setup page address with `?key=`). Only a browser that opened this link can use the page
	mu         sync.Mutex    // Lock that guards the fields below. The step logic and page requests read and write them from different goroutines
	current    int           // The step number in progress
	lines      []webLine     // All lines so far. All are kept, so a reloaded page can draw everything again from the start
	prompt     *webPrompt    // The prompt waiting for an answer. `nil` if none
	nextID     int           // The next prompt number
	finished   bool          // Whether the setup ended
	failed     bool          // Whether it ended with failure
	result     *webResult    // The result card content (`done`). `nil` if none
	seenFinish chan struct{} // Closed when the page fetches the finished state (`waitUntilSeen`)
	seenOnce   sync.Once     // Makes sure `seenFinish` is closed only once
}

// Attaches the setup page to the server `server`, and prints the first link to open (`startURL`) in the terminal
func newWebUI(server *localServer) (*webUI, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	u := &webUI{server: server, key: hex.EncodeToString(random), seenFinish: make(chan struct{})}
	server.handle("/", u.handlePage)
	server.handle("/setup.js", u.handleFile("web/setup.js", "text/javascript; charset=utf-8"))
	server.handle("/setup.css", u.handleFile("web/setup.css", "text/css; charset=utf-8"))
	server.handle("/state", u.handleState)
	server.handle("/answer", u.handleAnswer)
	server.handle("/switch-to-typing", u.handleSwitchToTyping)

	u.startURL = server.base + "/?key=" + u.key
	fmt.Println("Starkive Setup Wizard continues in your browser. Keep this window open until it finishes.")
	fmt.Println("   " + u.startURL)
	return u, nil
}

// Adds one line `line`
func (u *webUI) add(line webLine) {
	u.mu.Lock()
	defer u.mu.Unlock() // `defer`: unlocks when this function ends
	u.lines = append(u.lines, line)
}

// Marks step `n` as in progress and adds its title line
func (u *webUI) step(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.current = n
	u.lines = append(u.lines, webLine{Kind: "step", Text: setupSteps[n-1], Step: n})
}

// Adds one line of guidance `line`
func (u *webUI) say(line string) { u.add(webLine{Kind: "say", Text: line}) }

// Adds one line `line` that must not be missed.
// The page shows it in a box that stands out
func (u *webUI) note(line string) { u.add(webLine{Kind: "note", Text: line}) }

// Sets the result card content: the last message `text`, and the next link `target` to open with its label `label`.
// The card shows when the setup is marked as finished (`finish`)
func (u *webUI) done(text string, label string, target string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.result = &webResult{Text: text, Label: label, Target: target}
}

// Adds the name `name` and the value `value` to copy
func (u *webUI) value(name string, value string) {
	u.add(webLine{Kind: "value", Text: value, Label: name})
}

// Adds the label `label` and a link `target` to open if wanted
func (u *webUI) link(label string, target string) {
	u.add(webLine{Kind: "link", Text: target, Label: label})
}

// Adds the label `label` and a link `target` that must be opened to go on.
// The page shows it as a button (browsers block new windows the person didn't click, so it doesn't open by itself)
func (u *webUI) open(label string, target string) {
	u.add(webLine{Kind: "open", Text: target, Label: label})
}

// Shows the prompt `prompt` as an input box on the page and waits for the answer.
// `hint` shows faintly inside the input box
func (u *webUI) ask(prompt string, hint string) (string, error) {
	value, _ := u.show(&webPrompt{Text: prompt, Hint: hint}, nil)
	return value, nil
}

// Shows the prompt `prompt` with an input box that hides the characters, and waits for the answer
func (u *webUI) askSecret(prompt string) (string, error) {
	value, _ := u.show(&webPrompt{Text: prompt, Secret: true}, nil)
	return value, nil
}

// Shows the waiting sentence `waiting` and a link labeled `manualEntryLabel` to enter it by hand, instead of an input box.
// Waits until a value comes on `received` or the link is clicked.
// A received value is added as a line, together with the prompt `prompt`
func (u *webUI) waitForValue(waiting string, prompt string, manualEntryLabel string, received <-chan string) (string, waitResult, error) {
	value, result := u.show(&webPrompt{Text: prompt, Waiting: waiting, ManualEntry: manualEntryLabel}, received)
	return value, result, nil
}

// Shows the prompt `p` on the page and waits until it ends.
// A normal prompt ends when the answer comes.
// A waiting prompt (`Waiting`) ends when a value comes on `received` or a request to enter it by hand comes.
//
// Args:
//   - `p`: The prompt to show. The number and the channels are filled in here
//   - `received`: Channel that receives the value checked in the background. If `nil`, nothing comes from it
//
// Returns:
//   - The answer or the received value (with leading and trailing spaces removed). An empty string for a request to enter it by hand
//   - Why it ended. For an answer to a normal prompt, it is `typedAnswer`
//
// NOTE: `handleAnswer` and `handleSwitchToTyping` pass answers and switch requests through the channels, from other goroutines.
// The receiving side blocks until then.
// Closing the page doesn't stop the wait, and opening it again shows the same prompt.
// To give up, stop the tool with Ctrl+C in the terminal.
func (u *webUI) show(p *webPrompt, received <-chan string) (string, waitResult) {
	u.mu.Lock()
	u.nextID++
	p.ID, p.answer, p.typing = u.nextID, make(chan string, 1), make(chan struct{}, 1)
	u.prompt = p
	u.mu.Unlock()
	select { // Takes whichever comes first. If `received` is `nil`, nothing ever comes from it
	case value := <-p.answer:
		return strings.TrimSpace(value), typedAnswer
	case <-p.typing:
		return "", switchedToTyping
	case value := <-received:
		u.mu.Lock()
		if u.prompt == p { // Even if a switch request took the prompt away meanwhile, the confirmed value is used. But the prompt was taken away, so no line is added
			u.prompt = nil
			u.lines = append(u.lines, webLine{Kind: "answer", Text: p.Text, Label: value})
		}
		u.mu.Unlock()
		return strings.TrimSpace(value), receivedValue
	}
}

// Marks the setup as finished.
// `failed` is whether it ended with failure
func (u *webUI) finish(failed bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.finished, u.failed = true, failed
}

// Waits up to `timeout` until the page fetches the finished state.
// Used before stopping the server, so the last lines show on the page
func (u *webUI) waitUntilSeen(timeout time.Duration) {
	select { // Waits for whichever comes first
	case <-u.seenFinish:
	case <-time.After(timeout):
	}
}

// Tells whether the request has the cookie with this run's key
func (u *webUI) hasSession(r *http.Request) bool {
	cookie, err := r.Cookie(setupCookieName)
	return err == nil && hmac.Equal([]byte(cookie.Value), []byte(u.key)) // `hmac.Equal`: compares in constant time, so the key can't be guessed from the time it takes
}

// Sets the response headers of the setup page.
// The page and the progress can contain the environment variable value with the token.
// `no-store` and `no-referrer` keep it out of caches and other sites, and CSP and `nosniff` keep foreign scripts away from it
func setSetupPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", setupPageCSP)
	w.Header().Set("Referrer-Policy", "no-referrer") // Doesn't pass this address on when the page opens GitHub or Vercel links
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// Handler that serves the setup page (`/`).
// For the first address (`?key=`), it checks the key, sets the cookie, and redirects to `/` without the key
func (u *webUI) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	setSetupPageHeaders(w)
	if key := r.URL.Query().Get("key"); key != "" {
		if !hmac.Equal([]byte(key), []byte(u.key)) {
			http.Error(w, "This link isn't for this setup run. Open the link printed in the terminal.", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: setupCookieName, Value: u.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !u.hasSession(r) {
		http.Error(w, "Open the link printed in the terminal.", http.StatusForbidden)
		return
	}
	content, _ := webFiles.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

// Builds a handler that serves the setup page's static file `path` as `contentType`.
// It is 403 without the key cookie
func (u *webUI) handleFile(path string, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSetupPageHeaders(w)
		if !u.hasSession(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		content, _ := webFiles.ReadFile(path)
		w.Header().Set("Content-Type", contentType)
		w.Write(content)
	}
}

// Handler that answers the progress (`GET /state?since=<number>`) as JSON.
// It returns lines from the `since`-th line.
// The first time it sends the finished state, it releases the wait in `waitUntilSeen`
func (u *webUI) handleState(w http.ResponseWriter, r *http.Request) {
	setSetupPageHeaders(w)
	if r.Method != http.MethodGet || !u.hasSession(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	u.mu.Lock()
	since = min(max(since, 0), len(u.lines)) // Even if the page sends a wrong number, it is kept in range
	state := webState{
		Steps: setupSteps, Current: u.current, Lines: append([]webLine{}, u.lines[since:]...), Total: len(u.lines),
		Prompt: u.prompt, Finished: u.finished, Failed: u.failed, Result: u.result, Version: readVersionLabel(),
	}
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(state)
	if state.Finished {
		u.seenOnce.Do(func() { close(u.seenFinish) }) // `sync.Once`: closes it only once, even if several requests come at the same time
	}
}

// Handler that takes the answer to a prompt (`POST /answer`, body `{ id, value }`) and passes it to the waiting step logic.
// Requests from other sites (`Origin` isn't this server) and requests without the key cookie are 403.
// A number that isn't the current prompt is 409
func (u *webUI) handleAnswer(w http.ResponseWriter, r *http.Request) {
	setSetupPageHeaders(w)
	if r.Method != http.MethodPost || !u.server.isLocalOrigin(r.Header.Get("Origin")) || !u.hasSession(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		ID    int    `json:"id"`    // Number of the prompt being answered
		Value string `json:"value"` // The answer
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil { // Reads the body only up to 64KB
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.prompt
	if p == nil || p.ID != body.ID || p.Waiting != "" { // A waiting prompt has no input box, so it doesn't take answers
		http.Error(w, "That question was already answered", http.StatusConflict)
		return
	}
	if !p.Secret {
		u.lines = append(u.lines, webLine{Kind: "answer", Text: p.Text, Label: strings.TrimSpace(body.Value)})
	}
	u.prompt = nil
	p.answer <- body.Value // The channel has room for one (buffer of 1), so this doesn't block even if the waiting side hasn't taken it yet
	w.WriteHeader(http.StatusNoContent)
}

// Handler that takes a request to enter it by hand on a waiting prompt (`POST /switch-to-typing`, body `{ id }`) and passes it to the waiting step logic.
// It blocks the same requests as `handleAnswer`.
// A number that isn't the current waiting prompt is 409 (for example, the check finished meanwhile and the setup moved on)
func (u *webUI) handleSwitchToTyping(w http.ResponseWriter, r *http.Request) {
	setSetupPageHeaders(w)
	if r.Method != http.MethodPost || !u.server.isLocalOrigin(r.Header.Get("Origin")) || !u.hasSession(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		ID int `json:"id"` // Number of the waiting prompt
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.prompt
	if p == nil || p.ID != body.ID || p.Waiting == "" {
		http.Error(w, "Not waiting anymore", http.StatusConflict)
		return
	}
	u.prompt = nil
	p.typing <- struct{}{} // Buffer of 1, so this doesn't block even if the waiting side hasn't taken it yet
	w.WriteHeader(http.StatusNoContent)
}
