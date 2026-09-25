package quota

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// unified builds a response's unified rate-limit headers from name suffixes
// and values: unified("7d-status", "rejected") sets
// anthropic-ratelimit-unified-7d-status.
func unified(kv ...string) map[string][]string {
	h := make(map[string][]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		h[headerPrefix+kv[i]] = []string{kv[i+1]}
	}
	return h
}

// weeklyRefusal is a 429's headers on a spent 7-day window.
func weeklyRefusal(reset time.Time) map[string][]string {
	return unified(
		"5h-utilization", "0.4", "5h-reset", epoch(21, 0), "5h-status", "allowed",
		"7d-utilization", "1.0", "7d-reset", unix(reset), "7d-status", "rejected",
		"representative-claim", "seven_day", "status", "rejected", "reset", unix(reset),
	)
}

// sessionRefusal is a 429's headers on a spent 5-hour window.
func sessionRefusal(reset time.Time) map[string][]string {
	return unified(
		"5h-utilization", "1.0", "5h-reset", unix(reset), "5h-status", "rejected",
		"7d-utilization", "0.5", "7d-reset", epochDay(10, 14), "7d-status", "allowed",
		"representative-claim", "five_hour", "status", "rejected", "reset", unix(reset),
	)
}

// fableRefusal is a 429's headers on a spent Fable cap.
func fableRefusal() map[string][]string {
	return unified(
		"5h-utilization", "0.3", "5h-reset", epoch(21, 0), "5h-status", "allowed",
		"7d-utilization", "0.6", "7d-reset", epochDay(10, 14), "7d-status", "allowed",
		"7d_oi-utilization", "1.0", "7d_oi-reset", epochDay(10, 14), "7d_oi-status", "rejected",
		"representative-claim", "seven_day_overage_included", "status", "rejected", "reset", epochDay(10, 14),
	)
}

// refuse folds a refused response's headers into the store the way a usage
// record does: the reading merges, then its rejected windows record refusals.
func refuse(s *Store, id string, h map[string][]string, admitted, at time.Time) {
	ws := ParseResponseHeaders(h, at)
	s.MergeHeaders(id, ws, at)
	s.RecordRefusals(id, ws, admitted, at)
}

func unix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// span is an ongoing refusal span as a history publishes it.
func span(from, to time.Time) model.Lock {
	return model.Lock{From: from.Unix(), To: to.Unix()}
}

// ended is a refusal span as a history publishes it once end has ended it.
func ended(from, to time.Time, end model.LockEnd) model.Lock {
	return model.Lock{From: from.Unix(), To: to.Unix(), End: end}
}

// locksOf collects the refusal spans a credential's history publishes, keyed
// by window, leaving out windows with none.
func locksOf(s *Store, id string) map[windowKey][]model.Lock {
	out := map[windowKey][]model.Lock{}
	for _, h := range s.History(id, 0, testNow) {
		if len(h.Locks) > 0 {
			out[windowKey{kind: h.Kind, scope: h.Scope}] = h.Locks
		}
	}
	return out
}

