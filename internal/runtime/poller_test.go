package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

const secretToken = "sk-ant-oat01-SECRET-TOKEN-DO-NOT-LOG"

// usagePayload is a real usage-endpoint body from the quota fixtures.
func usagePayload(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "quota", "testdata", "usage_early_week.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pollFixture wires two Claude OAuth files, one disabled Claude file, one
// Claude API-key record with no token, and one Gemini file into the fake host.
func pollFixture(t *testing.T, tp *testPlugin) {
	t.Helper()
	tp.host.files = []HostAuthFileEntry{
		{ID: "claude-a.json", AuthIndex: "idx-a", Name: "claude-a.json", Type: "claude", Provider: "claude", Email: "a@example.com", Status: "active", Priority: 3},
		{ID: "claude-b.json", AuthIndex: "idx-b", Name: "claude-b.json", Type: "claude", Provider: "claude", Label: "Seat B", Status: "active", Priority: 3},
		{ID: "claude-off.json", AuthIndex: "idx-off", Name: "claude-off.json", Type: "claude", Provider: "claude", Disabled: true, Status: "disabled"},
		{ID: "claude-key", AuthIndex: "idx-key", Name: "claude-key", Type: "claude", Provider: "claude", Status: "active"},
		{ID: "gemini.json", AuthIndex: "idx-g", Name: "gemini.json", Type: "gemini", Provider: "gemini", Status: "active"},
	}
	tp.host.auths["idx-a"] = json.RawMessage(`{"type":"claude","email":"a@example.com","access_token":"` + secretToken + `","refresh_token":"rt-a"}`)
	tp.host.auths["idx-b"] = json.RawMessage(`{"type":"claude","access_token":"` + secretToken + `-b","refresh_token":"rt-b"}`)
	tp.host.auths["idx-off"] = json.RawMessage(`{"type":"claude","access_token":"off"}`)
	tp.host.auths["idx-key"] = json.RawMessage(`{"type":"claude","api_key":"sk-ant-api03-KEY"}`)
	tp.host.auths["idx-g"] = json.RawMessage(`{"type":"gemini","access_token":"g"}`)
	body := usagePayload(t)
	tp.host.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		if req.URL != model.DefaultUsageURL {
			return HostHTTPResponse{StatusCode: 404}, nil
		}
		if !strings.HasPrefix(req.Headers.Get("Authorization"), "Bearer "+secretToken) {
			return HostHTTPResponse{StatusCode: 401}, nil
		}
		return HostHTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	}
}

func TestPollFetchesGovernedOAuthCredentials(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)

	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	snaps := tp.quota.All()
	if len(snaps) != 2 || snaps[0].AuthID != "claude-a.json" || snaps[1].AuthID != "claude-b.json" {
		ids := make([]string, 0, len(snaps))
		for _, s := range snaps {
			ids = append(ids, s.AuthID)
		}
		t.Fatalf("snapshots = %v, want exactly the two enabled Claude OAuth credentials", ids)
	}
	for _, s := range snaps {
		if len(s.Windows) == 0 || s.Err != "" || !s.ObservedAt.Equal(testNow) {
			t.Errorf("snapshot %s = err %q windows %d observed %v", s.AuthID, s.Err, len(s.Windows), s.ObservedAt)
		}
	}
	if snaps[0].Label != "a@example.com" || snaps[1].Label != "Seat B" {
		t.Errorf("labels = %q %q, want email then host label", snaps[0].Label, snaps[1].Label)
	}
	if tp.host.count(MethodHostHTTPDo) != 2 {
		t.Errorf("host.http.do called %d times, want 2", tp.host.count(MethodHostHTTPDo))
	}
	// The token travels only inside host.http.do's Authorization header.
	for _, method := range tp.host.methodsContaining("SECRET-TOKEN") {
		if method != MethodHostHTTPDo {
			t.Errorf("the access token reached %s", method)
		}
	}
	for _, l := range tp.host.logs() {
		if raw, _ := json.Marshal(l); strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "rt-a") {
			t.Errorf("credential material reached host.log: %s", raw)
		}
	}

	status := tp.Status(testNow, "")
	if len(status.Auths) != 4 {
		t.Fatalf("status rows = %d, want the four governed entries", len(status.Auths))
	}
	for _, row := range status.Auths {
		if row.Provider != "claude" {
			t.Errorf("row %s provider = %q", row.AuthID, row.Provider)
		}
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "claude-key") {
		t.Errorf("warnings = %v, want only the API-key record flagged as having no snapshot", status.Warnings)
	}
}

