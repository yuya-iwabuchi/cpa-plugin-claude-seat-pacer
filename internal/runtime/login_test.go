package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

func loginAt(expires, refreshed time.Time) loginFile {
	return loginFile{Expired: expires.Format(time.RFC3339), LastRefresh: refreshed.Format(time.RFC3339), OrganizationName: " Team 5 "}
}

// withDevice is f holding the device ids given.
func withDevice(f loginFile, ids string) loginFile {
	f.DeviceIDs = json.RawMessage(ids)
	return f
}

const (
	deviceA = `["` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"]`
	deviceB = `["` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `"]`
)

// reading is one look at a credential file, at the plugin's instant, with
// the host's refusal of its refresh token.
type reading struct {
	at time.Time
	f  loginFile
	r  refusal
}

// observe runs the readings through observeLogin in order and returns the
// last track.
func observe(t *testing.T, readings ...reading) loginTrack {
	t.Helper()
	var prev *loginTrack
	var track loginTrack
	for _, r := range readings {
		next, _, ok := observeLogin(prev, r.f, r.r, r.at)
		if !ok {
			t.Fatalf("reading at %v not parsed", r.at)
		}
		track = next
		prev = &track
	}
	return track
}

func TestLoginStates(t *testing.T) {
	now := testNow
	refreshed := now.Add(-2 * time.Hour)
	healthy := loginAt(now.Add(6*time.Hour), refreshed)
	if l := observe(t, reading{at: now, f: healthy}).login(now, refusalNone); l.State != model.LoginOK || l.Team != "Team 5" || l.LoggedInAt != nil {
		t.Errorf("healthy = %+v", l)
	}

	// Inside the refresh lead, a token is failing only once it stays there
	// for loginConfirm on the plugin's clock.
	failing := loginAt(now.Add(3*time.Hour), now.Add(-5*time.Hour))
	track := observe(t, reading{at: now, f: failing})
	if l := track.login(now, refusalNone); l.State != model.LoginOK {
		t.Errorf("first look inside the lead = %q, want ok", l.State)
	}
	track = observe(t, reading{at: now, f: failing}, reading{at: now.Add(loginConfirm), f: failing})
	if l := track.login(now.Add(loginConfirm), refusalNone); l.State != model.LoginFailing {
		t.Errorf("confirmed inside the lead = %q, want failing", l.State)
	}

	// Past its end the token is expired once confirmed, or at once when the
	// host lists the refresh token refused.
	dead := loginAt(now.Add(-time.Hour), now.Add(-9*time.Hour))
	if l := observe(t, reading{at: now, f: dead}).login(now, refusalNone); l.State != model.LoginOK {
		t.Errorf("unconfirmed dead token = %q, want ok", l.State)
	}
	if l := observe(t, reading{at: now, f: dead}).login(now, refusalGrant); l.State != model.LoginExpired {
		t.Errorf("refused dead token = %q, want expired", l.State)
	}
	if l := observe(t, reading{at: now, f: dead}, reading{at: now.Add(loginConfirm), f: dead}).login(now.Add(loginConfirm), refusalNone); l.State != model.LoginExpired {
		t.Errorf("confirmed dead token = %q, want expired", l.State)
	}
}

