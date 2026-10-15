// Tests for the setup page (`webUI`) and this tool's localhost server (`localServer`).
// They check that only the person who started this run can use the page (key and cookie, `Host`, `Origin`).
// They check that step state, prompts and answers pass between the page and the step logic.
// They also check the hint for a browser on another computer.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Starts the setup page. The server stops when the test ends
func startTestWebUI(t *testing.T) (*localServer, *webUI) {
	server, err := startLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.close)
	u, err := newWebUI(server)
	if err != nil {
		t.Fatal(err)
	}
	return server, u
}

// HTTP client that doesn't follow redirects.
// Used to see the raw response where the first address sets the cookie and moves to `/`
var noRedirectClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Sends a request to the setup page and returns the response. Stops the test on failure.
//
// Args:
//   - `method`: HTTP method
//   - `target`: Server address
//   - `body`: Body. None if empty
//   - `headers`: Headers to add (`Cookie`, `Origin`, etc.)
func send(t *testing.T, method string, target string, body string, headers map[string]string) *http.Response {
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// Gets the progress (`GET /state`) from the first line, with the key cookie `session`
func readState(t *testing.T, server *localServer, session map[string]string) webState {
	var state webState
	json.NewDecoder(send(t, "GET", server.base+"/state", "", session).Body).Decode(&state)
	return state
}

// Checks that only a browser opened with this run's key can use the setup page.
//   - Opening without a key or with a wrong key gives 403.
//     Opening with the right key sets a cookie with the key and moves to `/` without the key
//   - With the cookie, it serves the page, the script and the progress. With a cookie of another value, all give 403
//   - The page has CSP and `no-store`
//   - If `Host` isn't this computer's address (DNS rebinding), it rejects the request whatever the path
func TestSetupPageOnlyForThisRun(t *testing.T) {
	server, u := startTestWebUI(t)
	if res := send(t, "GET", server.base+"/", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("no key: %d", res.StatusCode)
	}
	if res := send(t, "GET", server.base+"/?key=wrong", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong key: %d", res.StatusCode)
	}
	res := send(t, "GET", server.base+"/?key="+u.key, "", nil)
	cookie := res.Header.Get("Set-Cookie")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || !strings.Contains(cookie, u.key) ||
		!strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Fatalf("right key: %d %v", res.StatusCode, res.Header)
	}
	session := map[string]string{"Cookie": setupCookieName + "=" + u.key}
	for _, path := range []string{"/", "/setup.js", "/setup.css", "/state"} {
		if res := send(t, "GET", server.base+path, "", session); res.StatusCode != http.StatusOK || res.Header.Get("Content-Security-Policy") != setupPageCSP ||
			res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s with the cookie: %d %v", path, res.StatusCode, res.Header)
		}
		if res := send(t, "GET", server.base+path, "", map[string]string{"Cookie": setupCookieName + "=other"}); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s with another cookie: %d", path, res.StatusCode)
		}
	}

	req, _ := http.NewRequest("GET", server.base+"/?key="+u.key, nil)
	req.Host = fmt.Sprintf("attacker.example:%d", server.port)
	if res, err := noRedirectClient.Do(req); err != nil || res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host: %v %v", res, err)
	}
}

// Checks that the step state is in the progress.
// That is the list of step names (`setupSteps`), the current step number, the step title lines,
// whether it finished and whether it failed, and the content of the result card (`done`)
func TestSetupPageSteps(t *testing.T) {
	server, u := startTestWebUI(t)
	session := map[string]string{"Cookie": setupCookieName + "=" + u.key}
	if state := readState(t, server, session); len(state.Steps) != len(setupSteps) || state.Current != 0 {
		t.Fatalf("before the first step: %+v", state)
	}
	u.step(1)
	u.step(2)
	state := readState(t, server, session)
	if state.Current != 2 || len(state.Lines) != 2 || state.Lines[1].Kind != "step" || state.Lines[1].Step != 2 || state.Lines[1].Text != setupSteps[1] {
		t.Errorf("after step 2: %+v", state)
	}
	u.finish(true)
	if state := readState(t, server, session); !state.Finished || !state.Failed || state.Current != 2 || state.Result != nil {
		t.Errorf("failed in step 2: %+v", state)
	}
	u.done("Sign in.", "Open the dashboard", "https://me-starkive.vercel.app")
	if state := readState(t, server, session); state.Result == nil || state.Result.Target != "https://me-starkive.vercel.app" {
		t.Errorf("result: %+v", state.Result)
	}
}

