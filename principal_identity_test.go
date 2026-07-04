package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPrincipalIdentityPersistenceContract(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sdk := &ocSDK{
		principalID: "principal-1",
		cred:        "oc_prn_test",
		pubKeyB64:   base64.RawURLEncoding.EncodeToString(pub),
		privKey:     priv,
		identityDir: t.TempDir(),
	}
	if got := sdk.persistIdentity(); got != "file" {
		t.Fatalf("persistIdentity=%q", got)
	}
	raw, err := os.ReadFile(sdk.identityFilePath())
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	want := map[string]bool{"principal_id": true, "cred": true, "pubkey": true, "privkey": true}
	if len(keys) != len(want) {
		t.Fatalf("identity fields=%v", keys)
	}
	for key := range body {
		if !want[key] {
			t.Fatalf("unexpected identity field %q", key)
		}
	}

	loaded := &ocSDK{identityDir: sdk.identityDir}
	if !loaded.loadPersistedIdentity() {
		t.Fatal("loadPersistedIdentity=false")
	}
	if loaded.principalID != sdk.principalID || loaded.cred != sdk.cred ||
		loaded.pubKeyB64 != sdk.pubKeyB64 || !reflect.DeepEqual(loaded.privKey, sdk.privKey) {
		t.Fatal("loaded identity differs")
	}
	if mode := fileMode(t, sdk.identityFilePath()); mode != 0o600 {
		t.Fatalf("identity mode=%o", mode)
	}
}

func TestShowIdentityReturnsOnlyPrincipalFields(t *testing.T) {
	mock := newMockAPI(nil)
	server := httptest.NewServer(mock)
	defer server.Close()
	sdk := newTestSDK(server)
	sdk.storageBackend = "file"
	sdk.identityDir = t.TempDir()

	result, err := sdk.handleShowIdentity(context.Background(), makeCallReq(nil))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(toolResultText(result)), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"principal_id":       true,
		"pubkey_fp":          true,
		"balance_dcents":     true,
		"storage_backend":    true,
		"identity_file_path": true,
	}
	if len(body) != len(want) {
		t.Fatalf("show_identity fields=%v", body)
	}
	for key := range body {
		if !want[key] {
			t.Fatalf("unexpected show_identity field %q", key)
		}
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