func TestPollRecordsFailuresAndPrunesRemovedCredentials(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The endpoint starts refusing one credential and the other disappears.
	tp.host.mu.Lock()
	tp.host.files = tp.host.files[:1]
	tp.host.mu.Unlock()
	tp.host.http = func(HostHTTPRequest) (HostHTTPResponse, error) { return HostHTTPResponse{StatusCode: 429}, nil }
	// A refresh whose every usage fetch failed reports it, so an operator
	// debugging a dead poller does not read a green light.
	if err := tp.refresh(context.Background()); err == nil {
		t.Error("refresh reported success with every usage fetch failing")
	}

	snaps := tp.quota.All()
	if len(snaps) != 1 || snaps[0].AuthID != "claude-a.json" {
		t.Fatalf("snapshots after prune = %+v, want claude-a only", snaps)
	}
	if snaps[0].ErrCategory != string(quota.CategoryRateLimited) || len(snaps[0].Windows) == 0 {
		t.Errorf("snapshot = err %q category %q windows %d, want prior readings kept with the new error", snaps[0].Err, snaps[0].ErrCategory, len(snaps[0].Windows))
	}
	status := tp.Status(testNow, "")
	joined := strings.Join(status.Warnings, "\n")
	if !strings.Contains(joined, "quota poll failing for claude-a.json (rate-limited)") {
		t.Errorf("warnings = %v, want the poll failure with its category", status.Warnings)
	}

	// A failing auth.list is reported and keeps the previous state.
	tp.host.mu.Lock()
	tp.host.listErr = errors.New("core auth manager unavailable")
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "auth manager") {
		t.Errorf("refresh error = %v, want the list failure", err)
	}
	if len(tp.quota.All()) != 1 {
		t.Error("a list failure pruned the store")
	}
}

func TestPollSkipsWhenDisabled(t *testing.T) {
	tp := newTestPlugin(t, "enabled: false\n")
	pollFixture(t, tp)
	_ = tp.refresh(context.Background())
	if tp.host.count(MethodHostAuthList) != 0 {
		t.Error("a disabled plugin polled the host")
	}
}

func TestDisabledPollClearsAStaleListFailure(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.host.mu.Lock()
	tp.host.listErr = errors.New("core auth manager unavailable")
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil {
		t.Fatal("refresh hid the list failure")
	}

	tp.register(t, MethodPluginReconfigure, "enabled: false\n")
	if err := tp.refresh(context.Background()); err != nil {
		t.Errorf("refresh on a disabled plugin = %v, want the stale failure cleared", err)
	}
	if w := tp.Status(testNow, ""); len(w.Warnings) != 1 || !strings.Contains(w.Warnings[0], "disabled") {
		t.Errorf("warnings = %v, want only the disabled notice", w.Warnings)
	}
}

