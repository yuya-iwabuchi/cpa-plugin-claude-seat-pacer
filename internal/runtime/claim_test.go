package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

const testOrg = "0f8fad5b-d9cb-469f-a165-70867728950e"

// claimFixture is pollFixture with an organization on claude-a.json and an
// endpoint that answers a claim with answer. It records every request the
// plugin sends upstream, in order.
type claimFixture struct {
	mu   sync.Mutex
	sent []HostHTTPRequest
}

func (c *claimFixture) requests() []HostHTTPRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

func newClaimFixture(t *testing.T, tp *testPlugin, status int, answer string) *claimFixture {
	t.Helper()
	pollFixture(t, tp)
	tp.host.auths["idx-a"] = json.RawMessage(`{"type":"claude","email":"a@example.com","access_token":"` + secretToken + `","refresh_token":"rt-a","organization_uuid":"` + testOrg + `"}`)
	usage := tp.host.http
	c := &claimFixture{}
	tp.host.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		c.mu.Lock()
		c.sent = append(c.sent, req)
		c.mu.Unlock()
		if req.Method != http.MethodPost {
			return usage(req)
		}
		if req.URL != "https://api.anthropic.com/api/organizations/"+testOrg+"/reset_rate_limits" || req.Headers.Get("Authorization") != "Bearer "+secretToken {
			return HostHTTPResponse{StatusCode: 404}, nil
		}
		return HostHTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(answer)}, nil
	}
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return c
}

func TestClaimResetSendsTheSeatsClaimAndReadsItAgain(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	c := newClaimFixture(t, tp, 200, `{"result":"reset","resets_left":0,"cleared":["five_hour","seven_day"]}`)
	before := len(c.requests())

	res, err := tp.ClaimReset(context.Background(), "claude-a.json", "full-reset-2611", "req-1")
	if err != nil {
		t.Fatalf("ClaimReset: %v", err)
	}
	if res.Result != model.ClaimReset || res.ResetsLeft == nil || *res.ResetsLeft != 0 || len(res.Cleared) != 2 {
		t.Errorf("claim = %+v, want the endpoint's answer", res)
	}
	sent := c.requests()[before:]
	if len(sent) != 2 || sent[0].Method != http.MethodPost || sent[1].Method == http.MethodPost {
		t.Fatalf("requests after the poll = %+v, want the claim then one usage read", sent)
	}
	var body map[string]string
	if err := json.Unmarshal(sent[0].Body, &body); err != nil || body["program"] != "cedar_ember" || body["grant_id"] != "full-reset-2611" || body["request_id"] != "req-1" {
		t.Errorf("claim body = %s (%v), want the program, grant and request id", sent[0].Body, err)
	}
	if sent[1].Headers.Get("Authorization") != "Bearer "+secretToken {
		t.Error("the re-read did not read claude-a.json")
	}
	if got := tp.host.methodsContaining(secretToken); !slices.Equal(got, []string{MethodHostHTTPDo}) {
		t.Errorf("the token travelled through %v, want host.http.do alone", got)
	}
	var logged *HostLogRequest
	for _, l := range tp.host.logs() {
		if strings.Contains(l.Message, "limit reset") {
			logged = &l
		}
	}
	if logged == nil || logged.Level != "info" || logged.Fields["result"] != "reset" || logged.Fields["grant_id"] != "full-reset-2611" || logged.Fields["auth_id"] != "claude-a.json" {
		t.Errorf("claim log = %+v, want an info line naming the seat, grant and result", logged)
	}
}