func TestLoginSeenMade(t *testing.T) {
	now := testNow
	old := loginAt(now.Add(6*time.Hour), now.Add(-2*time.Hour))

	// A routine renewal, about LoginRefreshLead before the end, is no login.
	renewAt := now.Add(2 * time.Hour)
	renewed := loginAt(renewAt.Add(8*time.Hour), renewAt)
	if track := observe(t, reading{at: now, f: old}, reading{at: renewAt, f: renewed}); !track.loggedInAt.IsZero() {
		t.Errorf("routine renewal recorded a login at %v", track.loggedInAt)
	}

	// A token replaced well before the lead was replaced by a login.
	loginAtT := now.Add(10 * time.Minute)
	fresh := loginAt(loginAtT.Add(8*time.Hour), loginAtT)
	track := observe(t, reading{at: now, f: old}, reading{at: loginAtT.Add(time.Minute), f: fresh})
	if !track.loggedInAt.Equal(loginAtT) {
		t.Fatalf("early replacement: loggedInAt = %v, want %v", track.loggedInAt, loginAtT)
	}
	l := track.login(loginAtT.Add(time.Minute), refusalNone)
	if l.EndsAround == nil || !l.EndsAround.Equal(loginAtT.Add(model.LoginLifetime)) {
		t.Errorf("EndsAround = %v", l.EndsAround)
	}
	if l := track.login(loginAtT.Add(model.LoginLifetime-model.LoginAgeingLead+time.Hour), refusalNone); l.State != model.LoginAgeing {
		t.Errorf("near its end = %q, want ageing", l.State)
	}
	if l := track.login(loginAtT.Add(model.LoginLifetime+time.Hour), refusalNone); l.State != model.LoginOK || l.EndsAround != nil {
		t.Errorf("past its end and renewing = %q ends %v, want ok with no end", l.State, l.EndsAround)
	}

	// A token replaced after its renewals had been failing was replaced by a
	// login, however late in the lead.
	failing := loginAt(now.Add(3*time.Hour), now.Add(-5*time.Hour))
	backAt := now.Add(loginConfirm + time.Minute)
	back := loginAt(backAt.Add(8*time.Hour), backAt)
	track = observe(t, reading{at: now, f: failing}, reading{at: now.Add(loginConfirm), f: failing}, reading{at: backAt, f: back})
	if !track.loggedInAt.Equal(backAt) {
		t.Errorf("after failing: loggedInAt = %v, want %v", track.loggedInAt, backAt)
	}

	// A file whose last renewal moves back was rewritten, not logged in.
	older := loginAt(now.Add(6*time.Hour), now.Add(-5*time.Hour))
	if track := observe(t, reading{at: now, f: old}, reading{at: now.Add(time.Minute), f: older}); !track.loggedInAt.IsZero() {
		t.Errorf("a renewal moving back recorded a login at %v", track.loggedInAt)
	}

	// A token renewed late because the machine slept was never confirmed
	// failing, so its renewal is no login.
	slept := loginAt(now.Add(-time.Hour), now.Add(-9*time.Hour))
	wokeAt := now.Add(time.Minute)
	woke := loginAt(wokeAt.Add(8*time.Hour), wokeAt)
	if track := observe(t, reading{at: now, f: slept}, reading{at: wokeAt, f: woke}); !track.loggedInAt.IsZero() {
		t.Errorf("renewal after sleep recorded a login at %v", track.loggedInAt)
	}
}

func TestLoginByDeviceID(t *testing.T) {
	now := testNow
	old := withDevice(loginAt(now.Add(6*time.Hour), now.Add(-2*time.Hour)), deviceA)

	// A renewal the host forces early keeps the device id, so it is no login.
	forcedAt := now.Add(10 * time.Minute)
	forced := withDevice(loginAt(forcedAt.Add(8*time.Hour), forcedAt), deviceA)
	if track := observe(t, reading{at: now, f: old}, reading{at: forcedAt, f: forced}); !track.loggedInAt.IsZero() {
		t.Errorf("forced renewal recorded a login at %v", track.loggedInAt)
	}

	// Nor is the first renewal after an outage held renewals back.
	stuck := withDevice(loginAt(now.Add(time.Hour), now.Add(-7*time.Hour)), deviceA)
	backAt := now.Add(loginConfirm + time.Minute)
	back := withDevice(loginAt(backAt.Add(8*time.Hour), backAt), deviceA)
	if track := observe(t, reading{at: now, f: stuck}, reading{at: now.Add(loginConfirm), f: stuck}, reading{at: backAt, f: back}); !track.loggedInAt.IsZero() {
		t.Errorf("renewal after an outage recorded a login at %v", track.loggedInAt)
	}

	// A login at the time of a routine renewal carries a new device id.
	renewAt := now.Add(2 * time.Hour)
	login := withDevice(loginAt(renewAt.Add(8*time.Hour), renewAt), deviceB)
	track := observe(t, reading{at: now, f: old}, reading{at: renewAt, f: login})
	if !track.loggedInAt.Equal(renewAt) {
		t.Errorf("login at a routine renewal: loggedInAt = %v, want %v", track.loggedInAt, renewAt)
	}

	// A new device id under an older renewal is a login of unknown date.
	rewound := withDevice(loginAt(now.Add(5*time.Hour), now.Add(-3*time.Hour)), deviceA)
	if track := observe(t, reading{at: renewAt, f: login}, reading{at: renewAt.Add(time.Minute), f: rewound}); !track.loggedInAt.IsZero() {
		t.Errorf("new device id under an older renewal: loggedInAt = %v, want none", track.loggedInAt)
	}

	// The host reads an id trimmed and lowercased, skipping invalid entries,
	// and a file the host gives its first id falls back on timing.
	same := withDevice(loginAt(renewAt.Add(8*time.Hour), renewAt), `[7, "nope", " `+strings.ToUpper(deviceA[2:66])+` "]`)
	if track := observe(t, reading{at: now, f: old}, reading{at: renewAt, f: same}); !track.loggedInAt.IsZero() {
		t.Errorf("the same id written differently recorded a login at %v", track.loggedInAt)
	}
	bare := loginAt(now.Add(6*time.Hour), now.Add(-2*time.Hour))
	if track := observe(t, reading{at: now, f: bare}, reading{at: renewAt, f: login}); !track.loggedInAt.IsZero() {
		t.Errorf("a first device id at a routine renewal recorded a login at %v", track.loggedInAt)
	}
	if deviceDigest(json.RawMessage(deviceA)) == "" || deviceDigest(json.RawMessage(`["short"]`)) != "" || deviceDigest(nil) != "" {
		t.Error("deviceDigest misreads a valid or an invalid id")
	}
}

