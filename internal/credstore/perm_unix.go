//go:build unix

package credstore

import (
	"fmt"
	"os"
	"syscall"
)

// Supported reports whether saved passwords are available on this platform.
const Supported = true

const checkPerms = true

func checkOwner(st os.FileInfo) error {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("is owned by uid %d, not by you (uid %d)", sys.Uid, os.Getuid())
	}
	return nil
}
