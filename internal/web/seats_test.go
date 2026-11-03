package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// binderSource is a stubSource that also names a session's binding.
type binderSource struct {
	stubSource
	bindings map[string]model.Binding
	asked    string
}

// SessionBinding names the session's binding, expiring an hour after it was last
// seen.
func (s *binderSource) SessionBinding(sessionID, modelID string) (model.Binding, time.Time, bool) {
	s.asked = sessionID + " " + modelID
	b, ok := s.bindings[sessionID]
	if !ok {
		return b, time.Time{}, false
	}
	return b, b.LastSeen.Add(time.Hour), true
}

// raterSource is a stubSource that also gives seats their rates.
type raterSource struct {
	stubSource
	rates map[string]model.SpendRates
}

func (s *raterSource) SpendRates(time.Time) map[string]model.SpendRates { return s.rates }

// seatsStatus is a pool of three seats on two priority tiers: an eligible
// seat named by its account address, a cheaper eligible seat, and a spent one,
// plus a fallback seat on a lower tier that is cheapest of all.
func seatsStatus() model.Status {
	weekly := func(util float64) model.Window {
		return model.Window{Kind: model.WindowWeekly, Utilization: util, ResetsAt: base.Add(48 * time.Hour)}
	}
	seat := func(id, label string, priority int, cost float64, eligible bool, reason string, windows ...model.Window) model.AuthStatus {
		return model.AuthStatus{
			AuthID: id, Label: label, Email: label, Provider: "claude", Priority: priority, HostStatus: "active",
			Snapshot: model.AuthSnapshot{AuthID: id, Label: label, Windows: windows},
			Score: model.Score{AuthID: id, Cost: cost, Eligible: eligible, Reason: reason,
				Windows: []model.WindowScore{{Kind: model.WindowWeekly, Slack: 0.12}}},
		}
	}
	return model.Status{
		Now: base, PolledAt: base.Add(-time.Minute), Model: "claude-opus-5",
		Auths: []model.AuthStatus{
			seat("auth-a", "alice@example.com", 10, 0.4, true, "", weekly(0.61),
				model.Window{Kind: model.WindowSession, Utilization: math.NaN(), ResetsAt: base.Add(time.Hour)}),
			seat("auth-b", "Seat B", 10, 0.1, true, "", weekly(0.22)),
			seat("auth-c", "Seat C", 10, 0, false, model.ReasonSpent, weekly(1)),
			seat("auth-d", "Fallback", 1, -5, true, "", weekly(0.3)),
		},
		Warnings: []string{"quota poll failing for auth-c (timeout): boom"},
	}
}

func answerOfResponse(t *testing.T, rec *httptest.ResponseRecorder) SeatsAnswer {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var answer SeatsAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	return answer
}