func TestLoginKeptAfterRefusal(t *testing.T) {
	now := testNow
	dead := withDevice(loginAt(now.Add(-time.Hour), now.Add(-9*time.Hour)), deviceA)
	first, kept, _ := observeLogin(nil, dead, refusalGrant, now)
	if kept {
		t.Error("a first reading was kept")
	}
	// The host's status clears with the renewal it reported refused.
	backAt := now.Add(time.Hour)
	back := withDevice(loginAt(backAt.Add(8*time.Hour), backAt), deviceA)
	listed, _, _ := observeLogin(&first, dead, refusalNone, now.Add(time.Minute))
	next, kept, _ := observeLogin(&listed, back, refusalNone, backAt)
	if !kept || !next.loggedInAt.IsZero() {
		t.Errorf("renewal of a refused token under the same id: kept %v, loggedInAt %v", kept, next.loggedInAt)
	}
	if _, kept, _ := observeLogin(&next, withDevice(loginAt(backAt.Add(12*time.Hour), backAt.Add(4*time.Hour)), deviceA), refusalNone, backAt.Add(4*time.Hour)); kept {
		t.Error("the next routine renewal was kept again")
	}
	if _, kept, _ := observeLogin(&first, withDevice(back, deviceB), refusalNone, backAt); kept {
		t.Error("a login under a new id was kept")
	}

	// The host keeps an invalid-grant status through the file a login
	// writes, and its listing, read before the file, can still name a
	// revoked token: neither belongs to the new login.
	login, _, _ := observeLogin(&first, withDevice(back, deviceB), refusalGrant, backAt)
	renewAt := backAt.Add(4 * time.Hour)
	if _, kept, _ := observeLogin(&login, withDevice(loginAt(renewAt.Add(8*time.Hour), renewAt), deviceB), refusalGrant, renewAt); kept {
		t.Error("the new login's first renewal was kept under a stale invalid-grant status")
	}
	revoked, _, _ := observeLogin(&first, withDevice(back, deviceB), refusalRevoked, backAt)
	entries := []HostAuthFileEntry{{ID: "a", StatusMessage: "unauthorized (refresh token invalid)"}}
	if l := seatLogins(entries, map[string]loginTrack{"a": revoked}, nil, backAt)["a"]; l.State != model.LoginOK {
		t.Errorf("a new login under a listing read before it = %q, want ok", l.State)
	}
	if _, kept, _ := observeLogin(&revoked, withDevice(loginAt(renewAt.Add(8*time.Hour), renewAt), deviceB), refusalNone, renewAt); kept {
		t.Error("the new login's first renewal was kept under the revoked listing it replaced")
	}
}

