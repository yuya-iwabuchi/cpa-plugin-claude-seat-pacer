package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// bindingsFileName is the file, beside the history file, that carries the
// binding table from one run to the next. Its name does not end in .json: the
// host loads every .json file under its auth directory, which holds the
// history file's directory by default, as a credential.
const bindingsFileName = "bindings.state"

// bindingsFileVersion is the format of the bindings file; a file of another
// version is ignored rather than misread.
const bindingsFileVersion = 1

// bindingsFile is the on-disk form of the binding table. A conversation key in
// it is a digest, never the header or prompt it was derived from.
type bindingsFile struct {
	Version  int             `json:"version"`
	Bindings []model.Binding `json:"bindings"`
}

// bindingsPath is where the bindings file lives: in the history file's
// directory. Empty when no history location is known, which turns binding
// persistence off.
func (p *Plugin) bindingsPath() string {
	if p.opts.HistoryFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.opts.HistoryFile), bindingsFileName)
}

// persistsBindings reports whether cfg keeps bindings on disk. A config block
// that does not parse decodes as disabled and keeps none, since the block may
// have turned persistence off.
func (p *Plugin) persistsBindings(cfg model.Config) bool {
	return cfg.Enabled && cfg.Affinity.Enabled && cfg.Affinity.Persist && p.bindingsPath() != ""
}

// saveBindings writes every live binding to the bindings file, or removes the
// file when the config keeps no bindings on disk. It runs when the host stops
// the build, at plugin.quiesce or shutdown. A build no configure has reached
// holds no table and leaves the file alone. A failed write costs only the next
// run's bindings, so it is logged and dropped.
func (p *Plugin) saveBindings() {
	p.mu.Lock()
	store := p.bindings
	p.mu.Unlock()
	if store == nil {
		return
	}
	cfg := p.config()
	path := p.bindingsPath()
	if !p.persistsBindings(cfg) {
		if path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				p.host.log("warn", "claude-seat-pacer could not remove the conversation bindings file", map[string]any{"error": err.Error()})
			}
		}
		return
	}
	now := p.now()
	all := store.All()
	live := all[:0]
	for _, b := range all {
		if now.Sub(b.LastSeen) <= cfg.Affinity.TTL {
			live = append(live, b)
		}
	}
	body, err := json.Marshal(bindingsFile{Version: bindingsFileVersion, Bindings: live})
	if err == nil {
		err = quota.WriteFile(path, body)
	}
	if err != nil {
		p.host.log("warn", "claude-seat-pacer could not save the conversation bindings", map[string]any{"error": err.Error()})
	}
}

// restoreBindings replaces the binding table with the bindings file's. It runs
// at plugin.register and at a build's first configure, before the host routes
// a pick to this build. A build may hold a table already, since a host that
// loads a library it loaded before reuses that image's memory, but the build
// that ran in between owns the live table and saved it when the host stopped
// it. A file that is missing, unreadable, unparseable or of another version
// leaves the table empty, and the next save replaces it.
func (p *Plugin) restoreBindings(cfg model.Config) {
	if !p.persistsBindings(cfg) {
		return
	}
	p.mu.Lock()
	store := p.newBindingsLocked(cfg)
	p.mu.Unlock()

	body, err := os.ReadFile(p.bindingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f bindingsFile
	if err == nil {
		err = json.Unmarshal(body, &f)
	}
	if err == nil && f.Version != bindingsFileVersion {
		err = fmt.Errorf("bindings file names version %d", f.Version)
	}
	if err != nil {
		p.host.log("warn", "claude-seat-pacer could not restore the conversation bindings", map[string]any{"error": err.Error()})
		return
	}
	if n := store.Restore(f.Bindings, p.now()); n > 0 {
		p.host.log("info", fmt.Sprintf("claude-seat-pacer restored %d conversation bindings", n), nil)
	}
}
