package web

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// IDKeyFiler is a Source that names the file holding the key published
// credential ids are derived with. The page keeps per-seat state under the
// published id, so the key has to outlive the process. A Source that does not
// implement it, or names no file, gets a key of the process's own, and its
// published ids change on every restart.
type IDKeyFiler interface {
	// PageIDKeyFile is the key file's path, or "" for none.
	PageIDKeyFile() string
}

// idKeyBytes is the length of the key, the block of randomness HMAC-SHA256
// takes whole.
const idKeyBytes = 32

// idKey is the key published ids are derived with. It is read or created on
// the first status request rather than when the handler is built, which
// happens as the host loads the library.
type idKey struct {
	path string
	once sync.Once
	key  []byte
}

func newIDKey(src Source) *idKey {
	k := &idKey{}
	if f, ok := src.(IDKeyFiler); ok {
		k.path = f.PageIDKeyFile()
	}
	return k
}

func (k *idKey) get() []byte {
	k.once.Do(func() { k.key = loadIDKey(k.path) })
	return k.key
}

// loadIDKey is the key held at path, which is created, or replaced when it
// holds no key, with a fresh random one. A file that cannot be read or written
// leaves the fresh key in memory only: the published ids then change on
// restart, and are still keyed, so no one can check a guessed address against
// them.
func loadIDKey(path string) []byte {
	if path == "" {
		return randomIDKey()
	}
	body, err := os.ReadFile(path)
	if err == nil && len(body) == idKeyBytes {
		return body
	}
	key := randomIDKey()
	if err == nil || errors.Is(err, os.ErrNotExist) {
		_ = writeIDKey(path, key)
	}
	return key
}

func randomIDKey() []byte {
	key := make([]byte, idKeyBytes)
	_, _ = rand.Read(key)
	return key
}

// writeIDKey writes key to path, readable by the owner alone, through a
// sibling file synced and renamed into place so neither a reader nor a crash
// leaves a partial key. The directory is created as needed.
func writeIDKey(path string, key []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(key)
	if werr == nil {
		werr = tmp.Sync()
	}
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
