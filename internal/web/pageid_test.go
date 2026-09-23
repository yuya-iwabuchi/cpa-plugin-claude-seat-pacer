package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// keyedSource is a Source that keeps its id key in keyFile.
type keyedSource struct {
	stubSource
	keyFile string
}

func (s *keyedSource) PageIDKeyFile() string { return s.keyFile }

// servedIDs is the published id of every credential row a handler serves.
func servedIDs(t *testing.T, src Source) []string {
	t.Helper()
	rec := get(t, NewHandler(src), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	ids := make([]string, len(got.Auths))
	for i, a := range got.Auths {
		ids[i] = a.AuthID
	}
	return ids
}

// unkeyedID is the digest anyone can compute from a guessed credential id.
func unkeyedID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:publicIDBytes])
}

// TestPublishedIDsAreKeyed covers the guess a published id must not confirm:
// the real id is routinely an account address, so a digest of it alone is
// checkable against a staff list.
func TestPublishedIDsAreKeyed(t *testing.T) {
	t.Parallel()
	if got, want := publicID(testIDKey, seatAID), "e3cd3a634e95150f"; got != want {
		t.Errorf("publicID(testIDKey, %q) = %q, want the HMAC-SHA256 prefix %q", seatAID, got, want)
	}
	other := publicID([]byte(strings.Repeat("k", idKeyBytes)), seatAID)
	for _, id := range []string{testID(seatAID), other} {
		if id == unkeyedID(seatAID) {
			t.Errorf("published id %q is the unkeyed digest of the real id", id)
		}
	}
	if other == testID(seatAID) {
		t.Errorf("two keys publish one id %q", other)
	}
}

// TestPublishedIDsAreStable covers the operator watching the page, whose
// per-seat state the page keeps under the published id: one seat reads the
// same across polls, and across the restart that builds a new handler over the
// same key file.
func TestPublishedIDsAreStable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "page-id.key")
	src := &keyedSource{stubSource: stubSource{status: idStatus()}, keyFile: path}
	h := NewHandler(src)
	first := get(t, h, "/api/status").Body.String()
	if again := get(t, h, "/api/status").Body.String(); again != first {
		t.Errorf("two polls of one state differ\nfirst: %s\nagain: %s", first, again)
	}
	if restarted := get(t, NewHandler(src), "/api/status").Body.String(); restarted != first {
		t.Errorf("a restart over the same key file changed the ids\nbefore: %s\nafter:  %s", first, restarted)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %v, want 0600", mode)
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != idKeyBytes {
		t.Fatalf("key file holds %d bytes (%v), want %d", len(key), err, idKeyBytes)
	}
	if got, want := servedIDs(t, src)[0], publicID(key, seatAID); got != want {
		t.Errorf("served id = %q, want it keyed with the file's key, %q", got, want)
	}
}

// TestIDKeyFileIsReplacedWhenItHoldsNoKey covers a key file cut short: it is
// rewritten with a whole key, which the next start reads back.
func TestIDKeyFileIsReplacedWhenItHoldsNoKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "page-id.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &keyedSource{stubSource: stubSource{status: idStatus()}, keyFile: path}
	first := servedIDs(t, src)
	key, err := os.ReadFile(path)
	if err != nil || len(key) != idKeyBytes {
		t.Fatalf("key file holds %d bytes (%v), want %d", len(key), err, idKeyBytes)
	}
	if again := servedIDs(t, src); again[0] != first[0] {
		t.Errorf("ids changed across a restart after the file was replaced: %q then %q", first[0], again[0])
	}
}

// TestIDKeyFallsBackToAProcessKey covers a key that cannot be kept: each
// handler holds a random key of its own, so ids change on restart and are
// still no digest of the real id.
func TestIDKeyFallsBackToAProcessKey(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]Source{
		"unwritable": &keyedSource{stubSource: stubSource{status: idStatus()}, keyFile: filepath.Join(blocker, "page-id.key")},
		"no file":    &keyedSource{stubSource: stubSource{status: idStatus()}},
		"no filer":   &stubSource{status: idStatus()},
	} {
		first, second := servedIDs(t, src)[0], servedIDs(t, src)[0]
		if first == unkeyedID(seatAID) || second == unkeyedID(seatAID) {
			t.Errorf("%s: a published id is the unkeyed digest of the real id", name)
		}
		if first == second {
			t.Errorf("%s: two processes publish one id %q with no key file between them", name, first)
		}
	}
}

// TestIDKeyFileThatCannotBeReadIsLeftAlone covers a key file the process may
// not read: the fallback key stays in memory and the file is not overwritten.
func TestIDKeyFileThatCannotBeReadIsLeftAlone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "page-id.key")
	want := []byte(strings.Repeat("x", idKeyBytes))
	if err := os.WriteFile(path, want, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("the file mode does not stop this user reading it")
	}
	src := &keyedSource{stubSource: stubSource{status: idStatus()}, keyFile: path}
	if got := servedIDs(t, src)[0]; got == publicID(want, seatAID) || got == unkeyedID(seatAID) {
		t.Errorf("served id %q is derived from a key the process could not read", got)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != string(want) {
		t.Errorf("the unreadable key file was rewritten: %q (%v)", body, err)
	}
}
