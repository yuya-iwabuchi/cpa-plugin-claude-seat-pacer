package quota

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// historyFileVersion is the format of the history file; a file of another
// version is ignored rather than misread.
const historyFileVersion = 1

// historyFile is the on-disk form of every credential's recorded utilization.
// It carries credential ids, which are file names or account addresses, and
// utilization fractions: nothing else, and no token.
type historyFile struct {
	Version int                              `json:"version"`
	Seats   map[string][]model.WindowHistory `json:"seats"`
}

// SaveHistory writes the store's whole history to path, by writing a sibling
// file and renaming it into place so a reader never sees a partial file. The
// directory is created as needed.
func (s *Store) SaveHistory(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(historyFile{Version: historyFileVersion, Seats: s.ExportHistory()})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadHistory reads a file SaveHistory wrote and imports it. A missing file
// is not an error; a file that does not parse or is of another version is.
func (s *Store) LoadHistory(path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f historyFile
	if err := json.Unmarshal(body, &f); err != nil {
		return err
	}
	if f.Version != historyFileVersion {
		return errors.New("history file is of an unknown version")
	}
	s.ImportHistory(f.Seats)
	return nil
}
