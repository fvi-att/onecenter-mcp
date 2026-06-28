//go:build !windows

package main

import "os"

func chmodPrivateDir(path string) error {
	return os.Chmod(path, 0700)
}

// Unix permissions are process-independent, so enforce the documented mode
// even when an existing file had broader permissions.
func chmodPrivateFile(path string) error {
	return os.Chmod(path, 0600)
}