func TestPollCutShortKeepsThePreviousView(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(tp.Status(testNow, "").Auths)

	// A refresh whose deadline expires part-way must not publish the half-read
	// credential list.
	tp.host.httpGate = make(chan struct{})
	defer close(tp.host.httpGate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tp.refresh(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refresh error = %v, want the deadline", err)
	}
	if got := len(tp.Status(testNow, "").Auths); got != before {
		t.Errorf("status rows = %d, want the %d from the last complete poll", got, before)
	}
	if len(tp.quota.All()) != 2 {
		t.Errorf("snapshots = %d, want both kept", len(tp.quota.All()))
	}
}

// TestPollCutShortReportsItsOwnDeadline covers a cut-short poll that follows a
// failed one: the listing it did complete has to clear the earlier failure, or
// refresh and the status view both keep naming a listing that now works.
func TestPollCutShortReportsItsOwnDeadline(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.host.mu.Lock()
	tp.host.listErr = errors.New("core auth manager unavailable")
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil {
		t.Fatal("refresh hid the list failure")
	}

	tp.host.mu.Lock()
	tp.host.listErr = nil
	tp.host.mu.Unlock()
	tp.host.httpGate = make(chan struct{})
	defer close(tp.host.httpGate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tp.refresh(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refresh error = %v, want the deadline rather than the earlier listing failure", err)
	}
	if hasWarning(tp.Status(testNow, ""), "credential listing is failing") {
		t.Errorf("warnings = %v, want the listing failure gone after a listing that succeeded", tp.Status(testNow, "").Warnings)
	}
}

// TestEmptyGovernedListingKeepsThePreviousView covers the host answering a
// listing it cannot serve with an empty set and no error.
func TestEmptyGovernedListingKeepsThePreviousView(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(tp.Status(testNow, "").Auths)

	tp.host.mu.Lock()
	tp.host.files = nil
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil {
		t.Error("refresh reported success on a listing that named no governed credential")
	}
	if got := len(tp.quota.All()); got != 2 {
		t.Errorf("snapshots = %d, want the two from the last complete poll", got)
	}
	if got := len(tp.Status(testNow, "").Auths); got != before {
		t.Errorf("status rows = %d, want the %d from the last complete poll", got, before)
	}
	if !hasWarning(tp.Status(testNow, ""), errNoGovernedCredential) {
		t.Errorf("warnings = %v, want the empty listing surfaced", tp.Status(testNow, "").Warnings)
	}
}

func TestHostDoerHonoursContext(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	defer close(h.httpGate)
	d := hostDoer{h: newHost(h.call)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Do took %v, want it bounded by its 20ms context deadline", elapsed)
	}
}

func TestShutdownWaitsForAnAbandonedHostCall(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	hst := newHost(h.call)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (hostDoer{h: hst}).Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL}); err == nil {
		t.Fatal("Do waited for the wedged host")
	}

	// The call is still parked in the host, so the drain does not return until
	// it does: after this point the host frees its callback table and unloads
	// the library.
	drained := make(chan struct{})
	go func() {
		hst.drain(2 * time.Second)
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drain returned while a host call was still outstanding")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.httpGate)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not return after the host answered")
	}
}

func TestDrainGivesUpOnAHostThatNeverAnswers(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	defer close(h.httpGate)
	hst := newHost(h.call)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _ = (hostDoer{h: hst}).Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL})

	start := time.Now()
	hst.drain(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("drain took %v, want it bounded by its 50ms deadline", elapsed)
	}
}

// TestDrainRunsBesideCallbacksThatStartWhileItWaits covers plugin.quiesce
// racing a management refresh: the drain and the callbacks it does not cover
// run concurrently, and the drain must survive a callback starting as the
// count it is watching reaches zero.
func TestDrainRunsBesideCallbacksThatStartWhileItWaits(t *testing.T) {
	h := newFakeHost()
	hst := newHost(h.call)

	stop := make(chan struct{})
	calling := make(chan struct{})
	go func() {
		defer close(calling)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = hst.invokeCtx(context.Background(), MethodHostLog, HostLogRequest{Level: "debug"}, nil)
		}
	}()
	for i := 0; i < 3000; i++ {
		hst.drain(time.Second)
	}
	close(stop)
	<-calling
}

// TestTimedOutDrainsLeaveNoGoroutineBehind covers the hot-reload loop: every
// unload drains, and a host that never answers must not cost a goroutine per
// attempt.
func TestTimedOutDrainsLeaveNoGoroutineBehind(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	defer close(h.httpGate)
	hst := newHost(h.call)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _ = (hostDoer{h: hst}).Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL})

	const drains = 20
	before := goruntime.NumGoroutine()
	for i := 0; i < drains; i++ {
		hst.drain(time.Millisecond)
	}
	if grew := goruntime.NumGoroutine() - before; grew > drains/4 {
		t.Errorf("%d drains left %d goroutines behind", drains, grew)
	}
}

