//go:build windows

package runtime

import "errors"

// privateDir refuses every directory: Windows serves no local API socket.
func privateDir(string) error {
	return errors.New("the local API socket is served on macOS and Linux only")
}