// Checks that prompts and answers reach the step logic.
//   - The prompt is in the progress (`/state`).
//     Only answers sent from this server's page (`Origin`) with the cookie are accepted
//   - The accepted answer goes back to the waiting `askSecret`. Answering the same prompt again gives 409
//   - A secret prompt leaves neither the answer nor a line saying it was answered
func TestSetupPageAnswers(t *testing.T) {
	server, u := startTestWebUI(t)
	session := map[string]string{"Cookie": setupCookieName + "=" + u.key}
	answered := make(chan string, 1)
	go func() { // `askSecret` blocks until an answer comes, so it is called in another goroutine
		value, _ := u.askSecret("Paste the token:")
		answered <- value
	}()

	// Waits until the prompt is in the progress
	var state webState
	for deadline := time.Now().Add(5 * time.Second); state.Prompt == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the question never showed up")
		}
		state = readState(t, server, session)
	}
	if state.Prompt.Text != "Paste the token:" || !state.Prompt.Secret || state.Prompt.Waiting != "" {
		t.Fatalf("prompt: %+v", state.Prompt)
	}

	body := fmt.Sprintf(`{"id":%d,"value":"token_4242"}`, state.Prompt.ID)
	for name, headers := range map[string]map[string]string{
		"no Origin":      {"Cookie": session["Cookie"]},
		"another Origin": {"Cookie": session["Cookie"], "Origin": "https://attacker.example"},
		"no cookie":      {"Origin": server.base},
	} {
		if res := send(t, "POST", server.base+"/answer", body, headers); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	allowed := map[string]string{"Cookie": session["Cookie"], "Origin": server.base, "Content-Type": "application/json"}
	if res := send(t, "POST", server.base+"/answer", body, allowed); res.StatusCode != http.StatusNoContent {
		t.Fatalf("answer: %d", res.StatusCode)
	}
	select {
	case value := <-answered:
		if value != "token_4242" {
			t.Errorf("askSecret returned %q", value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askSecret never returned")
	}
	if res := send(t, "POST", server.base+"/answer", body, allowed); res.StatusCode != http.StatusConflict {
		t.Errorf("second answer: %d", res.StatusCode)
	}
	if state = readState(t, server, session); len(state.Lines) != 0 {
		t.Errorf("lines after the secret answer: %+v", state.Lines)
	}
}

// Shows the waiting prompt (`waitForValue`) and waits until it is in the progress.
// When the wait ends, the result is sent to `done`
func startWaiting(t *testing.T, server *localServer, u *webUI, session map[string]string, received <-chan string, done chan<- waitResult) webState {
	go func() { // `waitForValue` blocks until it ends, so it is called in another goroutine
		_, result, _ := u.waitForValue("Waiting for your dashboard.", "Dashboard address:", "Enter it manually", received)
		done <- result
	}()
	var state webState
	for deadline := time.Now().Add(5 * time.Second); state.Prompt == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the waiting prompt never showed up")
		}
		state = readState(t, server, session)
	}
	return state
}

