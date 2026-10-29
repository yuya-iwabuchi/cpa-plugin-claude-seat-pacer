package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// maxSessionIDLength bounds the session id a seats request names. A Claude
// Code session id is a 36-character UUID.
const maxSessionIDLength = 128

// SessionBinder is a Source that can name the binding a Claude Code session's
// root agent holds for a model, and when that binding expires unless used
// again: zero when bindings never expire. A Source without it answers every
// request with a null binding.
type SessionBinder interface {
	SessionBinding(sessionID, modelID string) (b model.Binding, expiresAt time.Time, ok bool)
}

// LocalAPIVersion is the local API's format version. It rises when a field goes
// away or changes meaning, and not when a field or a field's value is added.
const LocalAPIVersion = 1

// SeatsAnswer is the seats route's answer: one conversation's seat and every
// seat's windows, in the page's published form, with masked names and keyed
// ids.
type SeatsAnswer struct {
	Version  int            `json:"version"`
	Now      time.Time      `json:"now"`
	PolledAt time.Time      `json:"polled_at"`
	Model    string         `json:"model"`
	Binding  *AnswerBinding `json:"binding"`
	Next     string         `json:"next,omitempty"`
	Seats    []AnswerSeat   `json:"seats"`
	Warnings []string       `json:"warnings"`
}

// AnswerBinding is the seat the asking conversation is bound to, by its
// published id. ExpiresAt is when the binding expires unless the conversation
// sends another request, after which its next request is picked again; absent
// when bindings never expire.
type AnswerBinding struct {
	AuthID    string     `json:"auth_id"`
	Model     string     `json:"model"`
	LastSeen  time.Time  `json:"last_seen"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// AnswerSeat is one seat: its name as the page shows it, where the pick
// stands with it, and its windows. Slack is the 7-day plan's lead over the
// seat's use, absent where the seat is unscored.
type AnswerSeat struct {
	AuthID     string         `json:"auth_id"`
	Label      string         `json:"label"`
	Priority   int            `json:"priority"`
	HostStatus string         `json:"host_status,omitempty"`
	Eligible   bool           `json:"eligible"`
	Reason     string         `json:"reason,omitempty"`
	Slack      *float64       `json:"slack,omitempty"`
	Windows    []AnswerWindow `json:"windows"`
}

// AnswerWindow is one window reading. Utilization is a fraction, above 1 past
// the cap, and null when the reading is not a finite number.
type AnswerWindow struct {
	Kind        model.WindowKind `json:"kind"`
	Scope       string           `json:"scope,omitempty"`
	Utilization float64          `json:"utilization"`
	ResetsAt    time.Time        `json:"resets_at"`
}

// serveSeats answers the seats route: the status for the model the request
// names, reduced as the page's route reduces it, and the session's binding for
// the model the status is scored for. It never syncs: the poll loop keeps the
// readings fresh on its own cadence, however often a client asks.
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
	var binding *AnswerBinding
	if binder, ok := src.(SessionBinder); ok && sessionID != "" {
		if b, expires, ok := binder.SessionBinding(sessionID, st.Model); ok {
			binding = &AnswerBinding{AuthID: publicID(key, b.AuthID), Model: b.Model, LastSeen: b.LastSeen}
			if !expires.IsZero() {
				binding.ExpiresAt = &expires
			}
		}
	}
	body, err := encodeData(answerOf(reduceForPage(st, key), binding))
	if err != nil {
		setCommonHeaders(w)
		http.Error(w, "seats answer encode failed", http.StatusInternalServerError)
		return
	}
	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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

// answerOf picks the answer's figures out of a status already reduced for the
// page. Next is the eligible seat of the top priority tier the pick ranks
// first, the one a new conversation goes to: a seat with no weekly cap ahead,
// then the lowest cost. Seats arrive in credential id order, so a tie goes to
// the lower id, as the pick breaks it.
func answerOf(st model.Status, binding *AnswerBinding) SeatsAnswer {
	g := SeatsAnswer{
		Version:  LocalAPIVersion,
		Now:      st.Now,
		PolledAt: st.PolledAt,
		Model:    st.Model,
		Binding:  binding,
		Seats:    make([]AnswerSeat, 0, len(st.Auths)),
		Warnings: st.Warnings,
	}
	// The top tier is the highest priority among the seats the host offers:
	// a seat it holds back never takes a conversation, whatever its priority.
	top, hasTop := 0, false
	for _, a := range st.Auths {
		if a.Score.Reason != model.ReasonNotCandidate && (!hasTop || a.Priority > top) {
			top, hasTop = a.Priority, true
		}
	}
	var best *model.AuthStatus
	for i, a := range st.Auths {
		seat := AnswerSeat{
			AuthID:     a.AuthID,
			Label:      a.Label,
			Priority:   a.Priority,
			HostStatus: a.HostStatus,
			Eligible:   a.Score.Eligible,
			Reason:     a.Score.Reason,
			Windows:    make([]AnswerWindow, 0, len(a.Snapshot.Windows)),
		}
		for _, ws := range a.Score.Windows {
			if ws.Kind == model.WindowWeekly && ws.Scope == "" && ws.Slack != nonFiniteSentinel {
				slack := ws.Slack
				seat.Slack = &slack
			}
		}
		for _, w := range a.Snapshot.Windows {
			seat.Windows = append(seat.Windows, AnswerWindow{Kind: w.Kind, Scope: w.Scope, Utilization: w.Utilization, ResetsAt: w.ResetsAt})
		}
		g.Seats = append(g.Seats, seat)
		if a.Priority == top && a.Score.Eligible && (best == nil || ahead(a.Score, best.Score)) {
			best = &st.Auths[i]
		}
	}
	if best != nil {
		g.Next = best.AuthID
	}
	return g
}

// ahead reports whether the pick ranks eligible score a before b: a seat with
// no weekly cap first, then the lower cost, a cost the page could not express
// as a number counting as the highest.
func ahead(a, b model.Score) bool {
	switch {
	case a.SessionOnly != b.SessionOnly:
		return a.SessionOnly
	case a.Cost == nonFiniteSentinel:
		return false
	case b.Cost == nonFiniteSentinel:
		return true
	}
	return a.Cost < b.Cost
}