func TestSeatsNamesTheSessionsSeatInThePagesForm(t *testing.T) {
	const session = "0b2e7c1a-4d36-4f58-9a7e-3c1f5d8e2b40"
	src := &binderSource{
		stubSource: stubSource{status: seatsStatus()},
		bindings:   map[string]model.Binding{session: {AuthID: "auth-a", Model: "claude-opus-5", LastSeen: base}},
	}
	// Claude Code names a 1M-context model with a suffix its requests drop;
	// the binding is looked up for the model the status was scored for.
	rec := get(t, testHandler(src), "/api/seats?session="+session+"&model=claude-fable-5%5B1m%5D")
	answer := answerOfResponse(t, rec)

	if answer.Version != LocalAPIVersion {
		t.Errorf("version = %d, want %d", answer.Version, LocalAPIVersion)
	}
	if src.asked != session+" claude-opus-5" || src.gotModl != "claude-fable-5" {
		t.Errorf("asked session %q for model %q", src.asked, src.gotModl)
	}
	if answer.Binding == nil || answer.Binding.AuthID != testID("auth-a") || answer.Binding.Model != "claude-opus-5" {
		t.Fatalf("binding = %+v, want auth-a's published id", answer.Binding)
	}
	if answer.Binding.ExpiresAt == nil || !answer.Binding.ExpiresAt.Equal(base.Add(time.Hour)) {
		t.Errorf("expires at = %v, want an hour after it was last seen", answer.Binding.ExpiresAt)
	}
	if answer.Next != testID("auth-b") {
		t.Errorf("next = %q, want the cheaper eligible seat of the top tier", answer.Next)
	}
	if len(answer.Seats) != 4 {
		t.Fatalf("seats = %d, want 4", len(answer.Seats))
	}
	a := answer.Seats[0]
	if a.AuthID != testID("auth-a") || strings.Contains(a.Label, "alice") || a.Slack == nil || *a.Slack != 0.12 {
		t.Errorf("seat a = %+v, want a published id, a masked name and its slack", a)
	}
	if c := answer.Seats[2]; c.Eligible || c.Reason != model.ReasonSpent {
		t.Errorf("seat c = %+v, want spent", c)
	}
	if strings.Contains(rec.Body.String(), "alice@") || strings.Contains(rec.Body.String(), `"auth-a"`) {
		t.Errorf("the body carries an address or a raw id: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"utilization":null`) {
		t.Errorf("a NaN reading is not null: %s", rec.Body)
	}
	if len(answer.Warnings) != 1 || strings.Contains(answer.Warnings[0], "auth-c") {
		t.Errorf("warnings = %v, want the page's published text", answer.Warnings)
	}
}

func TestSeatsWithoutABindingOrABinder(t *testing.T) {
	src := &binderSource{stubSource: stubSource{status: seatsStatus()}}
	if answer := answerOfResponse(t, get(t, testHandler(src), "/api/seats?session=unbound")); answer.Binding != nil {
		t.Errorf("binding = %+v for an unbound session", answer.Binding)
	}
	plain := &stubSource{status: seatsStatus()}
	if answer := answerOfResponse(t, get(t, testHandler(plain), "/api/seats?session=x")); answer.Binding != nil || len(answer.Seats) != 4 {
		t.Errorf("answer from a source with no binder = %+v", answer)
	}
}

func TestSeatsRefusesWritesAndOversizedSessions(t *testing.T) {
	h := testHandler(&stubSource{status: seatsStatus()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/seats", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	if rec := get(t, h, "/api/seats?session="+strings.Repeat("a", maxSessionIDLength+1)); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized session = %d, want 400", rec.Code)
	}
}

func TestSeatsNextSkipsSeatsTheHostHoldsBack(t *testing.T) {
	st := seatsStatus()
	// The only seat above the rest is one the host holds back, so the top tier
	// a conversation can reach is the one below it.
	for i := range st.Auths {
		st.Auths[i].Priority = 5
	}
	st.Auths[0].Priority = 10
	st.Auths[0].Score.Eligible = false
	st.Auths[0].Score.Reason = model.ReasonNotCandidate
	st.Auths[3].Priority = 1
	answer := answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), "/api/seats"))
	if answer.Next != testID("auth-b") {
		t.Fatalf("next = %q, want auth-b on the tier the host offers", answer.Next)
	}
}

func TestSeatsNextRanksACostThatIsNotANumberLast(t *testing.T) {
	st := seatsStatus()
	st.Auths[1].Score.Cost = math.NaN()
	answer := answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), "/api/seats"))
	if answer.Next != testID("auth-a") {
		t.Errorf("next = %q, want auth-a over a seat whose cost is not a number", answer.Next)
	}
	st.Auths[0].Score.Eligible = false
	answer = answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), "/api/seats"))
	if answer.Next != testID("auth-b") {
		t.Errorf("next = %q, want auth-b when it is the only eligible seat", answer.Next)
	}
}

func TestSeatsNextPutsASeatWithNoWeeklyCapFirst(t *testing.T) {
	st := seatsStatus()
	st.Auths[0].Score.SessionOnly = true
	answer := answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), "/api/seats"))
	if answer.Next != testID("auth-a") {
		t.Errorf("next = %q, want auth-a, the seat with no weekly cap, over the cheaper auth-b", answer.Next)
	}
}

func TestSeatsGivesEachWindowItsUseSinceAndEachSeatItsRate(t *testing.T) {
	now := time.Now()
	st := seatsStatus()
	rate, fiveRate := 0.004, 0.03
	st.Auths[0].History = []model.WindowHistory{{Kind: model.WindowWeekly, Cycles: []model.Cycle{{
		ResetsAt: now.Add(48 * time.Hour),
		Samples: []model.Sample{
			{At: now.Add(-30 * time.Hour), Utilization: 0.30},
			{At: now.Add(-20 * time.Hour), Utilization: math.NaN()},
			{At: now.Add(-2 * time.Hour), Utilization: 0.42},
		},
	}}}}
	h := testHandler(&raterSource{
		stubSource: stubSource{status: st},
		rates:      map[string]model.SpendRates{"auth-a": {Weekly: &rate, FiveHour: &fiveRate}},
	})
	since := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	answer := answerOfResponse(t, get(t, h, "/api/seats?since="+since))
	a := answer.Seats[0]
	if a.WeeklyPerUSD == nil || *a.WeeklyPerUSD != rate || a.FiveHourPerUSD == nil || *a.FiveHourPerUSD != fiveRate {
		t.Errorf("rates = %v, %v; want %v, %v", a.WeeklyPerUSD, a.FiveHourPerUSD, rate, fiveRate)
	}
	var week *AnswerWindow
	for i := range a.Windows {
		if a.Windows[i].Kind == model.WindowWeekly {
			week = &a.Windows[i]
		}
	}
	if week == nil || week.UsedSince == nil || math.Abs(*week.UsedSince-0.12) > 1e-9 {
		t.Fatalf("weekly window = %+v, want 0.12 used since", week)
	}
	if b := answer.Seats[1]; b.Windows[0].UsedSince != nil || b.WeeklyPerUSD != nil || b.FiveHourPerUSD != nil {
		t.Errorf("seat b = %+v, want no use since and no rate without a history or rate", b)
	}
	for _, bad := range []string{"yesterday", now.Add(time.Hour).UTC().Format(time.RFC3339), now.Add(-9 * 24 * time.Hour).UTC().Format(time.RFC3339)} {
		answer := answerOfResponse(t, get(t, h, "/api/seats?since="+bad))
		if answer.Seats[0].Windows[0].UsedSince != nil {
			t.Errorf("since %q gave a use since", bad)
		}
	}
}

