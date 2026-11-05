//go:build !unix

package credstore

import "os"

// Supported reports whether saved passwords are available on this platform.
// Without Unix permission bits and owners (e.g. on Windows, where access is
// governed by ACLs) the file cannot be checked to be private, so saving and
// loading passwords is disabled.
const Supported = false

const checkPerms = false

func checkOwner(os.FileInfo) error { return nil }
