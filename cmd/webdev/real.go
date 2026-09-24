package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// fromHistory replaces the fixture's seats with the ones a real history file
// records: each seat's windows are the newest cycle of each recorded window,
// its reading the newest sample. The history file is only read. Each seat's
// note and email come from its credential file in the directory two above the
// history file's own, which is the host's auth directory in a default install;
// elsewhere every note reads as empty. The install's page-id.key beside the
// history file is copied, so the seats' published ids are the host page's.
// The scenario's seats, bindings, decisions and listing failure are cleared.
func (f *fixture) fromHistory(path string, now time.Time) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw struct {
		Seats map[string][]model.WindowHistory `json:"seats"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	store := quota.NewStore()
	store.ImportHistory(raw.Seats)

	ids := make([]string, 0, len(raw.Seats))
	for id := range raw.Seats {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	authDir := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	f.auths = nil
	f.bindings = nil
	f.decisions = nil
	f.listErr = ""
	f.clearedAgo = nil
	f.snapshotsPast = nil
	f.snapshots = map[string]model.AuthSnapshot{}
	f.observedAge = map[string]time.Duration{}
	f.realHistory = map[string][]model.WindowHistory{}
	for i, id := range ids {
		snap := model.AuthSnapshot{AuthID: id, AuthIndex: strconv.Itoa(i), Source: model.SourceUsageEndpoint}
		var newest time.Time
		for _, h := range raw.Seats[id] {
			if len(h.Cycles) == 0 {
				continue
			}
			c := h.Cycles[len(h.Cycles)-1]
			if len(c.Samples) == 0 {
				continue
			}
			last := c.Samples[len(c.Samples)-1]
			if last.At.After(newest) {
				newest = last.At
			}
			dur := model.WeeklyDuration
			if h.Kind == model.WindowSession {
				dur = model.SessionDuration
			}
			snap.Windows = append(snap.Windows, model.Window{
				Kind: h.Kind, Scope: h.Scope, Utilization: last.Utilization,
				ResetsAt: c.ResetsAt, Duration: dur,
				Status: model.StatusAllowed, Severity: model.SeverityNormal,
				Active: h.Kind == model.WindowSession,
			})
		}
		snap.ObservedAt = newest
		// The store holds an import until a seat's first reading arrives, so
		// the seat is put before its history is read back.
		store.Put(snap)
		note, email := credentialName(filepath.Join(authDir, id))
		snap.Label = note
		f.snapshots[id] = snap
		f.realHistory[id] = store.History(id, quota.HistoryPublishMax)
		if !newest.IsZero() {
			f.observedAge[id] = now.Sub(newest)
		}
		// The host labels a seat by its note; with none, the page names it
		// from its file name and address, as it does a host row.
		f.auths = append(f.auths, model.AuthStatus{
			AuthID: id, Name: id, Label: note, Email: email,
			Provider: "claude", Priority: 10, HostStatus: "active",
		})
	}

	// The web layer rewrites a key file of the wrong length, so it reads a
	// copy of its own rather than the install's.
	key, err := os.ReadFile(filepath.Join(filepath.Dir(path), "page-id.key"))
	if err != nil {
		return nil
	}
	tmp, err := os.CreateTemp("", "webdev-page-id-*.key")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(key)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	f.idKeyFile = tmp.Name()
	return nil
}

// credentialName reads a credential file's note and email and nothing else
// from it.
func credentialName(path string) (note, email string) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var c struct {
		Note  string `json:"note"`
		Email string `json:"email"`
	}
	if json.Unmarshal(body, &c) != nil {
		return "", ""
	}
	return strings.TrimSpace(c.Note), c.Email
}

// PageIDKeyFile names the key published ids are derived with.
func (f *fixture) PageIDKeyFile() string { return f.idKeyFile }