// TestHostLogGivesUpOnAWedgedHost covers the poll loop, which logs between the
// steps plugin.shutdown waits on, and the panic guard, which logs for every
// method including scheduler.pick.
func TestHostLogGivesUpOnAWedgedHost(t *testing.T) {
	h := newFakeHost()
	h.logGate = make(chan struct{})
	defer close(h.logGate)
	hst := newHost(h.call)

	logged := make(chan struct{})
	go func() {
		defer close(logged)
		hst.log("warn", "the host never answers", nil)
	}()
	select {
	case <-logged:
	case <-time.After(hostLogTimeout + 2*time.Second):
		t.Fatal("host.log parked its caller on a host that never answered")
	}
}

// TestAPanickingPickDoesNotWaitOnTheHostLogger pins the pick path's only host
// call. scheduler.pick has no timeout, so the panic guard's line goes out on
// its own goroutine.
func TestAPanickingPickDoesNotWaitOnTheHostLogger(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.host.logGate = make(chan struct{})
	defer close(tp.host.logGate)
	tp.handle = func(string, []byte) ([]byte, error) { panic("boom") }

	payload := mustJSON(t, pickRequest(fableModel, "k", "seat-a"))
	picked := make(chan SchedulerPickResponse, 1)
	go func() {
		var out SchedulerPickResponse
		raw, _ := tp.Call(MethodSchedulerPick, payload)
		_ = unwrapEnvelope(raw, &out)
		picked <- out
	}()
	select {
	case resp := <-picked:
		if resp.Handled {
			t.Errorf("pick = %+v, want a decline", resp)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the pick waited on the host logger")
	}
}

func TestHostDoerTranslatesResponse(t *testing.T) {
	h := newFakeHost()
	var seen HostHTTPRequest
	h.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		seen = req
		return HostHTTPResponse{StatusCode: 200, Headers: http.Header{"X-Test": {"1"}}, Body: []byte(`{}`)}, nil
	}
	d := hostDoer{h: newHost(h.call)}
	resp, err := d.Do(context.Background(), quota.Request{Method: "GET", URL: "https://u", Header: map[string]string{"Authorization": "Bearer x"}})
	if err != nil || resp.StatusCode != 200 || resp.Header["X-Test"][0] != "1" || string(resp.Body) != "{}" {
		t.Errorf("resp = %+v err = %v", resp, err)
	}
	if seen.Method != "GET" || seen.URL != "https://u" || seen.Headers.Get("Authorization") != "Bearer x" {
		t.Errorf("host saw %+v", seen)
	}
}

// A cold store whose very first poll fails knows nothing about any credential's
// caps. Reporting that as "no bearing window" would tell an operator the seat
// carries no cap for the model, sending them to the wrong place entirely.
func TestFirstPollFailureReportsTheFailedRead(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.host.http = func(HostHTTPRequest) (HostHTTPResponse, error) { return HostHTTPResponse{StatusCode: 429}, nil }

	if err := tp.refresh(context.Background()); err == nil {
		t.Fatal("refresh reported success with every usage fetch failing")
	}

	status := tp.Status(testNow, "")
	seen := 0
	for _, row := range status.Auths {
		if row.Snapshot.Err == "" {
			continue
		}
		seen++
		if row.Score.Reason != model.ReasonFetchFailed {
			t.Errorf("%s reason = %q, want %q", row.AuthID, row.Score.Reason, model.ReasonFetchFailed)
		}
	}
	if seen == 0 {
		t.Fatal("no credential recorded a failed read")
	}
}

