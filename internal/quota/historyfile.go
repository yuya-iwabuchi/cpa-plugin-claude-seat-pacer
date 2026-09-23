package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// It carries credential ids, which are file names or account addresses, and
// utilization fractions: nothing else, and no token.
type historyFile struct {
	Version int `json:"version"`
	// Auths is keyed by credential id. The JSON key is "seats", the name the
	// format shipped with.
	Auths map[string][]model.WindowHistory `json:"seats"`
}

// SaveHistory writes the store's whole history to path, by writing and syncing
// a temporary file of its own in the same directory and renaming it into
// place, so a reader never sees a partial file and two writers never share a
// temporary one. The directory is created as needed.
func (s *Store) SaveHistory(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(historyFile{Version: historyFileVersion, Auths: s.ExportHistory()})
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

// LoadHistory reads a file SaveHistory wrote and imports it. A missing file is
// not an error. A file of a newer version is left in place and reported as
// ErrHistoryVersion. A file that does not parse, or names a version no build
// writes, is renamed to path.corrupt-<unix seconds at now> and reported as a
// CorruptHistoryError; the store is left alone either way.
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
		s.ImportHistory(f.Auths)
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
