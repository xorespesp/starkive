package test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Imitates the scheduled sync request that the DB repository's workflow sends (`GET /api/sync-archive` with the correct sync key).
//
// Returns:
//   - The server's response
func callScheduledSync() *httptest.ResponseRecorder {
	cfg, err := core.LoadEnvConfig()
	if err != nil {
		return callScheduledSyncWithKey("") // If the environment variable is wrong and no key can be made, sends without a key. The server refuses it for the same environment variable
	}
	return callScheduledSyncWithKey(core.DeriveSyncKey(cfg))
}

// Sends the scheduled sync request (`GET /api/sync-archive`) with the given sync key.
//
// Args:
//   - `key`: The sync key to put in `Authorization: Bearer`. If empty, the header isn't sent
//
// Returns:
//   - The server's response
func callScheduledSyncWithKey(key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test/api/sync-archive", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	api.HandleSyncArchive(rec, req)
	return rec
}

// Reads the environment variables and makes the server config with the DB repository filled in (`core.LoadConfig`).
// Stops the test on failure.
func loadTestConfig(t *testing.T) *core.Config {
	cfg, err := core.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Reads the auto sync status (`GET /api/sync-status`).
// Stops the test if the response isn't 200.
//
// Args:
//   - `cookie`: The session cookie (the value `login` returned)
func readSyncStatus(t *testing.T, cookie string) core.SyncStatus {
	rec := call(api.HandleSyncStatus, "GET", "/api/sync-status", "", cookie)
	var status core.SyncStatus
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &status) != nil {
		t.Fatalf("sync status: %d %s", rec.Code, rec.Body)
	}
	return status
}

// Base time for testing the scheduled sync's date math. It matches the workflow's scheduled time (18:17 UTC)
var scheduledAt = time.Date(2026, 10, 14, 18, 17, 0, 0, time.UTC)

// Runs one scheduled call per day for three days from `scheduledAt`, with a 3-day interval.
// Checks that exactly one day synced, and returns that day.
// Stops the test on failure. Set the interval in the config file to 3 days before calling.
//
// Returns:
//   - The index of the day that synced (0, 1 or 2). It is that many days after `scheduledAt`
//
// NOTE: The sync day comes from date math (days counted from 1970-01-01), so it isn't known when the test is written.
// That is why the test finds it this way.
func findSyncDay(t *testing.T, cfg *core.Config) int {
	due := -1
	for day := 0; day < 3; day++ { // Runs each of the three days and finds the day that synced
		got, err := core.RunScheduledSync(cfg, time.Minute, scheduledAt.AddDate(0, 0, day))
		if err != nil {
			t.Fatal(err)
		}
		if got == "synced" {
			if due >= 0 {
				t.Fatalf("3-day interval synced on days %d and %d", due, day)
			}
			due = day
		}
	}
	if due < 0 {
		t.Fatal("3-day interval never synced in three days")
	}
	return due
}