func TestSeatsUsedSinceReadsTheFiveHourWindowAndAPlusOffset(t *testing.T) {
	now := time.Now()
	st := seatsStatus()
	st.Auths[1].Snapshot.Windows = append(st.Auths[1].Snapshot.Windows, model.Window{Kind: model.WindowSession, Utilization: 0.4, ResetsAt: now.Add(time.Hour)})
	// The 5-hour window opened four hours ago, after since, so it counts from
	// nothing; a weekly length would read it as open at since.
	st.Auths[1].History = []model.WindowHistory{{Kind: model.WindowSession, Cycles: []model.Cycle{{
		ResetsAt: now.Add(time.Hour),
		Samples:  []model.Sample{{At: now.Add(-3 * time.Hour), Utilization: 0.1}, {At: now.Add(-time.Minute), Utilization: 0.4}},
	}}}}
	zone := time.FixedZone("plus9", 9*3600)
	since := now.Add(-6 * time.Hour).In(zone).Format(time.RFC3339)
	answer := answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), "/api/seats?since="+since))
	five := answer.Seats[1].Windows[len(answer.Seats[1].Windows)-1]
	if five.Kind != model.WindowSession || five.UsedSince == nil || math.Abs(*five.UsedSince-0.4) > 1e-9 {
		t.Fatalf("5-hour window = %+v, want 0.4 used since %s", five, since)
	}
}

func TestSeatsGivesTheFiveHourWindowItsOwnSince(t *testing.T) {
	now := time.Now()
	st := seatsStatus()
	st.Auths[1].Snapshot.Windows = append(st.Auths[1].Snapshot.Windows, model.Window{Kind: model.WindowSession, Utilization: 0.4, ResetsAt: now.Add(time.Hour)})
	st.Auths[1].History = []model.WindowHistory{
		{Kind: model.WindowSession, Cycles: []model.Cycle{{ResetsAt: now.Add(time.Hour),
			Samples: []model.Sample{{At: now.Add(-3 * time.Hour), Utilization: 0.1}, {At: now.Add(-50 * time.Minute), Utilization: 0.3}, {At: now.Add(-time.Minute), Utilization: 0.4}}}}},
		{Kind: model.WindowWeekly, Cycles: []model.Cycle{{ResetsAt: now.Add(48 * time.Hour),
			Samples: []model.Sample{{At: now.Add(-30 * time.Hour), Utilization: 0.2}, {At: now.Add(-time.Minute), Utilization: 0.25}}}}},
	}
	q := "/api/seats?since=" + now.Add(-24*time.Hour).UTC().Format(time.RFC3339) + "&since_five_hour=" + now.Add(-time.Hour).UTC().Format(time.RFC3339)
	answer := answerOfResponse(t, get(t, testHandler(&stubSource{status: st}), q))
	for _, w := range answer.Seats[1].Windows {
		want := map[model.WindowKind]float64{model.WindowSession: 0.3, model.WindowWeekly: 0.05}[w.Kind]
		if w.UsedSince == nil || math.Abs(*w.UsedSince-want) > 1e-9 {
			t.Errorf("%s used since = %v, want %v", w.Kind, w.UsedSince, want)
		}
	}
}

func TestSeatsLeavesOutTimesItDoesNotKnow(t *testing.T) {
	st := seatsStatus()
	st.PolledAt = time.Time{}
	st.Auths[0].Snapshot.Windows[0].ResetsAt = time.Time{}
	body := get(t, testHandler(&stubSource{status: st}), "/api/seats").Body.String()
	if strings.Contains(body, `"polled_at"`) || strings.Contains(body, "0001-01-01") {
		t.Errorf("answer names an unknown time: %s", body)
	}
	if strings.Count(body, `"resets_at"`) != 4 {
		t.Errorf("answer = %s, want resets_at on every window but the one with none", body)
	}
}