// The usage endpoint throttles the caller rather than the credential, so reads
// that arrive together are what earns a 429. The poll spaces them, and a
// cancelled context ends the spacing rather than being held for it.
func TestFetchesAreStaggered(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.fetchStagger = 40 * time.Millisecond

	var at []time.Time
	inner := tp.host.http
	tp.host.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		at = append(at, time.Now())
		return inner(req)
	}

	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(at) != 2 {
		t.Fatalf("usage reads = %d, want one per governed OAuth credential", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < tp.fetchStagger {
		t.Errorf("gap between reads = %v, want at least %v", gap, tp.fetchStagger)
	}

	// A context already done stops the poll at the stagger, so the second
	// credential is not read and the first poll's view stands.
	at = nil
	tp.fetchStagger = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = tp.refresh(ctx)
	if len(at) > 1 {
		t.Errorf("usage reads = %d, want the poll cut short at the stagger", len(at))
	}
}

// TestPollKeepsHistoryForCredentialsItDoesNotFetch covers a credential the
// host lists but the poll never fetches — disabled, runtime-only, or carrying
// no access token. Response headers give such a seat a snapshot and a
// recorded history, and pruning against the fetch set would discard both on
// the very next poll.
func TestPollKeepsHistoryForCredentialsItDoesNotFetch(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)

	headerWindow := []model.Window{{
		Kind: model.WindowSession, Utilization: 0.42,
		ResetsAt: testNow.Add(2 * time.Hour), Duration: model.SessionDuration,
	}}
	for _, id := range []string{"claude-off.json", "claude-key"} {
		tp.quota.MergeHeaders(id, headerWindow, testNow)
	}

	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, id := range []string{"claude-off.json", "claude-key"} {
		if h := tp.quota.History(id, 0); len(h) != 1 || h[0].Samples() != 1 {
			t.Errorf("history for %s after one poll = %+v, want the header sample kept", id, h)
		}
	}

	// A credential the host stops listing does go, and a new reading for the
	// same id picks its samples back up.
	tp.host.mu.Lock()
	tp.host.files = tp.host.files[:2]
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh after removal: %v", err)
	}
	if h := tp.quota.History("claude-off.json", 0); h != nil {
		t.Errorf("an unlisted credential kept its status row: %+v", h)
	}
	tp.quota.MergeHeaders("claude-off.json", headerWindow, testNow.Add(time.Hour))
	if h := tp.quota.History("claude-off.json", 0); len(h) != 1 || h[0].Samples() != 2 {
		t.Errorf("history after the credential returned = %+v, want the earlier sample adopted", h)
	}
}

// TestStopPollerSavesUnderThePollLock covers the second writer to the history
// file: joining the poll loop leaves out a management refresh, which runs a
// poll and its save inline on the HTTP goroutine, so the stop path waits on
// the same lock a poll's own save holds.
func TestStopPollerSavesUnderThePollLock(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	tp.pollMu.Lock()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tp.Shutdown()
	}()
	select {
	case <-stopped:
		tp.pollMu.Unlock()
		t.Fatal("the stop path wrote the history file while a poll held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	tp.pollMu.Unlock()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop path never finished once the poll lock was free")
	}
}

func TestBackgroundPollSweepsExpiredBindings(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	ttl := tp.config().Affinity.TTL
	tp.bindings.Bind("claude", fableModel, "idle", "claude-a.json", testNow.Add(-2*ttl))
	tp.bindings.Bind("claude", fableModel, "live", "claude-a.json", testNow)

	tp.pollAndSchedule(context.Background())
	if got := tp.bindings.Len(); got != 1 {
		t.Errorf("bindings after a background poll = %d, want only the live one", got)
	}
}

// resetWindow is a session reading whose window closes at resetsAt, the shape a
// response-header merge gives a credential the poll never fetches.
func resetWindow(resetsAt time.Time) []model.Window {
	return []model.Window{{
		Kind: model.WindowSession, Utilization: 0.61,
		ResetsAt: resetsAt, Duration: model.SessionDuration,
	}}
}

// testPollInterval is the cadence the fixture polls at. The fixture's own
// readings reset 1.5h before testNow (session) and days after it (weekly), so a
// planted window is what decides these wakes.
const testPollInterval = 2 * time.Minute