// Tests that a scheduled call syncs only on days that match the interval.
//   - With a daily interval (the default), every call syncs and adds new Stars to the archive
//   - Changing the interval to 3 days is saved in `config.json` (App settings stay as they are), and only one day in three syncs
//   - A scheduled run that is a few hours late and passes midnight UTC still counts as the same day
//   - An interval that can't be chosen (0, 5, -1) and an empty request get 400
func TestScheduledSync(t *testing.T) {
	gh := setup(t, "", "")
	gh.starred = []string{"a/one"}

	if rec := callScheduledSync(); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"synced"}` {
		t.Fatalf("daily: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(gh.readFile(core.StarItemsDBPath), "a/one") {
		t.Errorf("star items: %s", gh.readFile(core.StarItemsDBPath))
	}

	cookie := login(t)
	if rec := call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", `{"sync_interval_days":3}`, cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"interval_days":3`) {
		t.Fatalf("set interval: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(gh.readFile(core.ConfigDBPath), `"interval_days": 3`) || !strings.Contains(gh.readFile(core.ConfigDBPath), `"sign_in_app"`) {
		t.Errorf("config (the interval changes, the sign-in app stays): %s", gh.readFile(core.ConfigDBPath))
	}

	// The scheduled run on the sync day is 10 hours late and arrives at 04:17 UTC the next day
	cfg := loadTestConfig(t)
	due := findSyncDay(t, cfg)
	late := scheduledAt.AddDate(0, 0, due).Add(10 * time.Hour)
	if got, err := core.RunScheduledSync(cfg, time.Minute, late); err != nil || got != "synced" {
		t.Errorf("a run delayed past midnight UTC: %q %v", got, err)
	}

	// Intervals that can't be chosen
	for _, body := range []string{`{"sync_interval_days":0}`, `{"sync_interval_days":5}`, `{"sync_interval_days":-1}`, `{}`} {
		if rec := call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", body, cookie); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
}

// Tests how the scheduled sync handles failures.
//   - A failed sync answers 502. Then the workflow fails and GitHub notifies the user.
//     The response doesn't include details such as the repository name
//   - If the last finished scheduled run failed, it syncs again even on a day that doesn't match the interval
//   - Only finished scheduled runs count for that check.
//     It leaves out the run in progress (the run that sent the scheduled call) and manual runs (`workflow_dispatch`),
//     and looks at the one that started last, whatever the order of the list
func TestScheduledSyncRetriesAfterFailure(t *testing.T) {
	gh := setup(t, "", "")
	gh.starringDenied = true
	rec := callScheduledSync()
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Sync failed") || strings.Contains(rec.Body.String(), "me/archive") {
		t.Fatalf("failed sync: %d %s", rec.Code, rec.Body)
	}
	gh.mu.Lock()
	gh.starringDenied = false
	gh.files[core.ConfigDBPath] = strings.Replace(gh.files[core.ConfigDBPath], `"interval_days":1`, `"interval_days":3`, 1)
	gh.mu.Unlock()

	cfg := loadTestConfig(t)
	due := findSyncDay(t, cfg)
	notDue := scheduledAt.AddDate(0, 0, (due+1)%3)
	// One workflow run that started `hoursAgo` hours before `notDue`
	run := func(event string, status string, conclusion string, hoursAgo int) fakeRun {
		return fakeRun{event: event, status: status, conclusion: conclusion, started: notDue.Add(-time.Duration(hoursAgo) * time.Hour)}
	}
	// On a day that doesn't match the interval, checks the result while changing only the run list
	for _, c := range []struct {
		name string
		runs []fakeRun
		want string
	}{
		{"never ran", nil, "not_due"},
		{"last failed", []fakeRun{run("schedule", "completed", "success", 48), run("schedule", "completed", "failure", 24)}, "synced"},
		{"failed, then succeeded", []fakeRun{run("schedule", "completed", "success", 24), run("schedule", "completed", "failure", 48)}, "not_due"},
		{"only the running call itself is newer", []fakeRun{run("schedule", "in_progress", "", 0), run("schedule", "completed", "success", 24)}, "not_due"},
		{"a manual run failed", []fakeRun{run("workflow_dispatch", "completed", "failure", 1), run("schedule", "completed", "success", 24)}, "not_due"},
	} {
		gh.mu.Lock()
		gh.runs = c.runs
		gh.mu.Unlock()
		if got, err := core.RunScheduledSync(cfg, time.Minute, notDue); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
}

// Returns the number of commits made so far on the fake GitHub
func countCommits(gh *fakeGitHub) int {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return len(gh.commits)
}

// Reads the time auto sync was turned on (`sync.enabled_since`) from the config file. `nil` if it is off (no key)
func readEnabledSince(t *testing.T, gh *fakeGitHub) *time.Time {
	var file struct {
		Sync struct {
			EnabledSince *time.Time `json:"enabled_since"`
		} `json:"sync"`
	}
	if err := json.Unmarshal([]byte(gh.readFile(core.ConfigDBPath)), &file); err != nil {
		t.Fatal(err)
	}
	return file.Sync.EnabledSince
}

// Tests the answer of the auto sync status (`GET /api/sync-status`) when it can't be read.
//   - If GitHub refuses a read (no Actions permission), it is 502 with GitHub's message and the needed permission.
//     (The page uses this message to warn that the status couldn't be read.)
//   - An error that isn't GitHub's answer (a cut connection) is 500 "Internal server error", without internal details such as the request address
//   - The sign-in status (`GET /api/login-session`) doesn't carry the auto sync status, so it still answers when only this can't be read
func TestSyncStatusReadFailureIsReported(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	read := func() (int, string) {
		rec := call(api.HandleSyncStatus, "GET", "/api/sync-status", "", cookie)
		return rec.Code, rec.Body.String()
	}
	if code, body := read(); code != http.StatusOK || !strings.Contains(body, `"workflow"`) {
		t.Errorf("readable: %d %s", code, body)
	}
	gh.mu.Lock()
	gh.actionsDenied = true
	gh.mu.Unlock()
	if code, body := read(); code != http.StatusBadGateway || !strings.Contains(body, "actions=read") {
		t.Errorf("no Actions permission: %d %s", code, body)
	}
	if rec := call(api.HandleLoginSession, "GET", "/api/login-session", "", cookie); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"repo":"me/archive"}` {
		t.Errorf("sign-in status: %d %s", rec.Code, rec.Body)
	}
	gh.mu.Lock()
	gh.actionsDenied = false
	gh.contentsHangup = true
	gh.mu.Unlock()
	if code, body := read(); code != http.StatusInternalServerError || !strings.Contains(body, "Internal server error") || strings.Contains(body, "/repos/") {
		t.Errorf("internal error: %d %s", code, body)
	}
}

// Tests that turning auto sync on or off (`enabled` in `PATCH /api/sync-archive`) changes two things:
// the time it was turned on in the config file, and the workflow itself in the DB repository.
//   - Turning it off disables the workflow and removes the time it was turned on.
//     Turning it off again when it is already off writes nothing to GitHub or the repository.
//     (Disabling a disabled workflow again gives 403, measured)
//   - Turning it on and the interval can change in one request. Turning it on records the time
//   - Only reading the status (only opening the dashboard or Settings) makes no commit
//   - A workflow that GitHub disabled for inactivity (`disabled_inactivity`) only shows in the status.
//     Turning it on again records a new time
//   - If the setting is off but the workflow is on, turning it off disables only the workflow and makes no commit
//   - If there is no workflow, the status is `missing`, and turning it on gets 409 asking to run the setup tool
func TestSyncCanBeTurnedOnAndOff(t *testing.T) {
	gh := setup(t, "", "")
	cookie := login(t)
	patch := func(body string) *httptest.ResponseRecorder {
		return call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", body, cookie)
	}

	for i := 0; i < 2; i++ { // The second turn-off finds it already off, so it must write nothing to GitHub or the repository
		commits := countCommits(gh)
		if rec := patch(`{"enabled":false}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workflow":"disabled_manually"`) || !strings.Contains(rec.Body.String(), `"enabled_since":null`) {
			t.Fatalf("turn off %d: %d %s", i, rec.Code, rec.Body)
		}
		if want := 1 - i; countCommits(gh)-commits != want {
			t.Errorf("turn off %d: %d commits, want %d", i, countCommits(gh)-commits, want)
		}
	}
	if readEnabledSince(t, gh) != nil {
		t.Errorf("config after turning off: %s", gh.readFile(core.ConfigDBPath))
	}
	if rec := patch(`{"enabled":true,"sync_interval_days":7}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workflow":"active"`) || !strings.Contains(rec.Body.String(), `"interval_days":7`) {
		t.Fatalf("turn on with an interval: %d %s", rec.Code, rec.Body)
	}
	firstOn := readEnabledSince(t, gh)
	if firstOn == nil {
		t.Fatalf("config after turning on: %s", gh.readFile(core.ConfigDBPath))
	}
	gh.mu.Lock()
	toggles := gh.workflowToggles
	gh.mu.Unlock()
	if toggles != 2 {
		t.Errorf("workflow toggles: %d, want 2 (off once, on once)", toggles)
	}

	// Only reading the status makes no commit
	commits := countCommits(gh)
	readSyncStatus(t, cookie)
	if countCommits(gh) != commits {
		t.Errorf("reading the status made %d commits", countCommits(gh)-commits)
	}

	// A workflow that GitHub disabled for inactivity only shows in the status. Turning it on again records a new time
	gh.mu.Lock()
	gh.workflowState = "disabled_inactivity"
	gh.mu.Unlock()
	if status := readSyncStatus(t, cookie); status.Workflow != "disabled_inactivity" || status.EnabledSince == nil {
		t.Errorf("status: %+v", status)
	}
	time.Sleep(1100 * time.Millisecond) // The time is in seconds, so wait over a second to tell the new time apart
	if rec := patch(`{"enabled":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workflow":"active"`) {
		t.Errorf("turn on after inactivity: %d %s", rec.Code, rec.Body)
	}
	if again := readEnabledSince(t, gh); again == nil || !again.After(*firstOn) {
		t.Errorf("enabled_since after turning on again: %v, before %v", again, firstOn)
	}

	// The setting is off but the workflow is on (turned on in the Actions tab). Turning it off disables only the workflow
	gh.mu.Lock()
	gh.files[core.ConfigDBPath] = "{\n  \"sync\": {\n    \"interval_days\": 1\n  }\n}\n" // Same layout the server writes, so if turning off has nothing to change, there is no commit
	gh.mu.Unlock()
	if status := readSyncStatus(t, cookie); status.Workflow != "active" || status.EnabledSince != nil {
		t.Fatalf("status (off in settings, on in GitHub): %+v", status)
	}
	commits = countCommits(gh)
	if rec := patch(`{"enabled":false}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workflow":"disabled_manually"`) || countCommits(gh) != commits {
		t.Errorf("turn off the workflow only: %d %s, %d commits", rec.Code, rec.Body, countCommits(gh)-commits)
	}

	gh.mu.Lock()
	delete(gh.files, core.SyncWorkflowPath)
	gh.mu.Unlock()
	if status := readSyncStatus(t, cookie); status.Workflow != "missing" || status.LastScheduledRun != nil {
		t.Errorf("status without the workflow: %+v", status)
	}
	if rec := patch(`{"enabled":true}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "setup tool") {
		t.Errorf("turn on without the workflow: %d %s", rec.Code, rec.Body)
	}
}

// Tests Sync now (`POST /api/sync-archive`) and the auto sync status that the Settings dialog gets from the sign-in status response.
//   - Status: interval, time turned on, workflow state, and the last finished scheduled run (start time, result, GitHub run page address)
//   - Sync now needs sign-in and a same-site request. It syncs on every call, whatever the interval
func TestSyncNowAndStatus(t *testing.T) {
	gh := setup(t, "", "")
	gh.starred = []string{"a/one"}
	lastRun := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	gh.runs = []fakeRun{
		{event: "schedule", status: "completed", conclusion: "success", started: lastRun.Add(-24 * time.Hour)},
		{event: "schedule", status: "completed", conclusion: "failure", started: lastRun},
	}
	cookie := login(t)

	status := readSyncStatus(t, cookie)
	if status.IntervalDays != 1 || status.Workflow != "active" || status.EnabledSince == nil {
		t.Errorf("status: %+v", status)
	}
	if last := status.LastScheduledRun; last == nil || !last.StartedAt.Equal(lastRun) || last.Conclusion != "failure" || !strings.HasPrefix(last.URL, "https://github.com/") {
		t.Errorf("last scheduled run: %+v", last)
	}

	if rec := call(api.HandleSyncArchive, "POST", "/api/sync-archive", "", ""); rec.Code != 401 {
		t.Errorf("no session: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "http://example.test/api/sync-archive", nil)
	req.Header.Set("Origin", "https://evil.test")
	req.Header.Set("Cookie", cookie)
	cross := httptest.NewRecorder()
	api.HandleSyncArchive(cross, req)
	if cross.Code != 403 {
		t.Errorf("cross-origin: %d", cross.Code)
	}
	for i, want := range []string{`"added":1`, `"added":0`} { // The second call syncs too. There are no new Stars, so it is 0
		if rec := call(api.HandleSyncArchive, "POST", "/api/sync-archive", "", cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("sync now %d: %d %s", i, rec.Code, rec.Body)
		}
	}
}

// Tests that each function that writes the config file changes only its own field.
// It checks the two functions the setup tool uses.
//   - Recording the dashboard address keeps an interval that can't be chosen (5), and adds no time turned on
//   - Recording that auto sync is on adds only the time turned on. Calling it again makes no commit
//   - An interval that can't be chosen reads as the default (1)
func TestConfigWritesTouchOnlyTheirField(t *testing.T) {
	gh := setup(t, "", "")
	gh.mu.Lock()
	gh.files[core.ConfigDBPath] = `{"dashboard_url":"https://old.test","sync":{"interval_days":5}}` + "\n"
	gh.mu.Unlock()
	cfg := loadTestConfig(t)

	if err := core.SetDashboardURL(cfg, "https://new.test"); err != nil {
		t.Fatal(err)
	}
	config := gh.readFile(core.ConfigDBPath)
	if !strings.Contains(config, `"dashboard_url": "https://new.test"`) || !strings.Contains(config, `"interval_days": 5`) || strings.Contains(config, "enabled_since") {
		t.Errorf("after setting the dashboard address: %s", config)
	}

	if err := core.RecordSyncEnabled(cfg); err != nil {
		t.Fatal(err)
	}
	commits := countCommits(gh)
	if err := core.RecordSyncEnabled(cfg); err != nil {
		t.Fatal(err)
	}
	config = gh.readFile(core.ConfigDBPath)
	if readEnabledSince(t, gh) == nil || !strings.Contains(config, `"interval_days": 5`) || countCommits(gh) != commits {
		t.Errorf("after recording auto sync on twice (%d new commits): %s", countCommits(gh)-commits, config)
	}

	if status, err := core.ReadSyncStatus(cfg); err != nil || status.IntervalDays != 1 {
		t.Errorf("status with an interval that can't be chosen: %+v %v", status, err)
	}
}

// Tests that the response to an auto sync settings change has the new settings.
// This must hold even while GitHub briefly shows the just-moved branch at the old commit.
//   - Reading again by branch name at the same moment shows the old settings.
//     (This confirms that the fake GitHub's `branchReadsLag` creates that situation)
//   - Still, the response is made from what was written, so it has the new settings
//   - Sending on/off and the interval together writes one commit
func TestSyncSettingsAnswerWithWhatWasWritten(t *testing.T) {
	gh := setup(t, "", "")
	cookie := login(t)
	gh.mu.Lock()
	gh.branchReadsLag = true
	gh.mu.Unlock()

	rec := call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", `{"enabled":false}`, cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled_since":null`) || !strings.Contains(rec.Body.String(), `"workflow":"disabled_manually"`) {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body)
	}
	if status := readSyncStatus(t, cookie); status.EnabledSince == nil {
		t.Errorf("a plain read right after the write should still see the old config here: %+v", status)
	}

	commits := countCommits(gh)
	rec = call(api.HandleSyncArchive, "PATCH", "/api/sync-archive", `{"enabled":true,"sync_interval_days":3}`, cookie)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"enabled_since":null`) || !strings.Contains(rec.Body.String(), `"interval_days":3`) {
		t.Fatalf("turn on with an interval: %d %s", rec.Code, rec.Body)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if n := len(gh.commits) - commits; n != 1 || !strings.HasPrefix(gh.commits[len(gh.commits)-1], "user(config): sync every 3 days, turn on auto sync") {
		t.Errorf("%d commits, last %q", n, gh.commits[len(gh.commits)-1])
	}
}