// Checks the prompt that waits for a value checked in the background.
//   - The prompt comes with the waiting text (`waiting`) and the link label (`manual_entry`). It has no input box, so it accepts no answer (409)
//   - When the value comes, the prompt is removed, and the value is kept as an answer line with the prompt
func TestSetupPageWaitReceives(t *testing.T) {
	server, u := startTestWebUI(t)
	session := map[string]string{"Cookie": setupCookieName + "=" + u.key}
	received := make(chan string, 1)
	done := make(chan waitResult, 1)
	state := startWaiting(t, server, u, session, received, done)
	if state.Prompt.Waiting != "Waiting for your dashboard." || state.Prompt.ManualEntry != "Enter it manually" {
		t.Errorf("prompt: %+v", state.Prompt)
	}
	allowed := map[string]string{"Cookie": session["Cookie"], "Origin": server.base, "Content-Type": "application/json"}
	if res := send(t, "POST", server.base+"/answer", fmt.Sprintf(`{"id":%d,"value":"x"}`, state.Prompt.ID), allowed); res.StatusCode != http.StatusConflict {
		t.Errorf("answer to a waiting prompt: %d", res.StatusCode)
	}
	received <- "https://me-starkive.vercel.app"
	select {
	case result := <-done:
		if result != receivedValue {
			t.Errorf("result: %v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForValue never returned")
	}
	state = readState(t, server, session)
	if state.Prompt != nil || len(state.Lines) != 1 || state.Lines[0].Kind != "answer" || state.Lines[0].Label != "https://me-starkive.vercel.app" {
		t.Errorf("after receiving: %+v", state)
	}
}

// Checks switching from the waiting prompt to typing the value in.
//   - Only a request from this server's page with the cookie is accepted.
//     Then the prompt is removed and the wait ends with `switchedToTyping`
//   - Sending again with the number of a past prompt gives 409
//   - It can't be sent for a plain input prompt (409)
func TestSetupPageSwitchToTyping(t *testing.T) {
	server, u := startTestWebUI(t)
	session := map[string]string{"Cookie": setupCookieName + "=" + u.key}
	done := make(chan waitResult, 1)
	state := startWaiting(t, server, u, session, nil, done)
	body := fmt.Sprintf(`{"id":%d}`, state.Prompt.ID)
	for name, headers := range map[string]map[string]string{
		"another Origin": {"Cookie": session["Cookie"], "Origin": "https://attacker.example"},
		"no cookie":      {"Origin": server.base},
	} {
		if res := send(t, "POST", server.base+"/switch-to-typing", body, headers); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	allowed := map[string]string{"Cookie": session["Cookie"], "Origin": server.base, "Content-Type": "application/json"}
	if res := send(t, "POST", server.base+"/switch-to-typing", body, allowed); res.StatusCode != http.StatusNoContent {
		t.Fatalf("switch: %d", res.StatusCode)
	}
	select {
	case result := <-done:
		if result != switchedToTyping {
			t.Errorf("result: %v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForValue never returned")
	}
	if res := send(t, "POST", server.base+"/switch-to-typing", body, allowed); res.StatusCode != http.StatusConflict {
		t.Errorf("second switch: %d", res.StatusCode)
	}
	if state = readState(t, server, session); state.Prompt != nil || len(state.Lines) != 0 {
		t.Errorf("after switching: %+v", state)
	}

	go u.ask("Dashboard address:", "the production domain")
	for deadline := time.Now().Add(5 * time.Second); state.Prompt == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the question never showed up")
		}
		state = readState(t, server, session)
	}
	if state.Prompt.Hint != "the production domain" || state.Prompt.Waiting != "" {
		t.Errorf("prompt: %+v", state.Prompt)
	}
	if res := send(t, "POST", server.base+"/switch-to-typing", fmt.Sprintf(`{"id":%d}`, state.Prompt.ID), allowed); res.StatusCode != http.StatusConflict {
		t.Errorf("switch on a plain question: %d", res.StatusCode)
	}
}

// Guards the hint for a browser on another computer.
// Only for a localhost address of this computer, it says to forward the same port over SSH
func TestBuildPortForwardHint(t *testing.T) {
	if hint := buildPortForwardHint("http://localhost:51234/github-app?state=x"); !strings.Contains(hint, "ssh -L 51234:127.0.0.1:51234") {
		t.Errorf("localhost: %q", hint)
	}
	if hint := buildPortForwardHint("https://vercel.com/new"); hint != "" {
		t.Errorf("remote address: %q", hint)
	}
}