func TestRefusalSpans(t *testing.T) {
	var (
		t0      = testNow
		weekly  = windowKey{kind: model.WindowWeekly}
		session = windowKey{kind: model.WindowSession}
		fable   = windowKey{kind: model.WindowWeeklyScoped, scope: model.FamilyFable}
		opus    = windowKey{kind: model.WindowWeeklyScoped, scope: model.FamilyOpus}
		r7      = day(10, 14)
		r5      = at(21, 0)
	)
	refusedAdmitted := func(admitted, when time.Time, h map[string][]string) func(*Store) {
		return func(s *Store) { refuse(s, "auth-1", h, admitted, when) }
	}
	refused := func(when time.Time, h map[string][]string) func(*Store) {
		return refusedAdmitted(when, when, h)
	}
	servedAdmitted := func(family string, admitted, when time.Time) func(*Store) {
		return func(s *Store) { s.MarkServed("auth-1", family, admitted, when) }
	}
	served := func(family string, when time.Time) func(*Store) {
		return servedAdmitted(family, when, when)
	}
	read := func(when time.Time, h map[string][]string) func(*Store) {
		return func(s *Store) { s.MergeHeaders("auth-1", ParseResponseHeaders(h, when), when) }
	}
	polled := func(when time.Time, ws ...model.Window) func(*Store) {
		return func(s *Store) { s.Put(endpointSnapshot("auth-1", when, ws...)) }
	}

	cases := []struct {
		name  string
		steps []func(*Store)
		want  map[windowKey][]model.Lock
	}{
		{
			name:  "a 429 opens a span on the rejected window until its reset",
			steps: []func(*Store){refused(t0, weeklyRefusal(r7))},
			want:  map[windowKey][]model.Lock{weekly: {span(t0, r7)}},
		},
		{
			name: "a later refusal extends the span to its reset",
			steps: []func(*Store){
				refused(t0, weeklyRefusal(r7)),
				refused(t0.Add(10*time.Minute), weeklyRefusal(r7.Add(time.Minute))),
			},
			want: map[windowKey][]model.Lock{weekly: {span(t0, r7.Add(time.Minute))}},
		},
		{
			name: "a refusal after a span ended opens another",
			steps: []func(*Store){
				refused(t0, weeklyRefusal(r7)),
				served(model.FamilySonnet, t0.Add(5*time.Minute)),
				refused(t0.Add(10*time.Minute), weeklyRefusal(r7)),
			},
			want: map[windowKey][]model.Lock{weekly: {ended(t0, t0.Add(5*time.Minute), model.LockEndServed), span(t0.Add(10*time.Minute), r7)}},
		},
		{
			name: "a served request on any model ends a 5-hour span",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				served(model.FamilySonnet, t0.Add(20*time.Minute)),
			},
			want: map[windowKey][]model.Lock{session: {ended(t0, t0.Add(20*time.Minute), model.LockEndServed)}},
		},
		{
			name: "a served Sonnet request leaves a Fable cap span running",
			steps: []func(*Store){
				refused(t0, fableRefusal()),
				served(model.FamilySonnet, t0.Add(5*time.Minute)),
			},
			want: map[windowKey][]model.Lock{fable: {span(t0, r7)}},
		},
		{
			name: "a served Fable request ends a Fable cap span",
			steps: []func(*Store){
				refused(t0, fableRefusal()),
				served(model.FamilySonnet, t0.Add(5*time.Minute)),
				served(model.FamilyFable, t0.Add(9*time.Minute)),
			},
			want: map[windowKey][]model.Lock{fable: {ended(t0, t0.Add(9*time.Minute), model.LockEndServed)}},
		},
		{
			name: "a span ends at the reset, and nothing after the reset moves it",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				refused(r5.Add(10*time.Minute), unified("5h-utilization", "0.01", "5h-reset", unix(r5.Add(model.SessionDuration)))),
				served(model.FamilySonnet, r5.Add(10*time.Minute)),
				refused(r5.Add(30*time.Minute), sessionRefusal(r5.Add(model.SessionDuration))),
			},
			want: map[windowKey][]model.Lock{session: {ended(t0, r5, model.LockEndReset), span(r5.Add(30*time.Minute), r5.Add(model.SessionDuration))}},
		},
		{
			name: "an early clearing ends the span at its first fallen reading",
			steps: []func(*Store){
				refused(t0, weeklyRefusal(r7)),
				polled(t0.Add(30*time.Minute), weeklyWindow(0.02)),
				polled(t0.Add(37*time.Minute), weeklyWindow(0.03)),
			},
			want: map[windowKey][]model.Lock{weekly: {ended(t0, t0.Add(30*time.Minute), model.LockEndCleared)}},
		},
		{
			name: "an early clearing confirmed lower still ends the span at its first fallen reading",
			steps: []func(*Store){
				polled(t0.Add(-time.Hour), weeklyWindow(0.99)),
				refused(t0, weeklyRefusal(r7)),
				polled(t0.Add(30*time.Minute), weeklyWindow(0.30)),
				polled(t0.Add(37*time.Minute), weeklyWindow(0.02)),
			},
			want: map[windowKey][]model.Lock{weekly: {ended(t0, t0.Add(30*time.Minute), model.LockEndCleared)}},
		},
		{
			name: "a cycle rolling onto a new reset ends the span as reset",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				read(t0.Add(time.Hour), unified("5h-utilization", "0.01", "5h-reset", unix(r5.Add(-time.Hour)))),
			},
			want: map[windowKey][]model.Lock{session: {ended(t0, t0.Add(time.Hour), model.LockEndReset)}},
		},
		{
			name: "a refusal admitted before a served request that ended the span reopens nothing",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				servedAdmitted(model.FamilySonnet, t0.Add(30*time.Minute+500*time.Millisecond), t0.Add(30*time.Minute+1500*time.Millisecond)),
				refusedAdmitted(t0.Add(30*time.Minute), t0.Add(30*time.Minute+3*time.Second), sessionRefusal(r5)),
			},
			want: map[windowKey][]model.Lock{session: {ended(t0, t0.Add(30*time.Minute+time.Second), model.LockEndServed)}},
		},
		{
			name: "a refusal admitted before a served request opens no span after the one it ended",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				served(model.FamilySonnet, t0.Add(5*time.Minute)),
				refusedAdmitted(t0.Add(4*time.Minute), t0.Add(10*time.Minute), sessionRefusal(r5)),
			},
			want: map[windowKey][]model.Lock{session: {ended(t0, t0.Add(5*time.Minute), model.LockEndServed)}},
		},
		{
			name: "a refusal admitted after a served request reopens the span it ended",
			steps: []func(*Store){
				refused(t0, sessionRefusal(r5)),
				servedAdmitted(model.FamilySonnet, t0.Add(5*time.Minute), t0.Add(6*time.Minute)),
				refusedAdmitted(t0.Add(5*time.Minute+time.Second), t0.Add(5*time.Minute+2*time.Second), sessionRefusal(r5)),
			},
			want: map[windowKey][]model.Lock{session: {span(t0, r5)}},
		},
		{
			name: "a late refusal before the span's start moves the start back",
			steps: []func(*Store){
				refusedAdmitted(t0.Add(50*time.Second), t0.Add(time.Minute), sessionRefusal(r5)),
				refusedAdmitted(t0.Add(-10*time.Second), t0, sessionRefusal(r5)),
			},
			want: map[windowKey][]model.Lock{session: {span(t0, r5)}},
		},
		{
			name: "a refusal after the expected reset ends the span before as reset",
			steps: []func(*Store){
				refused(t0, unified("5h-utilization", "1.0", "5h-status", "rejected")),
				refused(t0.Add(6*time.Hour), unified("5h-utilization", "1.0", "5h-status", "rejected")),
			},
			want: map[windowKey][]model.Lock{session: {
				ended(t0, t0.Add(model.SessionDuration), model.LockEndReset),
				span(t0.Add(6*time.Hour), t0.Add(6*time.Hour+model.SessionDuration)),
			}},
		},
		{
			name: "a single fallen reading leaves the span running",
			steps: []func(*Store){
				refused(t0, weeklyRefusal(r7)),
				polled(t0.Add(30*time.Minute), weeklyWindow(0.02)),
			},
			want: map[windowKey][]model.Lock{weekly: {span(t0, r7)}},
		},
		{
			name: "a derived family cap records its refusal",
			steps: []func(*Store){refused(t0, unified(
				"7d-utilization", "0.45", "7d-reset", unix(r7), "7d-status", "allowed",
				"representative-claim", "seven_day_opus", "status", "rejected", "reset", unix(r7),
			))},
			want: map[windowKey][]model.Lock{opus: {span(t0, r7)}},
		},
		{
			name: "a request admitted before the refusal and answered after it leaves the span running",
			steps: []func(*Store){
				refusedAdmitted(t0.Add(-time.Second), t0, sessionRefusal(r5)),
				servedAdmitted(model.FamilySonnet, t0.Add(-20*time.Second), t0.Add(5*time.Second)),
			},
			want: map[windowKey][]model.Lock{session: {span(t0, r5)}},
		},
		{
			name: "a request admitted before the refusal and answered in its second leaves the span running",
			steps: []func(*Store){
				refusedAdmitted(t0.Add(-time.Second), t0.Add(100*time.Millisecond), sessionRefusal(r5)),
				servedAdmitted(model.FamilySonnet, t0.Add(-20*time.Second), t0.Add(700*time.Millisecond)),
			},
			want: map[windowKey][]model.Lock{session: {span(t0, r5)}},
		},
		{
			name: "only a request admitted after the latest refusal ends the span",
			steps: []func(*Store){
				refused(t0, weeklyRefusal(r7)),
				refusedAdmitted(t0.Add(10*time.Minute-time.Second), t0.Add(10*time.Minute), weeklyRefusal(r7)),
				servedAdmitted(model.FamilySonnet, t0.Add(5*time.Minute), t0.Add(11*time.Minute)),
				servedAdmitted(model.FamilySonnet, t0.Add(10*time.Minute), t0.Add(12*time.Minute)),
			},
			want: map[windowKey][]model.Lock{weekly: {ended(t0, t0.Add(12*time.Minute), model.LockEndServed)}},
		},
		{
			name: "a request admitted after the refusal and answered in its second drops the span",
			steps: []func(*Store){
				refusedAdmitted(t0.Add(-time.Second), t0.Add(100*time.Millisecond), sessionRefusal(r5)),
				servedAdmitted(model.FamilySonnet, t0.Add(200*time.Millisecond), t0.Add(900*time.Millisecond)),
			},
			want: map[windowKey][]model.Lock{},
		},
		{
			name: "a refusal behind a fresher reading still opens a span",
			steps: []func(*Store){
				read(t0.Add(time.Second), unified("5h-utilization", "0.99", "5h-reset", unix(r5), "5h-status", "allowed_warning")),
				servedAdmitted(model.FamilySonnet, t0.Add(-20*time.Second), t0.Add(time.Second)),
				refusedAdmitted(t0.Add(-time.Second), t0, sessionRefusal(r5)),
			},
			want: map[windowKey][]model.Lock{session: {span(t0, r5)}},
		},
		{
			name: "a refusal with no reset of its own runs to the stored window's reset",
			steps: []func(*Store){
				polled(t0, weeklyWindow(0.9)),
				refused(t0.Add(time.Minute), unified("7d-utilization", "1.0", "7d-status", "rejected")),
			},
			want: map[windowKey][]model.Lock{weekly: {span(t0.Add(time.Minute), weeklyWindow(0).ResetsAt)}},
		},
		{
			name: "a rejection kept from an endpoint read opens nothing",
			steps: []func(*Store){
				polled(t0, model.Window{Kind: model.WindowWeekly, Utilization: 1, ResetsAt: r7, Duration: model.WeeklyDuration, Status: model.StatusRejected}),
				refused(t0.Add(5*time.Minute), unified("7d-utilization", "1.0", "7d-reset", unix(r7))),
			},
			want: map[windowKey][]model.Lock{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			for _, step := range tc.steps {
				step(s)
			}
			if got := locksOf(s, "auth-1"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("locks = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestADerivedCapRefusalKeepsASpanWithoutSamples covers a family cap only a
// refusal has named: its history carries the span and no cycle.
func TestADerivedCapRefusalKeepsASpanWithoutSamples(t *testing.T) {
	s := NewStore()
	h := unified("representative-claim", "seven_day_opus", "status", "rejected", "reset", epochDay(10, 14))
	refuse(s, "auth-1", h, testNow, testNow)
	got := s.History("auth-1", 0, testNow)
	want := []model.WindowHistory{{
		Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Cycles: []model.Cycle{},
		Locks: []model.Lock{span(testNow, day(10, 14))},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("history = %+v, want %+v", got, want)
	}
}

// TestRefusalSpansPublishWhole covers the published form: an ongoing span
// ends at the reset it expects, an ended one where it ended, and thinning the
// samples leaves every span.
func TestRefusalSpansPublishWhole(t *testing.T) {
	s := NewStore()
	refuse(s, "auth-1", sessionRefusal(at(21, 0)), testNow, testNow)
	s.MarkServed("auth-1", model.FamilyHaiku, testNow.Add(3*time.Minute), testNow.Add(3*time.Minute))
	refuse(s, "auth-1", fableRefusal(), testNow.Add(4*time.Minute), testNow.Add(4*time.Minute))
	for i := 0; i < 20; i++ {
		s.Put(endpointSnapshot("auth-1", testNow.Add(time.Duration(5+i)*time.Minute), sessionWindow(float64(i)/100)))
	}

	b, err := json.Marshal(s.History("auth-1", 3, testNow))
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Kind  model.WindowKind `json:"kind"`
		Scope string           `json:"scope"`
		Locks []model.Lock     `json:"locks"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	session := windowKey{kind: model.WindowSession}
	fable := windowKey{kind: model.WindowWeeklyScoped, scope: model.FamilyFable}
	want := map[windowKey][]model.Lock{
		session: {ended(testNow, testNow.Add(3*time.Minute), model.LockEndServed)},
		fable:   {span(testNow.Add(4*time.Minute), day(10, 14))},
	}
	for _, h := range got {
		key := windowKey{kind: h.Kind, scope: h.Scope}
		if !reflect.DeepEqual(h.Locks, want[key]) {
			t.Errorf("%v locks = %v, want %v", key, h.Locks, want[key])
		}
	}
}

// TestRefusalSpansSurviveSaveAndLoad covers the history file: spans ride
// along with the samples, an ongoing one stays ongoing after a load, and a
// file written before spans were recorded still loads.
func TestRefusalSpansSurviveSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s := NewStore()
	refuse(s, "auth-1", sessionRefusal(at(21, 0)), testNow, testNow)
	s.MarkServed("auth-1", model.FamilySonnet, testNow.Add(10*time.Minute), testNow.Add(10*time.Minute))
	fableAt := testNow.Add(20 * time.Minute)
	refuse(s, "auth-1", fableRefusal(), fableAt, fableAt)
	if err := s.SaveHistory(path, testNow); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore()
	if err := fresh.LoadHistory(path, testNow); err != nil {
		t.Fatal(err)
	}
	fresh.Put(endpointSnapshot("auth-1", testNow.Add(30*time.Minute), sessionWindow(0.3), fableWindow(1.0)))
	session := windowKey{kind: model.WindowSession}
	fable := windowKey{kind: model.WindowWeeklyScoped, scope: model.FamilyFable}
	want := map[windowKey][]model.Lock{
		session: {ended(testNow, testNow.Add(10*time.Minute), model.LockEndServed)},
		fable:   {span(fableAt, day(10, 14))},
	}
	if got := locksOf(fresh, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("locks after load = %v, want %v", got, want)
	}
	fresh.MarkServed("auth-1", model.FamilyFable, testNow.Add(40*time.Minute), testNow.Add(40*time.Minute))
	want[fable] = []model.Lock{ended(fableAt, testNow.Add(40*time.Minute), model.LockEndServed)}
	if got := locksOf(fresh, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a served Fable request = %v, want the loaded span ended there: %v", got, want)
	}

	old := filepath.Join(t.TempDir(), "old.json")
	body := `{"version":1,"seats":{"auth-1":[{"kind":"five_hour","cycles":[{"resets_at":"2026-09-04T21:00:00Z","samples":[[` +
		strconv.FormatInt(testNow.Unix(), 10) + `,0.1]]}]}]}}`
	if err := writeFile(old, body); err != nil {
		t.Fatal(err)
	}
	legacy := NewStore()
	if err := legacy.LoadHistory(old, testNow); err != nil {
		t.Fatalf("a file without locks: %v", err)
	}
	legacy.Put(endpointSnapshot("auth-1", testNow.Add(time.Minute), sessionWindow(0.2)))
	h := historyOf(t, legacy, "auth-1", model.WindowSession)
	if h.Samples() != 2 || h.Locks != nil {
		t.Errorf("history from a file without locks = %+v, want its sample plus the live one and no spans", h)
	}
}

// TestRefusalSpanRetention covers the two bounds: a span that ended before
// the oldest sample the window still holds goes, and past historyMaxLocks the
// oldest spans go.
func TestRefusalSpanRetention(t *testing.T) {
	r := &ring{}
	r.replay(model.WindowHistory{
		Kind:   model.WindowSession,
		Cycles: []model.Cycle{{ResetsAt: at(21, 0), Samples: []model.Sample{{At: testNow, Utilization: 0.5}}}},
		Locks: []model.Lock{
			span(testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)),
			span(testNow.Add(-30*time.Minute), testNow.Add(10*time.Minute)),
		},
	}, testNow)
	want := []model.Lock{span(testNow.Add(-30*time.Minute), testNow.Add(10*time.Minute))}
	if got := r.export(model.WindowSession, "", 0, testNow).Locks; !reflect.DeepEqual(got, want) {
		t.Errorf("locks = %v, want only the span reaching the oldest sample: %v", got, want)
	}

	r = &ring{}
	for i := 0; i < historyMaxLocks+6; i++ {
		from := testNow.Add(time.Duration(i) * 10 * time.Minute)
		r.refuse(from, from, from.Add(5*time.Minute))
	}
	locks := r.export(model.WindowSession, "", 0, testNow).Locks
	if first := testNow.Add(60 * time.Minute); len(locks) != historyMaxLocks || locks[0].From != first.Unix() {
		t.Errorf("kept %d spans from %v, want %d from %v", len(locks), locks[0].From, historyMaxLocks, first.Unix())
	}
}

// TestRefusalSpansGoWithTheCredential covers a pruned credential: its spans
// leave the published view with its samples and are forgotten with them.
func TestRefusalSpansGoWithTheCredential(t *testing.T) {
	s := NewStore()
	refuse(s, "auth-1", weeklyRefusal(day(10, 14)), testNow, testNow)
	s.Prune(map[string]struct{}{})
	if s.History("auth-1", 0, testNow) != nil {
		t.Error("a pruned credential still publishes history")
	}
	if len(s.ExportHistory(testNow)["auth-1"]) == 0 {
		t.Fatal("the pending history lost the pruned credential")
	}
	s.Prune(map[string]struct{}{})
	if _, ok := s.ExportHistory(testNow)["auth-1"]; ok {
		t.Error("a second prune kept the departed credential's spans")
	}
}

// TestAReturningCredentialKeepsItsAdmissions covers a credential a prune
// drops while a request is in flight: back in the listing, a request admitted
// before the span's latest refusal still leaves the span running, and a
// refusal admitted before a served request still changes nothing.
func TestAReturningCredentialKeepsItsAdmissions(t *testing.T) {
	reset := at(21, 0)
	session := windowKey{kind: model.WindowSession}
	readopt := func(s *Store, when time.Time) {
		s.Prune(map[string]struct{}{})
		s.MergeHeaders("auth-1", ParseResponseHeaders(sessionRefusal(reset), when), when)
	}
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow.Add(-time.Hour), sessionWindow(0.5)))
	refuse(s, "auth-1", sessionRefusal(reset), testNow.Add(4*time.Second), testNow.Add(5*time.Second))
	readopt(s, testNow.Add(40*time.Second))
	s.MarkServed("auth-1", model.FamilySonnet, testNow.Add(time.Second), testNow.Add(30*time.Second))
	want := map[windowKey][]model.Lock{session: {span(testNow.Add(5*time.Second), reset)}}
	if got := locksOf(s, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a request admitted before the refusal = %v, want the span running %v", got, want)
	}

	s.MarkServed("auth-1", model.FamilySonnet, testNow.Add(50*time.Second), testNow.Add(55*time.Second))
	readopt(s, testNow.Add(58*time.Second))
	s.RecordRefusals("auth-1", ParseResponseHeaders(sessionRefusal(reset), testNow.Add(time.Minute)), testNow.Add(45*time.Second), testNow.Add(time.Minute))
	want[session] = []model.Lock{ended(testNow.Add(5*time.Second), testNow.Add(55*time.Second), model.LockEndServed)}
	if got := locksOf(s, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a refusal admitted before the served request = %v, want %v", got, want)
	}
}

// TestMarkServedCountsOnlyAChange covers the history version the poller saves
// on: a served request that ends a span advances it, one that ends nothing
// leaves it.
func TestMarkServedCountsOnlyAChange(t *testing.T) {
	s := NewStore()
	refuse(s, "auth-1", fableRefusal(), testNow, testNow)
	before := s.HistoryVersion()
	s.MarkServed("auth-1", model.FamilySonnet, testNow.Add(time.Minute), testNow.Add(time.Minute))
	s.MarkServed("auth-9", model.FamilyFable, testNow.Add(time.Minute), testNow.Add(time.Minute))
	if got := s.HistoryVersion(); got != before {
		t.Errorf("version = %d after requests that ended nothing, want %d", got, before)
	}
	s.MarkServed("auth-1", model.FamilyFable, testNow.Add(2*time.Minute), testNow.Add(2*time.Minute))
	if got := s.HistoryVersion(); got != before+1 {
		t.Errorf("version = %d after a request ended a span, want %d", got, before+1)
	}
}

// TestImportTakesTheLiveEndOfAnOverlappingSpan covers a history file loaded
// after traffic: a live span overlapping the stored ongoing one ends the
// merged span where the live one ended, and the merged span still ends at a
// request admitted after the live refusal.
func TestImportTakesTheLiveEndOfAnOverlappingSpan(t *testing.T) {
	weekly := windowKey{kind: model.WindowWeekly}
	saved := NewStore()
	refuse(saved, "auth-1", weeklyRefusal(day(10, 14)), testNow, testNow)

	live := NewStore()
	refuse(live, "auth-1", weeklyRefusal(day(10, 14)), testNow.Add(time.Minute), testNow.Add(time.Minute))
	live.MarkServed("auth-1", model.FamilySonnet, testNow.Add(2*time.Hour), testNow.Add(2*time.Hour))
	live.ImportHistory(saved.ExportHistory(testNow), testNow.Add(3*time.Hour))
	want := map[windowKey][]model.Lock{weekly: {ended(testNow, testNow.Add(2*time.Hour), model.LockEndServed)}}
	if got := locksOf(live, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks = %v, want the stored start and the live end %v", got, want)
	}

	ongoing := NewStore()
	refuse(ongoing, "auth-1", weeklyRefusal(day(10, 14)), testNow.Add(time.Minute), testNow.Add(time.Minute))
	ongoing.ImportHistory(saved.ExportHistory(testNow), testNow.Add(90*time.Second))
	ongoing.MarkServed("auth-1", model.FamilySonnet, testNow.Add(30*time.Second), testNow.Add(3*time.Hour))
	want[weekly] = []model.Lock{span(testNow, day(10, 14))}
	if got := locksOf(ongoing, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a request admitted before the live refusal = %v, want %v", got, want)
	}
	ongoing.MarkServed("auth-1", model.FamilySonnet, testNow.Add(2*time.Minute), testNow.Add(3*time.Hour))
	want[weekly] = []model.Lock{ended(testNow, testNow.Add(3*time.Hour), model.LockEndServed)}
	if got := locksOf(ongoing, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a request admitted after it = %v, want %v", got, want)
	}
}

// TestAnExpiredSpanPublishesAsReset covers an ongoing span whose expected
// reset has passed: every export as of a later instant names the reset as
// what ended it, and one as of an earlier instant leaves it ongoing.
func TestAnExpiredSpanPublishesAsReset(t *testing.T) {
	s := NewStore()
	r5 := at(21, 0)
	refuse(s, "auth-1", sessionRefusal(r5), testNow, testNow)
	session := windowKey{kind: model.WindowSession}
	for _, tc := range []struct {
		now  time.Time
		want model.Lock
	}{
		{r5.Add(-time.Second), span(testNow, r5)},
		{r5, ended(testNow, r5, model.LockEndReset)},
	} {
		got := map[windowKey][]model.Lock{}
		for _, h := range s.History("auth-1", 0, tc.now) {
			if len(h.Locks) > 0 {
				got[windowKey{kind: h.Kind, scope: h.Scope}] = h.Locks
			}
		}
		if want := map[windowKey][]model.Lock{session: {tc.want}}; !reflect.DeepEqual(got, want) {
			t.Errorf("locks as of %v = %v, want %v", tc.now, got, want)
		}
		if exported := s.ExportHistory(tc.now)["auth-1"]; !reflect.DeepEqual(exported, s.History("auth-1", 0, tc.now)) {
			t.Errorf("the history file as of %v holds %+v, want what the status view publishes", tc.now, exported)
		}
	}
}

// TestALoadedSpanEndsOnlyAtARequestAdmittedAfterTheLoad covers a span
// extended by a later refusal and then loaded from disk: a request admitted
// between its start and that refusal leaves it running, as it would have
// live, and one admitted after the load ends it.
func TestALoadedSpanEndsOnlyAtARequestAdmittedAfterTheLoad(t *testing.T) {
	reset := testNow.Add(3 * time.Hour)
	path := filepath.Join(t.TempDir(), "history.json")
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow.Add(-time.Hour), sessionWindow(0.5)))
	refuse(s, "auth-1", sessionRefusal(reset), testNow.Add(-time.Second), testNow)
	refuse(s, "auth-1", sessionRefusal(reset), testNow.Add(60*time.Minute-time.Second), testNow.Add(60*time.Minute))
	if err := s.SaveHistory(path, testNow.Add(60*time.Minute)); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore()
	loaded := testNow.Add(61 * time.Minute)
	if err := fresh.LoadHistory(path, loaded); err != nil {
		t.Fatal(err)
	}
	full := sessionWindow(1.0)
	full.ResetsAt = reset
	fresh.Put(endpointSnapshot("auth-1", loaded, full))
	fresh.MarkServed("auth-1", model.FamilySonnet, testNow.Add(59*time.Minute), testNow.Add(59*time.Minute+2*time.Second))
	session := windowKey{kind: model.WindowSession}
	want := map[windowKey][]model.Lock{session: {span(testNow, reset)}}
	if got := locksOf(fresh, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a request admitted before the load = %v, want the span running %v", got, want)
	}
	fresh.MarkServed("auth-1", model.FamilySonnet, loaded.Add(time.Second), loaded.Add(3*time.Second))
	want[session] = []model.Lock{ended(testNow, loaded.Add(3*time.Second), model.LockEndServed)}
	if got := locksOf(fresh, "auth-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a request admitted after the load = %v, want %v", got, want)
	}
}

// TestRecordRefusalsCountsOnlyAChange covers the history version the poller
// saves on: a refusal that opens a span advances it, and one that changes no
// span leaves it.
func TestRecordRefusalsCountsOnlyAChange(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5)))
	ws := ParseResponseHeaders(sessionRefusal(at(21, 0)), testNow)
	before := s.HistoryVersion()
	s.RecordRefusals("auth-1", ws, testNow, testNow)
	if got := s.HistoryVersion(); got != before+1 {
		t.Fatalf("version = %d after a refusal opened a span, want %d", got, before+1)
	}
	s.RecordRefusals("auth-1", ws, testNow.Add(time.Minute), testNow.Add(time.Minute))
	s.MarkServed("auth-1", model.FamilySonnet, testNow.Add(2*time.Minute), testNow.Add(2*time.Minute))
	if got := s.HistoryVersion(); got != before+2 {
		t.Fatalf("version = %d after a refusal that changed no span and a request that ended it, want %d", got, before+2)
	}
	s.RecordRefusals("auth-1", ws, testNow.Add(time.Minute), testNow.Add(3*time.Minute))
	if got := s.HistoryVersion(); got != before+2 {
		t.Errorf("version = %d after a refusal admitted before the served request, want %d", got, before+2)
	}
}
