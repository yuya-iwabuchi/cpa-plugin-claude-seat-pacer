package runtime

import "path/filepath"

// pageIDKeyName is the file, beside the history file, that holds the key the
// status app derives published credential ids with.
const pageIDKeyName = "page-id.key"

// PageIDKeyFile is where the status app keeps the key it derives published
// credential ids with: in the history file's directory, so the ids the page
// keeps per-seat state under read the same across restarts. Empty when no
// history location is known, which leaves the app a key of the process's own.
func (p *Plugin) PageIDKeyFile() string {
	if p.opts.HistoryFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.opts.HistoryFile), pageIDKeyName)
}
