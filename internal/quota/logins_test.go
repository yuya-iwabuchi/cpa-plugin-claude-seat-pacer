package quota

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoginInstantsSurviveTheHistoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	at := testNow.Add(-time.Hour)
	s := NewStore()
	before := s.HistoryVersion()
	s.SetLoggedIn("seat-a", at, "d1")
	s.SetLoggedIn("seat-b", testNow.Add(time.Hour), "")
	if s.HistoryVersion() == before {
		t.Error("a login instant left the history version alone")
	}
	v := s.HistoryVersion()
	s.SetLoggedIn("seat-a", at, "d1")
	if s.HistoryVersion() != v {
		t.Error("the same login instant advanced the history version")
	}
	s.SetLoggedIn("seat-a", at, "d2")
	if s.HistoryVersion() == v {
		t.Error("a new device digest left the history version alone")
	}
	s.SetLoggedIn("seat-a", at, "d1")
	if err := s.SaveHistory(path, testNow); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore()
	if err := fresh.LoadHistory(path, testNow); err != nil {
		t.Fatal(err)
	}
	if got, device, ok := fresh.LoggedIn("seat-a"); !ok || !got.Equal(at) || device != "d1" {
		t.Errorf("seat-a = %v %q %v, want %v d1", got, device, ok, at)
	}
	// An instant after the load was stamped under a clock since set back.
	if got, _, ok := fresh.LoggedIn("seat-b"); ok {
		t.Errorf("seat-b = %v, want dropped", got)
	}

	// A login this run saw made outranks the saved one.
	live := NewStore()
	live.SetLoggedIn("seat-a", testNow, "")
	if err := live.LoadHistory(path, testNow); err != nil {
		t.Fatal(err)
	}
	if got, device, _ := live.LoggedIn("seat-a"); !got.Equal(testNow) || device != "" {
		t.Errorf("seat-a after a load = %v, want this run's %v", got, testNow)
	}

	// A credential the host no longer lists loses its instant.
	fresh.Prune(map[string]struct{}{})
	if _, _, ok := fresh.LoggedIn("seat-a"); ok {
		t.Error("an unlisted credential kept its login instant")
	}
}