// TestAResetAheadOfTheIntervalShortensTheNextWake covers the reason the feature
// exists: the seat's utilization drops to zero at the reset, and the page would
// otherwise carry the pre-reset reading for the rest of the interval.
func TestAResetAheadOfTheIntervalShortensTheNextWake(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.quota.MergeHeaders("claude-off.json", resetWindow(testNow.Add(30*time.Second)), testNow)

	wait := tp.pollAndSchedule(context.Background())
	if want := 30*time.Second + resetSettle; wait != want {
		t.Errorf("wait = %v, want %v", wait, want)
	}
	tp.mu.Lock()
	next := tp.nextPollAt
	tp.mu.Unlock()
	if want := testNow.Add(30*time.Second + resetSettle); !next.Equal(want) {
		t.Errorf("nextPollAt = %v, want %v: the status page counts down to the wake the loop takes", next, want)
	}
}

func TestAResetBeyondTheIntervalLeavesTheWakeAlone(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.quota.MergeHeaders("claude-off.json", resetWindow(testNow.Add(5*time.Minute)), testNow)

	if wait := tp.pollAndSchedule(context.Background()); wait != testPollInterval {
		t.Errorf("wait = %v, want the %v interval", wait, testPollInterval)
	}
	tp.mu.Lock()
	next := tp.nextPollAt
	tp.mu.Unlock()
	if want := testNow.Add(testPollInterval); !next.Equal(want) {
		t.Errorf("nextPollAt = %v, want %v", next, want)
	}
}

// TestSeatsResettingTogetherShareOneWake covers a pool whose windows close
// within the settle delay of one another: the settle batches them, so the
// resets cost one read between them rather than one each.
func TestSeatsResettingTogetherShareOneWake(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.quota.MergeHeaders("claude-off.json", resetWindow(testNow.Add(30*time.Second)), testNow)
	tp.quota.MergeHeaders("claude-key", resetWindow(testNow.Add(31*time.Second)), testNow)

	wait := tp.pollAndSchedule(context.Background())
	if want := 30*time.Second + resetSettle; wait != want {
		t.Fatalf("wait = %v, want the earlier reset's %v", wait, want)
	}
	// Both windows have closed by the time that wake lands, so neither seat
	// asks for a wake of its own afterwards.
	if got := nextPollWait(tp.quota.All(), testNow.Add(wait), testPollInterval); got != testPollInterval {
		t.Errorf("wake after the shared poll = %v, want the %v interval", got, testPollInterval)
	}
}

// TestAResetAtHandNeverWakesTheLoopAtOnce covers the tight loop: a wake of zero
// or less would spin the poll goroutine, and a reset already served must not
// shorten anything at all.
func TestAResetAtHandNeverWakesTheLoopAtOnce(t *testing.T) {
	justAhead := []model.AuthSnapshot{{AuthID: "a", Windows: resetWindow(testNow.Add(time.Nanosecond))}}
	if got := nextPollWait(justAhead, testNow, testPollInterval); got < resetSettle {
		t.Errorf("wake for a reset a nanosecond ahead = %v, want at least the %v floor", got, resetSettle)
	}
	for _, resetsAt := range []time.Time{testNow, testNow.Add(-time.Nanosecond), testNow.Add(-3 * time.Hour)} {
		snaps := []model.AuthSnapshot{{AuthID: "a", Windows: resetWindow(resetsAt)}}
		if got := nextPollWait(snaps, testNow, testPollInterval); got != testPollInterval {
			t.Errorf("wake for a reset at %v = %v, want the %v interval", resetsAt, got, testPollInterval)
		}
	}
}