func TestSeatLoginsStallOnlyWhileCutOff(t *testing.T) {
	now := testNow
	entries := []HostAuthFileEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	failing := observe(t, reading{at: now.Add(-loginConfirm), f: loginAt(now.Add(3*time.Hour), now.Add(-5*time.Hour))},
		reading{at: now, f: loginAt(now.Add(3*time.Hour), now.Add(-5*time.Hour))})
	dead := observe(t, reading{at: now.Add(-loginConfirm), f: loginAt(now.Add(-time.Hour), now.Add(-9*time.Hour))},
		reading{at: now, f: loginAt(now.Add(-time.Hour), now.Add(-9*time.Hour))})
	healthy := observe(t, reading{at: now, f: loginAt(now.Add(6*time.Hour), now.Add(-2*time.Hour))})
	tracks := map[string]loginTrack{"a": failing, "b": dead, "c": healthy}
	transport := pollState{category: string(quota.CategoryTransport)}
	cut := map[string]pollState{"a": transport, "b": {category: string(quota.CategoryTimeout)}, "c": transport}

	// An outage reaches seats at different points of their tokens' lives.
	got := seatLogins(entries, tracks, cut, now)
	if got["a"].State != model.LoginStalled || got["b"].State != model.LoginStalled || got["c"].State != model.LoginOK {
		t.Errorf("cut off: a=%s b=%s c=%s, want stalled, stalled, ok", got["a"].State, got["b"].State, got["c"].State)
	}

	// A refusal from the host is no outage.
	refused := append([]HostAuthFileEntry(nil), entries...)
	refused[1].StatusMessage = "invalid grant (retrying)"
	if got := seatLogins(refused, tracks, cut, now); got["b"].State != model.LoginExpired {
		t.Errorf("refused while cut off: b=%s, want expired", got["b"].State)
	}

	// One usage read reaching Anthropic shows the host is not cut off.
	reached := map[string]pollState{"a": transport, "b": transport, "c": {}}
	if got := seatLogins(entries, tracks, reached, now); got["a"].State != model.LoginFailing || got["b"].State != model.LoginExpired {
		t.Errorf("one read reached Anthropic: a=%s b=%s, want failing, expired", got["a"].State, got["b"].State)
	}

	// A disabled seat has no login.
	off := append([]HostAuthFileEntry(nil), entries...)
	off[2].Disabled = true
	if _, ok := seatLogins(off, tracks, cut, now)["c"]; ok {
		t.Error("a disabled seat has a login")
	}
}

func TestLoginRevokedAndUnread(t *testing.T) {
	now := testNow
	healthy := observe(t, reading{at: now, f: loginAt(now.Add(6*time.Hour), now.Add(-2*time.Hour))})
	if l := healthy.login(now, refusalRevoked); l.State != model.LoginExpired {
		t.Errorf("revoked with time left = %q, want expired", l.State)
	}
	if l := healthy.login(now, refusalGrant); l.State != model.LoginOK {
		t.Errorf("refused refresh with time left = %q, want ok", l.State)
	}
	if got := hostRefusal(HostAuthFileEntry{StatusMessage: "unauthorized (refresh token invalid)"}); got != refusalRevoked {
		t.Errorf("hostRefusal = %v, want revoked", got)
	}

	// A seat not read again stays as last read, however late the clock.
	first := observe(t, reading{at: now, f: loginAt(now.Add(3*time.Hour), now.Add(-5*time.Hour))})
	if l := first.login(now.Add(time.Hour), refusalNone); l.State != model.LoginOK {
		t.Errorf("one reading inside the lead, an hour on = %q, want ok", l.State)
	}
}

// loginJSON is a Claude credential file as host.auth.get returns it, holding
// the device ids given.
func loginJSON(expires, refreshed time.Time, devices string) json.RawMessage {
	return json.RawMessage(`{"type":"claude","access_token":"` + secretToken + `","refresh_token":"rt-a","expired":"` +
		expires.Format(time.RFC3339) + `","last_refresh":"` + refreshed.Format(time.RFC3339) + `","organization_name":"Team A","claude_device_ids":` + devices + `}`)
}

// loginLines is every host.log message about logins, in order.
func loginLines(tp *testPlugin) []string {
	var out []string
	for _, l := range tp.host.logs() {
		if strings.Contains(l.Message, "login") || strings.Contains(l.Message, "logged in") {
			out = append(out, l.Level+" "+l.Message)
		}
	}
	return out
}

