// platform.go centralizes the host-OS boundary for local MCP state.
//
// filepath and os.UserHomeDir intentionally provide the platform-specific
// separator and home-directory behavior. Callers must not concatenate paths
// or inspect HOME/USERPROFILE directly.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	dataDirEnv     = "OC_DATA_DIR"
	identityDirEnv = "OC_IDENTITY_DIR" // compatibility with the identity-only override
)

// oneCenterDataDir resolves the shared local state root. OC_DATA_DIR is useful
// for containers, CI, and Windows installations where the default home is not
// appropriate. Relative overrides are made absolute once, at process startup.
func oneCenterDataDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv(dataDirEnv)); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", dataDirEnv, err)
		}
		return filepath.Clean(absolute), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home (set %s to override): %w", dataDirEnv, err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("user home is empty (set %s to override)", dataDirEnv)
	}
	return filepath.Join(home, ".onecenter"), nil
}

func mustOneCenterDataDir() string {
	dir, err := oneCenterDataDir()
	if err != nil {
		panic(err)
	}
	return dir
}

// identityBaseDir preserves the existing test injection and
// OC_IDENTITY_DIR behavior while allowing OC_DATA_DIR to configure all local
// MCP state through one cross-platform root.
func identityBaseDir(injected string) string {
	if strings.TrimSpace(injected) != "" {
		return filepath.Clean(injected)
	}
	if configured := strings.TrimSpace(os.Getenv(identityDirEnv)); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			panic(fmt.Errorf("resolve %s: %w", identityDirEnv, err))
		}
		return filepath.Clean(absolute)
	}
	return mustOneCenterDataDir()
}

// writePrivateFile is the only creation path for credentials, private keys,
// agreements, quotations, and local demand records. Go maps filepath and file
// creation semantics to the host OS; chmodPrivateFile handles the remaining
// Unix/Windows permission difference.
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := chmodPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return chmodPrivateFile(path)
}
