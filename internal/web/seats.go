package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
)

// maxSessionIDLength bounds the session id a seats request names. A Claude
// Code session id is a 36-character UUID.
const maxSessionIDLength = 128

// maxSinceAge bounds how far back a request's since may reach: the history the
// status carries spans about nine days.
const maxSinceAge = 8 * 24 * time.Hour

// SessionBinder is a Source that can name the binding a Claude Code session's
// root agent holds for a model, and when that binding expires unless used
// again: zero when bindings never expire. A Source without it answers every
// request with a null binding.
type SessionBinder interface {
	SessionBinding(sessionID, modelID string) (b model.Binding, expiresAt time.Time, ok bool)
}

// SpendRater is a Source that can give each seat's use per dollar of
// list-price spend, by credential id. A Source without it answers every
// request with no rates.
type SpendRater interface {
	SpendRates(now time.Time) map[string]model.SpendRates
}

// LocalAPIVersion is the local API's format version. It rises when a field goes
// away or changes meaning, and not when a field or a field's value is added.
const LocalAPIVersion = 1

// SeatsResponse is the seats route's answer: one conversation's seat and every
// seat's windows, in the page's published form, with masked names and keyed
// ids. PolledAt is absent before the first usage read.
type SeatsResponse struct {
	Version  int                   `json:"version"`
	Now      time.Time             `json:"now"`
	PolledAt *time.Time            `json:"polled_at,omitempty"`
	Model    string                `json:"model"`
	Binding  *SeatsResponseBinding `json:"binding"`
	Next     string                `json:"next,omitempty"`
	Seats    []SeatsResponseSeat   `json:"seats"`
	Warnings []string              `json:"warnings"`
}