func TestPollReportsLoginChangesAndKeepsTheLoginTime(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	now := testNow
	tp.now = func() time.Time { return now }
	poll := func() {
		t.Helper()
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	tp.host.auths["idx-a"] = loginJSON(now.Add(6*time.Hour), now.Add(-2*time.Hour), deviceA)
	poll()
	if lines := loginLines(tp); len(lines) != 0 {
		t.Fatalf("a seat renewing normally was logged: %v", lines)
	}

	// Renewals stop: the file stands while the token enters the refresh lead
	// and stays there past loginConfirm.
	now = now.Add(3 * time.Hour)
	poll()
	now = now.Add(loginConfirm)
	poll()
	lines := loginLines(tp)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "warn ") || !strings.Contains(lines[0], "renewal is failing") {
		t.Fatalf("after failing renewals: %v", lines)
	}

	// Someone logs in again: one line says so, and none repeats the state.
	loggedIn := now.Add(time.Minute)
	now = loggedIn.Add(time.Minute)
	tp.host.auths["idx-a"] = loginJSON(loggedIn.Add(8*time.Hour), loggedIn, deviceB)
	poll()
	lines = loginLines(tp)
	if len(lines) != 2 || !strings.Contains(lines[1], "logged in again") {
		t.Fatalf("after a new login: %v", lines)
	}
	for _, l := range tp.host.logs() {
		if raw, _ := json.Marshal(l); strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "rt-a") || strings.Contains(string(raw), "bbbb") {
			t.Errorf("credential material reached host.log: %s", raw)
		}
	}

	// The history file keeps the login time for the next run.
	body, err := os.ReadFile(tp.opts.HistoryFile)
	if err != nil || !strings.Contains(string(body), `"logins"`) || !strings.Contains(string(body), `"login_devices"`) || strings.Contains(string(body), "bbbb") {
		t.Fatalf("history file = %s, %v", body, err)
	}
	restart := func(devices string) *model.Login {
		t.Helper()
		again := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML)
		again.awaitStartupLoad(t)
		again.host.auths["idx-a"] = loginJSON(loggedIn.Add(8*time.Hour), loggedIn, devices)
		again.now = func() time.Time { return now }
		if err := again.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if lines := loginLines(again); len(lines) != 0 {
			t.Errorf("a restart logged %v", lines)
		}
		for _, r := range again.Status(now, "").Auths {
			if r.AuthID == "claude-a.json" {
				return r.Login
			}
		}
		return nil
	}
	if l := restart(deviceB); l == nil || l.LoggedInAt == nil || !l.LoggedInAt.Equal(loggedIn) {
		t.Errorf("login after a restart = %+v, want logged in at %v", l, loggedIn)
	}
	// A login made while the plugin was not running left a new device id.
	if l := restart(deviceA); l == nil || l.LoggedInAt != nil {
		t.Errorf("login after a restart under a new device id = %+v, want no date", l)
	}
}

func TestPollWarnsOfARefusedTokenRenewedUnderTheSameDevice(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	now := testNow
	tp.now = func() time.Time { return now }
	tp.host.files[0].StatusMessage = "invalid grant (retrying)"
	tp.host.auths["idx-a"] = loginJSON(now.Add(-time.Hour), now.Add(-9*time.Hour), deviceA)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	tp.host.files[0].StatusMessage = ""
	tp.host.auths["idx-a"] = loginJSON(now.Add(8*time.Hour), now, deviceA)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, l := range tp.host.logs() {
		if l.Level == "warn" && strings.Contains(l.Message, "keeping its device id") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning of a refused token renewed under the same device id: %v", loginLines(tp))
	}
}

func TestPollStaysQuietWhenALoginsDateRunsOut(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	now := testNow
	tp.now = func() time.Time { return now }
	poll := func(devices string) []string {
		t.Helper()
		tp.host.auths["idx-a"] = loginJSON(now.Add(6*time.Hour), now.Add(-2*time.Hour), devices)
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		return loginLines(tp)
	}
	poll(deviceA)
	loggedIn := now.Add(2 * time.Hour)
	now = loggedIn.Add(2 * time.Hour)
	if lines := poll(deviceB); len(lines) != 1 || !strings.Contains(lines[0], "logged in again") {
		t.Fatalf("after a login: %v", lines)
	}
	now = loggedIn.Add(model.LoginLifetime - model.LoginAgeingLead + time.Hour)
	if lines := poll(deviceB); len(lines) != 2 || !strings.Contains(lines[1], "within days") {
		t.Fatalf("near the login's end: %v", lines)
	}
	// Past its expected end the login drops its date and reads ok again,
	// though it never stopped renewing.
	now = loggedIn.Add(model.LoginLifetime + time.Hour)
	if lines := poll(deviceB); len(lines) != 2 {
		t.Errorf("a login past its expected end logged %v", lines[2:])
	}
}