// TestAResetKeptSecondsAheadDoesNotSpinTheLoop covers a provider that reports
// the same few seconds to a reset on every read: the wake floors well above
// the settle delay, and never past the interval.
func TestAResetKeptSecondsAheadDoesNotSpinTheLoop(t *testing.T) {
	snaps := []model.AuthSnapshot{{AuthID: "a", Windows: resetWindow(testNow.Add(time.Second))}}
	if got := nextPollWait(snaps, testNow, testPollInterval); got != resetWakeFloor {
		t.Errorf("wake for a reset a second ahead = %v, want the %v floor", got, resetWakeFloor)
	}
	if got := nextPollWait(snaps, testNow, 20*time.Second); got != 20*time.Second {
		t.Errorf("wake under a 20s interval = %v, want the interval", got)
	}
}

// TestTheWakeAfterAResetDrivenPollIsTheInterval walks two iterations of the
// loop: the instant that shortened the first wake is in the past by the second,
// so the loop returns to the interval instead of polling every settle delay.
func TestTheWakeAfterAResetDrivenPollIsTheInterval(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.quota.MergeHeaders("claude-off.json", resetWindow(testNow.Add(30*time.Second)), testNow)

	wait := tp.pollAndSchedule(context.Background())
	if want := 30*time.Second + resetSettle; wait != want {
		t.Fatalf("first wait = %v, want %v", wait, want)
	}

	woke := testNow.Add(wait)
	tp.now = func() time.Time { return woke }
	if second := tp.pollAndSchedule(context.Background()); second != testPollInterval {
		t.Errorf("wait after the reset-driven poll = %v, want the %v interval", second, testPollInterval)
	}
	tp.mu.Lock()
	next := tp.nextPollAt
	tp.mu.Unlock()
	if want := woke.Add(testPollInterval); !next.Equal(want) {
		t.Errorf("nextPollAt = %v, want %v", next, want)
	}
}

// TestAStoreWithNoFutureResetWakesAsBefore covers the fail-safe: a store that is
// empty, or that holds nothing resetting, schedules exactly what the poll asked
// for.
func TestAStoreWithNoFutureResetWakesAsBefore(t *testing.T) {
	if got := nextPollWait(nil, testNow, testPollInterval); got != testPollInterval {
		t.Errorf("wake on an empty store = %v, want the %v interval", got, testPollInterval)
	}
	noReset := []model.AuthSnapshot{{AuthID: "a", Windows: resetWindow(time.Time{})}}
	if got := nextPollWait(noReset, testNow, testPollInterval); got != testPollInterval {
		t.Errorf("wake on a window with no reset instant = %v, want the %v interval", got, testPollInterval)
	}

	// A poll that could not list credentials retries sooner than the interval,
	// and an empty store leaves that retry where it is.
	tp := newTestPlugin(t, testConfigYAML)
	if wait := tp.pollAndSchedule(context.Background()); wait != listRetryWait {
		t.Errorf("wait after a failed listing = %v, want the %v retry", wait, listRetryWait)
	}
	if len(tp.quota.All()) != 0 {
		t.Fatal("the store was not empty")
	}
}

// TestSeatNameComesFromTheNoteBeforeTheLabel covers the name a seat carries:
// the host fills a credential's label from its account email, so the note an
// operator writes on the auth-file card is the one name that is not an
// address. A blank note yields to the label, and a blank label to the email.
func TestSeatNameComesFromTheNoteBeforeTheLabel(t *testing.T) {
	cases := []struct {
		name  string
		entry HostAuthFileEntry
		want  string
	}{
		{"note wins", HostAuthFileEntry{Note: "personal", Label: "a@example.com", Email: "a@example.com", Name: "claude-a.json"}, "personal"},
		{"note is trimmed", HostAuthFileEntry{Note: "  personal  ", Label: "a@example.com"}, "personal"},
		{"blank note yields to label", HostAuthFileEntry{Note: "   ", Label: "Seat B", Email: "b@example.com"}, "Seat B"},
		{"no note or label yields to email", HostAuthFileEntry{Email: "c@example.com", Name: "claude-c.json"}, "c@example.com"},
		{"file name last", HostAuthFileEntry{Name: "claude-d.json"}, "claude-d.json"},
	}
	for _, tc := range cases {
		if got := authLabel(tc.entry); got != tc.want {
			t.Errorf("%s: authLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}