// SeatsResponseBinding is the seat the asking conversation is bound to, by its
// published id. ExpiresAt is when the binding expires unless the conversation
// sends another request, after which its next request is picked again; absent
// when bindings never expire.
type SeatsResponseBinding struct {
	AuthID    string     `json:"auth_id"`
	Model     string     `json:"model"`
	LastSeen  time.Time  `json:"last_seen"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SeatsResponseSeat is one seat: its name as the page shows it, where the pick
// stands with it, and its windows. Slack is the 7-day plan's lead over the
// seat's use, absent where the seat is unscored.
type SeatsResponseSeat struct {
	AuthID     string   `json:"auth_id"`
	Label      string   `json:"label"`
	Priority   int      `json:"priority"`
	HostStatus string   `json:"host_status,omitempty"`
	Eligible   bool     `json:"eligible"`
	Reason     string   `json:"reason,omitempty"`
	Slack      *float64 `json:"slack,omitempty"`
	// WeeklyPerUSD is how much of the 7-day window a dollar of list-price
	// spend used on this seat lately, absent until the plugin knows.
	WeeklyPerUSD *float64 `json:"weekly_per_usd,omitempty"`
	// FiveHourPerUSD is the same for the 5-hour window.
	FiveHourPerUSD *float64              `json:"five_hour_per_usd,omitempty"`
	Windows        []SeatsResponseWindow `json:"windows"`
}

// SeatsResponseWindow is one window reading. Utilization is a fraction, above
// 1 past the cap, and null when the reading is not a finite number. UsedSince
// is how much of the window was used after the request's since, present when
// it named one and the history holds the window. ResetsAt is absent when the
// reading names no reset.
type SeatsResponseWindow struct {
	Kind        model.WindowKind `json:"kind"`
	Scope       string           `json:"scope,omitempty"`
	Utilization float64          `json:"utilization"`
	ResetsAt    *time.Time       `json:"resets_at,omitempty"`
	UsedSince   *float64         `json:"used_since,omitempty"`
}

// serveSeats answers the seats route: the status for the model the request
// names, reduced as the page's route reduces it, and the session's binding for
// the model the status is scored for. since gives each window its use after
// that instant, and since_five_hour does so for the 5-hour window in its
// place. One that is no RFC 3339 instant within maxSinceAge before now is
// ignored; a "+" offset left unescaped decodes as a space and reads as "+". It
// never syncs: the poll loop keeps the readings fresh on its own cadence,
// however often a client asks.
func serveSeats(w http.ResponseWriter, r *http.Request, src Source, key []byte) {
	q := r.URL.Query()
	sessionID := q.Get("session")
	if len(sessionID) > maxSessionIDLength {
		setCommonHeaders(w)
		http.Error(w, "session id too long", http.StatusBadRequest)
		return
	}
	now := time.Now()
	st := src.Status(now, requestModel(q.Get("model")))
	since := sinceOf(q.Get("since"), now)
	fiveSince := sinceOf(q.Get("since_five_hour"), now)
	if fiveSince.IsZero() {
		fiveSince = since
	}
	var binding *SeatsResponseBinding
	if binder, ok := src.(SessionBinder); ok && sessionID != "" {
		if b, expires, ok := binder.SessionBinding(sessionID, st.Model); ok {
			binding = &SeatsResponseBinding{AuthID: publicID(key, b.AuthID), Model: b.Model, LastSeen: b.LastSeen, ExpiresAt: timeOrNil(expires)}
		}
	}
	var rates map[string]model.SpendRates
	if rater, ok := src.(SpendRater); ok {
		rates = make(map[string]model.SpendRates)
		for id, r := range rater.SpendRates(now) {
			rates[publicID(key, id)] = r
		}
	}
	answer := responseOf(reduceForPage(st, key), binding, rates, since, fiveSince)
	if next, ok := nextOf(st); ok {
		answer.Next = publicID(key, next)
	}
	body, err := encodeData(answer)
	if err != nil {
		setCommonHeaders(w)
		http.Error(w, "seats answer encode failed", http.StatusInternalServerError)
		return
	}
	writeRawJSON(w, body)
}

// requestModel is the model id a request for modelID carries upstream. Claude
// Code names a model with a larger context window by a bracketed suffix, as in
// claude-opus-5-5[1m], and sends its requests without it.
func requestModel(modelID string) string {
	if i := strings.LastIndexByte(modelID, '['); i > 0 && strings.HasSuffix(modelID, "]") {
		return modelID[:i]
	}
	return modelID
}

// responseOf picks the answer's figures, all but Next, out of a status already
// reduced for the page. A non-zero since gives each window the use its history
// records after it, and a non-zero fiveSince does so for the 5-hour window in
// its place. rates are by published id.
func responseOf(st model.Status, binding *SeatsResponseBinding, rates map[string]model.SpendRates, since, fiveSince time.Time) SeatsResponse {
	answer := SeatsResponse{
		Version:  LocalAPIVersion,
		Now:      st.Now,
		PolledAt: timeOrNil(st.PolledAt),
		Model:    st.Model,
		Binding:  binding,
		Seats:    make([]SeatsResponseSeat, 0, len(st.Auths)),
		Warnings: st.Warnings,
	}
	for _, a := range st.Auths {
		seat := SeatsResponseSeat{
			AuthID:         a.AuthID,
			Label:          a.Label,
			Priority:       a.Priority,
			HostStatus:     a.HostStatus,
			Eligible:       a.Score.Eligible,
			Reason:         a.Score.Reason,
			WeeklyPerUSD:   rates[a.AuthID].Weekly,
			FiveHourPerUSD: rates[a.AuthID].FiveHour,
			Windows:        make([]SeatsResponseWindow, 0, len(a.Snapshot.Windows)),
		}
		for _, ws := range a.Score.Windows {
			if ws.Kind == model.WindowWeekly && ws.Scope == "" && ws.Slack != nonFiniteSentinel {
				slack := ws.Slack
				seat.Slack = &slack
			}
		}
		for _, w := range a.Snapshot.Windows {
			window := SeatsResponseWindow{Kind: w.Kind, Scope: w.Scope, Utilization: w.Utilization, ResetsAt: timeOrNil(w.ResetsAt)}
			from := since
			if w.Kind == model.WindowSession {
				from = fiveSince
			}
			if !from.IsZero() {
				window.UsedSince = usedSince(a.History, w, from)
			}
			seat.Windows = append(seat.Windows, window)
		}
		answer.Seats = append(answer.Seats, seat)
	}
	return answer
}

// nextOf is the credential id of the seat a new conversation goes to: the one
// the pick ranks first among the top priority tier the host offers. A seat the
// host holds back never takes a conversation, whatever its priority.
func nextOf(st model.Status) (string, bool) {
	top, hasTop := 0, false
	for _, a := range st.Auths {
		if a.Score.Reason != model.ReasonNotCandidate && (!hasTop || a.Priority > top) {
			top, hasTop = a.Priority, true
		}
	}
	var scores []model.Score
	for _, a := range st.Auths {
		if hasTop && a.Priority == top {
			scores = append(scores, a.Score)
		}
	}
	best, ok := pace.Best(scores)
	return best.AuthID, ok
}

// timeOrNil is t, or nil when t is the zero instant that stands for none.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// usedSince is how much of a window its history records as used after since,
// or nil when the history holds no record of the window.
func usedSince(history []model.WindowHistory, w model.Window, since time.Time) *float64 {
	for _, h := range history {
		if h.Kind == w.Kind && h.Scope == w.Scope {
			rise := h.Rise(since)
			return &rise
		}
	}
	return nil
}

// sinceOf reads a since parameter, zero when it is absent or out of bounds.
func sinceOf(raw string, now time.Time) time.Time {
	since, err := time.Parse(time.RFC3339, strings.Replace(raw, " ", "+", 1))
	if err != nil || since.After(now) || since.Before(now.Add(-maxSinceAge)) {
		return time.Time{}
	}
	return since
}
