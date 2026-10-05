package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// historyFileVersion is the format of the history file; a file of another
// version is ignored rather than misread.
const historyFileVersion = 1

// ErrHistoryVersion reports a history file of a newer format than this build
// writes. LoadHistory leaves such a file where it is, since the build that
// wrote it can still read it.
var ErrHistoryVersion = errors.New("history file is of a newer version than this build reads")

// CorruptHistoryError reports a history file that did not parse, or named no
// format any build writes, and has been renamed to Aside. The path it was read
// from is free again, so the next save starts a fresh file there.
type CorruptHistoryError struct {
	Aside string
	Err   error
}

func (e *CorruptHistoryError) Error() string {
	return "history file is unreadable and was moved to " + e.Aside + ": " + e.Err.Error()
}

func (e *CorruptHistoryError) Unwrap() error { return e.Err }

// historyFile is the on-disk form of every credential's recorded utilization.
// It carries credential ids, which are file names or account addresses,
// utilization fractions, the spans in which the provider refused each window
// with what ended each, and each credential's last reading: nothing else, and
// no token.
type historyFile struct {
	Version int `json:"version"`
	// Auths is keyed by credential id. The JSON key is "seats", the name the
	// format shipped with.
	Auths map[string][]model.WindowHistory `json:"seats"`
	// Readings is each credential's last reading, keyed by credential id. A
	// build that does not know the key ignores it.
	Readings map[string]savedReading `json:"readings,omitempty"`
}

// savedReading is a credential's last reading as the history file holds it.
type savedReading struct {
	ObservedAt time.Time      `json:"observed_at"`
	Source     string         `json:"source,omitempty"`
	Windows    []model.Window `json:"windows"`
}

// exportReadings copies every credential's last reading, pending ones
// included. A reading with a utilization JSON cannot carry is left out rather
// than failing the whole file.
func (s *Store) exportReadings() map[string]savedReading {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]savedReading, len(s.entries)+len(s.pending))
	add := func(id string, e *entry) {
		if e.snap.ObservedAt.IsZero() || len(e.snap.Windows) == 0 {
			return
		}
		for _, w := range e.snap.Windows {
			if math.IsNaN(w.Utilization) || math.IsInf(w.Utilization, 0) {
				return
			}
		}
		out[id] = savedReading{ObservedAt: e.snap.ObservedAt, Source: e.snap.Source, Windows: slices.Clone(e.snap.Windows)}
	}
	for id, e := range s.pending {
		add(id, e)
	}
	for id, e := range s.entries {
		add(id, e)
	}
	return out
}

// importReadings holds saved readings as pending, for Restore to make live
// once the host lists their credentials. A credential with an entry already
// has a newer reading, and a pending reading newer than the saved one stands.
func (s *Store) importReadings(saved map[string]savedReading) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, r := range saved {
		if id == "" || r.ObservedAt.IsZero() || len(r.Windows) == 0 {
			continue
		}
		if _, live := s.entries[id]; live {
			continue
		}
		p := s.pending[id]
		if p == nil {
			p = &entry{history: make(map[windowKey]*ring)}
			s.pending[id] = p
		}
		if !p.snap.ObservedAt.Before(r.ObservedAt) {
			continue
		}
		p.snap = model.AuthSnapshot{AuthID: id, Windows: slices.Clone(r.Windows), ObservedAt: r.ObservedAt, Source: r.Source}
		p.seenAt = nil
	}
}

// SaveHistory writes the store's whole history as of now, and every
// credential's last reading, to path, by writing and syncing a temporary file
// of its own in the same directory and renaming it into place, so a reader
// never sees a partial file and two writers never share a temporary one. The
// directory is created as needed.
func (s *Store) SaveHistory(path string, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(historyFile{Version: historyFileVersion, Auths: s.ExportHistory(now), Readings: s.exportReadings()})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(body)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// LoadHistory reads a file SaveHistory wrote and imports it as loaded at now,
// its readings held for Restore. A missing file is not an error. A file of a newer version is left in place
// and reported as ErrHistoryVersion. A file that does not parse, or names a
// version no build writes, is renamed to path.corrupt-<unix seconds at now>
// and reported as a CorruptHistoryError; the store is left alone either way.
func (s *Store) LoadHistory(path string, now time.Time) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f historyFile
	parseErr := json.Unmarshal(body, &f)
	switch {
	case parseErr == nil && f.Version == historyFileVersion:
		s.ImportHistory(f.Auths, now)
		s.importReadings(f.Readings)
		return nil
	case parseErr == nil && f.Version > historyFileVersion:
		return ErrHistoryVersion
	case parseErr == nil:
		parseErr = errors.New("history file names version " + strconv.Itoa(f.Version))
	}
	aside := path + ".corrupt-" + strconv.FormatInt(now.Unix(), 10)
	if err := os.Rename(path, aside); err != nil {
		return fmt.Errorf("history file is unreadable (%v) and could not be moved aside: %w", parseErr, err)
	}
	return &CorruptHistoryError{Aside: aside, Err: parseErr}
}
