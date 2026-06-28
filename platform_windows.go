//go:build windows

package main

func chmodPrivateDir(_ string) error {
	return nil
}

// Windows access control is inherited from the containing user profile or
// explicitly configured OC_DATA_DIR. Unix mode bits are not an ACL and must
// not be presented as one; os.OpenFile still creates/truncates the file using
// native Windows APIs.
func chmodPrivateFile(_ string) error {
	return nil
}