func TestClaimResetRefusesWhatItCannotSend(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	c := newClaimFixture(t, tp, 200, `{"result":"reset"}`)
	before := len(c.requests())

	for _, tc := range []struct {
		seat string
		want error
	}{
		{"claude-missing.json", ErrNoSuchSeat},
		{"claude-off.json", ErrNoSuchSeat},
		{"claude-key", ErrSeatUnreadable},
		{"claude-b.json", ErrNoOrganization},
	} {
		if _, err := tp.ClaimReset(context.Background(), tc.seat, "full-reset-2611", "req-1"); !errors.Is(err, tc.want) || !errors.Is(err, model.ErrClaimNotSent) {
			t.Errorf("%s: err = %v, want %v, a claim not sent", tc.seat, err, tc.want)
		}
	}
	if sent := c.requests()[before:]; len(sent) != 0 {
		t.Errorf("sent %d requests for claims that could not go out, want none", len(sent))
	}
}

func TestAFailedClaimIsLoggedWithoutTheToken(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	newClaimFixture(t, tp, 500, `{"error":"`+secretToken+`"}`)

	if _, err := tp.ClaimReset(context.Background(), "claude-a.json", "full-reset-2611", "req-1"); err == nil {
		t.Fatal("ClaimReset answered a 500 without an error")
	}
	warned := false
	for _, l := range tp.host.logs() {
		warned = warned || l.Level == "warn" && strings.Contains(l.Message, "could not use a limit reset")
	}
	if !warned {
		t.Error("no warning for the failed claim")
	}
	if got := tp.host.methodsContaining(secretToken); !slices.Equal(got, []string{MethodHostHTTPDo}) {
		t.Errorf("the token travelled through %v, want host.http.do alone", got)
	}
}

