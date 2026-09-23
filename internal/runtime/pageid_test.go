package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/web"
)

// The status app reads its key file through a type assertion that would
// otherwise fail in silence, leaving the page's ids to change on every restart.
var _ web.IDKeyFiler = (*Plugin)(nil)

// TestPageIDKeyIsKeptBesideTheHistory covers the page's ids across a restart:
// the key they are derived with is written beside the history file, owner-only,
// and a new app over the same plugin state publishes the same ids.
func TestPageIDKeyIsKeptBesideTheHistory(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	served := func() []string {
		t.Helper()
		tp.SetResourceHandler(web.NewHandler(tp.Plugin))
		resp := tp.manage(t, http.MethodGet, testMgmtPrefix+routePageStatus, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page status = %d: %s", resp.StatusCode, resp.Body)
		}
		var st model.Status
		if err := json.Unmarshal(resp.Body, &st); err != nil {
			t.Fatalf("decode page status: %v", err)
		}
		ids := make([]string, len(st.Auths))
		for i, a := range st.Auths {
			ids[i] = a.AuthID
		}
		return ids
	}
	first := served()

	want := filepath.Join(filepath.Dir(tp.opts.HistoryFile), "page-id.key")
	if got := tp.PageIDKeyFile(); got != want {
		t.Errorf("PageIDKeyFile = %q, want %q", got, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %v, want 0600", mode)
	}

	again := served()
	if len(first) == 0 || len(again) != len(first) {
		t.Fatalf("served ids = %q then %q, want the same non-empty rows", first, again)
	}
	for i := range first {
		if again[i] != first[i] {
			t.Errorf("row %d id = %q after a restart, want %q", i, again[i], first[i])
		}
	}
}
