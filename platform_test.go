package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPlatformDataDirOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "onecenter state")
	t.Setenv(dataDirEnv, want)
	t.Setenv(identityDirEnv, "")

	got, err := oneCenterDataDir()
	if err != nil {
		t.Fatalf("oneCenterDataDir: %v", err)
	}
	absolute, err := filepath.Abs(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(absolute) {
		t.Fatalf("data dir: got %q, want %q", got, filepath.Clean(absolute))
	}

	sdk := &ocSDK{agentID: "seller-1"}
	if got := sdk.identityFilePath(); got != filepath.Join(absolute, "mcp", "identity.json") {
		t.Fatalf("identity path: %q", got)
	}
	if got := sdk.p2pFilePath("agreements", "seller", "call-1"); got != filepath.Join(absolute, "p2p", "seller-1", "agreements", "seller", "call-1.json") {
		t.Fatalf("p2p path: %q", got)
	}
	if got := sdk.demandFilePath("demand-1"); got != filepath.Join(absolute, "demand", "demand-1.json") {
		t.Fatalf("demand path: %q", got)
	}
}

func TestPlatformIdentityDirCompatibility(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "all-data")
	identityDir := filepath.Join(t.TempDir(), "identity-only")
	t.Setenv(dataDirEnv, dataDir)
	t.Setenv(identityDirEnv, identityDir)

	sdk := &ocSDK{agentID: "seller-1"}
	if got := sdk.identityFilePath(); got != filepath.Join(identityDir, "mcp", "identity.json") {
		t.Fatalf("identity override: %q", got)
	}
	if got := sdk.p2pFilePath("quotations", "sent", "q-1"); got != filepath.Join(dataDir, "p2p", "seller-1", "quotations", "sent", "q-1.json") {
		t.Fatalf("identity override leaked into p2p path: %q", got)
	}
}

func TestWritePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "identity.json")
	if err := writePrivateFile(path, []byte("first")); err != nil {
		t.Fatalf("create private file: %v", err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatalf("broaden mode: %v", err)
	}
	if err := writePrivateFile(path, []byte("second")); err != nil {
		t.Fatalf("rewrite private file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("content: %q", data)
	}
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0700 {
			t.Fatalf("directory mode: got %04o, want 0700", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("mode: got %04o, want 0600", got)
		}
	}
}