// A recheck of a seat the host still cools after its refusal cleared asks the
// host to end the cooldown, as a poll would, and changes no other seat.
func TestRecheckSeatReleasesAStaleCooldown(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	tp.mu.Lock()
	tp.auths = slices.Clone(tp.host.files)
	tp.mu.Unlock()

	read, err := tp.RecheckSeat(context.Background(), "claude-a.json")
	if err != nil || !read {
		t.Fatalf("RecheckSeat = %v, %v; want a read", read, err)
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 || !tp.host.sent(`{"auth_index":"idx-a"}`) {
		t.Fatalf("reset_cooldown called %d times, want once for idx-a", n)
	}
	for _, row := range tp.Status(testNow, "").Auths {
		switch row.AuthID {
		case "claude-a.json":
			if row.HostStatus == "unavailable" || row.HostHeldUntil != nil {
				t.Errorf("claude-a.json = status %q held until %v, want the cleared seat offered", row.HostStatus, row.HostHeldUntil)
			}
		case "claude-b.json":
			if _, ok := tp.quota.Get(row.AuthID); ok {
				t.Error("the recheck read claude-b.json too")
			}
		}
	}

	// A second recheck inside the gap reads nothing; one after it reads and
	// asks the host nothing more about a span it already cleared.
	if read, err := tp.RecheckSeat(context.Background(), "claude-a.json"); err != nil || read {
		t.Errorf("recheck inside %v = %v, %v; want no read", MinSeatRecheckGap, read, err)
	}
	later := testNow.Add(MinSeatRecheckGap)
	tp.now = func() time.Time { return later }
	if read, err := tp.RecheckSeat(context.Background(), "claude-a.json"); err != nil || !read {
		t.Errorf("recheck after the gap = %v, %v; want a read", read, err)
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Errorf("reset_cooldown called %d times, want once", n)
	}
	if _, err := tp.RecheckSeat(context.Background(), "claude-off.json"); !errors.Is(err, ErrNoSuchSeat) {
		t.Errorf("recheck of a disabled seat: err = %v, want %v", err, ErrNoSuchSeat)
	}
}

func TestWithEntryLeavesTheSliceItCopied(t *testing.T) {
	in := []HostAuthFileEntry{{ID: "a", Status: "unavailable", Unavailable: true}, {ID: "b", Status: "unavailable", Unavailable: true}}
	out := withEntry(in, "a", markCleared)
	if !in[0].Unavailable || in[0].Status != "unavailable" {
		t.Errorf("withEntry wrote the slice it was given: %+v", in[0])
	}
	if out[0].Unavailable || out[0].Status != "active" || !out[1].Unavailable {
		t.Errorf("withEntry = %+v, want only a cleared", out)
	}
}

// A recheck during a poll reads nothing: the poll reads the seat itself, and
// would otherwise replace what the recheck published with a listing it took
// before. A claim during a poll is still sent, without its re-read.
func TestAPollRunningTakesThePlaceOfARecheck(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	c := newClaimFixture(t, tp, 200, `{"result":"reset"}`)
	tp.now = func() time.Time { return testNow.Add(time.Minute) }
	before := len(c.requests())

	tp.pollMu.Lock()
	read, err := tp.RecheckSeat(context.Background(), "claude-a.json")
	if err != nil || read {
		t.Errorf("recheck during a poll = %v, %v; want no read", read, err)
	}
	if _, err := tp.ClaimReset(context.Background(), "claude-a.json", "full-reset-2611", "req-1"); err != nil {
		t.Errorf("claim during a poll: %v", err)
	}
	tp.pollMu.Unlock()
	if sent := c.requests()[before:]; len(sent) != 1 || sent[0].Method != http.MethodPost {
		t.Errorf("requests = %+v, want the claim alone", sent)
	}
	if read, err := tp.RecheckSeat(context.Background(), "claude-a.json"); err != nil || !read {
		t.Errorf("recheck once the poll ended = %v, %v; want a read", read, err)
	}
}

func TestASecondClaimDoesNotWait(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	newClaimFixture(t, tp, 200, `{"result":"reset"}`)
	tp.claimMu.Lock()
	defer tp.claimMu.Unlock()
	// The claim running may be this one sent again, so busy is not "not sent".
	if _, err := tp.ClaimReset(context.Background(), "claude-a.json", "full-reset-2611", "req-2"); !errors.Is(err, ErrClaimBusy) || errors.Is(err, model.ErrClaimNotSent) {
		t.Errorf("err = %v, want %v, a claim that may have been sent", err, ErrClaimBusy)
	}
}

// A recheck whose read fails reports it, settles no cooldown on the older
// reading, and still counts toward the gap, so a failing endpoint is not
// asked again at once.
func TestAFailedRecheckSettlesNothing(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	// A good poll while the host offers the seat leaves a reading a stale
	// cooldown would be settled on; the host then cools the seat.
	cooled, until := tp.host.files[0].Unavailable, tp.host.files[0].NextRetryAfter
	tp.host.files[0].Unavailable, tp.host.files[0].NextRetryAfter = false, time.Time{}
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	tp.host.files[0].Unavailable, tp.host.files[0].NextRetryAfter = cooled, until
	tp.now = func() time.Time { return testNow.Add(time.Minute) }
	reads := 0
	tp.host.http = func(HostHTTPRequest) (HostHTTPResponse, error) {
		reads++
		return HostHTTPResponse{StatusCode: 500}, nil
	}

	if read, err := tp.RecheckSeat(context.Background(), "claude-a.json"); err == nil || read {
		t.Errorf("failed recheck = %v, %v; want the read's error", read, err)
	}
	if read, err := tp.RecheckSeat(context.Background(), "claude-a.json"); err != nil || read {
		t.Errorf("recheck inside the gap = %v, %v; want no read", read, err)
	}
	if reads != 1 {
		t.Errorf("usage reads = %d, want 1", reads)
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 0 {
		t.Errorf("reset_cooldown called %d times after a failed read, want none", n)
	}
}

// A sync asked for while a seat recheck holds the poll lock waits for it and
// reads every seat, where a running poll would have answered for it.
func TestASyncWaitsOutARecheck(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if !tp.tryRecheckLock() {
		t.Fatal("the poll lock is taken")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		tp.recheckUnlock()
	}()
	if !tp.SyncNow(context.Background()) {
		t.Error("SyncNow behind a recheck read nothing")
	}
	if _, ok := tp.quota.Get("claude-b.json"); !ok {
		t.Error("the sync did not read claude-b.json")
	}
}
