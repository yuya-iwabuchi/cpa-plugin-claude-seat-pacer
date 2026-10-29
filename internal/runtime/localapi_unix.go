//go:build !windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// privateDir makes dir the user's alone: it must be a real directory, not a
// link, owned by the process's user, and loses any group or other permission
// it has.
func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect the local API socket's directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("the local API socket's directory is not a directory")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return errors.New("the local API socket's directory belongs to another user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, info.Mode().Perm()&0o700); err != nil {
			return fmt.Errorf("restrict the local API socket's directory: %w", err)
		}
	}
	return nil
}
