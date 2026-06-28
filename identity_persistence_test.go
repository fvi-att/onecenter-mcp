// identity_persistence_test.go — v2-r23: MCP identity 永続化の統合テスト
//
// 設計根拠:
//   _prelude.lisp *mcp-identity-persistence-spec* / *mcp-key-management-tools*
//   design/20260607_mcp_client_server_dsl.lisp SECTION 8 (v2-mcp-r7)
//   flow mcp-identity-first-run (F1-F5) / flow mcp-identity-restart (R1-R4)
//   *mcp-show-identity-tool* / *mcp-regenerate-identity-tool*
//
// 受け入れ観点:
//   I1: persistIdentity() → identity.json (0600) + keypair files (0600) が作成される
//   I2: loadPersistedIdentity() → identity.json から読み込み、selfRegister はスキップされる
//   I3: identity.json は存在するが keypair file が欠損 → first-run にフォールスルー (false を返す)
//   S1: show_identity → file mode で agent_id / pubkey fingerprint / storage_backend / đ 残高 を返す
//   S2: show_identity → ephemeral mode で storage_backend=ephemeral + note を返す
//   R7: regenerate_identity confirm=false → dry-run (実行なし; 残高 + 警告を返す)
//   R8: regenerate_identity confirm=true → 実行 + identity.json + keypair file 更新

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
	"testing"
	"time"
)

// newTestSDKWithDir — identityDir を指定した ocSDK (テスト用)
func newTestSDKWithDir(ts *httptest.Server, dir string) *ocSDK {
	sdk := newTestSDK(ts)
	sdk.identityDir = dir
	return sdk
}

// ── I1: persistIdentity() — first-run でファイルが作成される ────────────────

func TestIdentityPersistence_I1_PersistCreatesFiles(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	dir := t.TempDir()

	sdk := newTestSDKWithDir(ts, dir)

	backend := sdk.persistIdentity()
	if backend != "file" {
		t.Fatalf("I1: expected 'file' backend, got %q", backend)
	}

	// identity.json が作成されたこと
	idPath := sdk.identityFilePath()
	data, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatalf("I1: identity.json not created: %v", err)
	}

	// パーミッション 0600
	info, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("I1: stat identity.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("I1: identity.json perm should be 0600, got %04o", info.Mode().Perm())
	}

	// スキーマ検証
	var cfg identityConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("I1: identity.json not valid JSON: %v", err)
	}
	if cfg.SellerAgentID != sdk.agentID {
		t.Errorf("I1: seller_agent_id mismatch: %s vs %s", cfg.SellerAgentID, sdk.agentID)
	}
	if cfg.BuyerAgentID != sdk.buyerAgentID {
		t.Errorf("I1: buyer_agent_id mismatch: %s vs %s", cfg.BuyerAgentID, sdk.buyerAgentID)
	}
	if cfg.SellerCred != sdk.apiKey {
		t.Errorf("I1: seller_cred mismatch")
	}
	if cfg.CreatedAt == "" {
		t.Errorf("I1: created_at should be set")
	}

	// seller keypair file が作成されたこと
	sellerKeyPath := sdk.mcpKeyFilePath("seller", sdk.agentID)
	if _, err := os.Stat(sellerKeyPath); err != nil {
		t.Errorf("I1: seller keypair file not created: %v", err)
	}
	if ki, err := os.Stat(sellerKeyPath); err == nil && ki.Mode().Perm() != 0600 {
		t.Errorf("I1: seller keypair perm should be 0600, got %04o", ki.Mode().Perm())
	}

	// buyer keypair file が作成されたこと
	buyerKeyPath := sdk.mcpKeyFilePath("buyer", sdk.buyerAgentID)
	if _, err := os.Stat(buyerKeyPath); err != nil {
		t.Errorf("I1: buyer keypair file not created: %v", err)
	}
}

// ── I2: loadPersistedIdentity() — restart でファイルから identity をロードする ─

func TestIdentityPersistence_I2_RestartLoadsIdentity(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	dir := t.TempDir()

	// first-run: ファイルを作成
	sdk1 := newTestSDKWithDir(ts, dir)
	if backend := sdk1.persistIdentity(); backend != "file" {
		t.Fatalf("I2 setup: persistIdentity returned %q", backend)
	}

	// selfRegister 呼び出しカウンタを記録
	registrationsBefore := len(mock.registeredRoles)

	// restart: 新しい SDK で同一ディレクトリからロード
	sdk2 := &ocSDK{
		oncenterURL:     ts.URL,
		client:          ts.Client(),
		identityDir:     dir,
		storageBackend:  "ephemeral",
		localQuotations: make(map[string]map[string]any),
		seenQIDs:        make(map[string]struct{}),
		localAgreements: make(map[string]map[string]any),
		seenCallIDs:     make(map[string]struct{}),
	}
	loaded := sdk2.loadPersistedIdentity()
	if !loaded {
		t.Fatalf("I2: expected loadPersistedIdentity to return true")
	}

	// 同じ agent_id がロードされたこと
	if sdk2.agentID != sdk1.agentID {
		t.Errorf("I2: seller agent_id mismatch: got %s, want %s", sdk2.agentID, sdk1.agentID)
	}
	if sdk2.buyerAgentID != sdk1.buyerAgentID {
		t.Errorf("I2: buyer agent_id mismatch: got %s, want %s", sdk2.buyerAgentID, sdk1.buyerAgentID)
	}
	if sdk2.apiKey != sdk1.apiKey {
		t.Errorf("I2: seller cred mismatch")
	}
	if sdk2.buyerCred != sdk1.buyerCred {
		t.Errorf("I2: buyer cred mismatch")
	}

	// selfRegister が呼ばれていないこと
	if len(mock.registeredRoles) != registrationsBefore {
		t.Errorf("I2: selfRegister should not be called on restart; got %d new registrations",
			len(mock.registeredRoles)-registrationsBefore)
	}

	// storageBackend が "file" になること
	if sdk2.storageBackend != "file" {
		t.Errorf("I2: storageBackend should be 'file', got %q", sdk2.storageBackend)
	}

	// keypair が正しくロードされ、同一秘密鍵で同一署名が作られること
	testMsg := []byte("v2-r23 identity persistence test")
	sig1 := ed25519.Sign(sdk1.privKey, testMsg)
	sig2 := ed25519.Sign(sdk2.privKey, testMsg)
	if string(sig1) != string(sig2) {
		t.Errorf("I2: seller keypair should be identical after load")
	}
	bsig1 := ed25519.Sign(sdk1.buyerPrivKey, testMsg)
	bsig2 := ed25519.Sign(sdk2.buyerPrivKey, testMsg)
	if string(bsig1) != string(bsig2) {
		t.Errorf("I2: buyer keypair should be identical after load")
	}
}

// ── I3: keypair file 欠損 → first-run にフォールスルー ──────────────────────

func TestIdentityPersistence_I3_KeypairMissing_FallsThrough(t *testing.T) {
	dir := t.TempDir()

	// identity.json だけ作成 (keypair files なし)
	cfg := &identityConfig{
		SellerAgentID: "seller-i3-001",
		SellerCred:    "cred-seller-i3",
		BuyerAgentID:  "buyer-i3-001",
		BuyerCred:     "cred-buyer-i3",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	idPath := filepath.Join(dir, "mcp", "identity.json")
	if err := os.MkdirAll(filepath.Dir(idPath), 0700); err != nil {
		t.Fatalf("I3 setup: %v", err)
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(idPath, data, 0600); err != nil {
		t.Fatalf("I3 setup: %v", err)
	}

	sdk := &ocSDK{
		identityDir:     dir,
		storageBackend:  "ephemeral",
		localQuotations: make(map[string]map[string]any),
		seenQIDs:        make(map[string]struct{}),
		localAgreements: make(map[string]map[string]any),
		seenCallIDs:     make(map[string]struct{}),
	}
	loaded := sdk.loadPersistedIdentity()
	if loaded {
		t.Errorf("I3: expected false (first-run fallback) when keypair files are missing, got true")
	}
	// agentID は変更されていないこと (sdk の agentID はまだ空)
	if sdk.agentID != "" {
		t.Errorf("I3: agentID should not be set on fallback, got %q", sdk.agentID)
	}
}

// ── I4: identity.json が存在しない → false を返す ───────────────────────────

func TestIdentityPersistence_I4_NoFile_ReturnsFalse(t *testing.T) {
	dir := t.TempDir() // 空ディレクトリ (identity.json なし)
	sdk := &ocSDK{
		identityDir:     dir,
		storageBackend:  "ephemeral",
		localQuotations: make(map[string]map[string]any),
		seenQIDs:        make(map[string]struct{}),
		localAgreements: make(map[string]map[string]any),
		seenCallIDs:     make(map[string]struct{}),
	}
	if sdk.loadPersistedIdentity() {
		t.Errorf("I4: expected false when identity.json does not exist")
	}
}

// ── S1: show_identity — file モード ──────────────────────────────────────────

func TestShowIdentity_S1_FileMode(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	dir := t.TempDir()

	sdk := newTestSDKWithDir(ts, dir)
	sdk.storageBackend = "file"

	result, err := sdk.handleShowIdentity(context.Background(), makeCallReq(nil))
	if err != nil {
		t.Fatalf("S1: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("S1: unexpected error: %s", toolResultText(result))
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("S1: invalid JSON: %v", err)
	}

	if out["seller_agent_id"] != sdk.agentID {
		t.Errorf("S1: seller_agent_id mismatch: %v vs %s", out["seller_agent_id"], sdk.agentID)
	}
	if out["buyer_agent_id"] != sdk.buyerAgentID {
		t.Errorf("S1: buyer_agent_id mismatch: %v vs %s", out["buyer_agent_id"], sdk.buyerAgentID)
	}
	if out["storage_backend"] != "file" {
		t.Errorf("S1: expected storage_backend=file, got %v", out["storage_backend"])
	}
	if _, ok := out["seller_pubkey_fp"]; !ok {
		t.Errorf("S1: missing seller_pubkey_fp")
	}
	if _, ok := out["buyer_pubkey_fp"]; !ok {
		t.Errorf("S1: missing buyer_pubkey_fp")
	}
	if _, ok := out["identity_file_path"]; !ok {
		t.Errorf("S1: missing identity_file_path")
	}
	// file mode では note フィールドが不要
	if _, ok := out["note"]; ok {
		t.Errorf("S1: note should not appear in file mode, got: %v", out["note"])
	}

	// pubkey fingerprint が 8 文字以下
	if fp, ok := out["seller_pubkey_fp"].(string); ok && len(fp) > 8 {
		t.Errorf("S1: seller_pubkey_fp should be ≤8 chars, got %q", fp)
	}
}

// ── S2: show_identity — ephemeral モードは note を返す ───────────────────────

func TestShowIdentity_S2_EphemeralMode_HasNote(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()

	sdk := newTestSDK(ts)
	sdk.storageBackend = "ephemeral"

	result, err := sdk.handleShowIdentity(context.Background(), makeCallReq(nil))
	if err != nil {
		t.Fatalf("S2: %v", err)
	}
	var out map[string]any
	json.Unmarshal([]byte(toolResultText(result)), &out)

	if out["storage_backend"] != "ephemeral" {
		t.Errorf("S2: expected storage_backend=ephemeral, got %v", out["storage_backend"])
	}
	if note, ok := out["note"].(string); !ok || note == "" {
		t.Errorf("S2: ephemeral mode should include non-empty note, got %v", out["note"])
	}
}

// ── S3: show_identity — storageBackend 未設定は ephemeral 扱い ───────────────

func TestShowIdentity_S3_EmptyBackend_TreatedAsEphemeral(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()

	sdk := newTestSDK(ts)
	sdk.storageBackend = "" // 未設定

	result, _ := sdk.handleShowIdentity(context.Background(), makeCallReq(nil))
	var out map[string]any
	json.Unmarshal([]byte(toolResultText(result)), &out)

	if out["storage_backend"] != "ephemeral" {
		t.Errorf("S3: empty storageBackend should show as ephemeral, got %v", out["storage_backend"])
	}
	if _, ok := out["note"]; !ok {
		t.Errorf("S3: ephemeral mode should have note field")
	}
}

// ── R7: regenerate_identity confirm=false → dry-run ────────────────────────

func TestRegenerateIdentity_R7_DryRun(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID
	oldSeller := sdk.agentID

	// confirm=false (明示的 false)
	req := makeCallReq(map[string]any{"role": "buyer", "confirm": false})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R7: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("R7: unexpected error: %s", toolResultText(result))
	}

	text := toolResultText(result)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("R7: invalid JSON: %s", text)
	}
	if out["dry_run"] != true {
		t.Errorf("R7: expected dry_run=true: %s", text)
	}
	if _, ok := out["warning"]; !ok {
		t.Errorf("R7: expected warning field: %s", text)
	}
	if _, ok := out["current_buyer_balance_dcents"]; !ok {
		t.Errorf("R7: expected current_buyer_balance_dcents: %s", text)
	}

	// identity は変更されていないこと
	if sdk.buyerAgentID != oldBuyer || sdk.agentID != oldSeller {
		t.Errorf("R7: dry-run must not change identity (buyer=%s seller=%s)", sdk.buyerAgentID, sdk.agentID)
	}
	// selfRegister が呼ばれていないこと
	if len(mock.registeredRoles) > 0 {
		t.Errorf("R7: dry-run must not call selfRegister, got %v", mock.registeredRoles)
	}
}

// ── R7b: confirm 未指定 (nil) → dry-run ────────────────────────────────────

func TestRegenerateIdentity_R7b_NoConfirm_IsDryRun(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID

	// confirm キーなし
	req := makeCallReq(map[string]any{"role": "buyer"})
	result, _ := sdk.handleRegenerateIdentity(context.Background(), req)

	var out map[string]any
	json.Unmarshal([]byte(toolResultText(result)), &out)
	if out["dry_run"] != true {
		t.Errorf("R7b: omitting confirm should be dry-run, got: %s", toolResultText(result))
	}
	if sdk.buyerAgentID != oldBuyer {
		t.Errorf("R7b: identity must not change without confirm=true")
	}
}

// ── R8: regenerate_identity confirm=true → 実行 + identity.json + keypair 更新 ─

func TestRegenerateIdentity_R8_ConfirmTrue_PersistsFiles(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	dir := t.TempDir()

	sdk := newTestSDKWithDir(ts, dir)
	sdk.storageBackend = "file"

	// 初期 identity を永続化しておく (created_at 引き継ぎのテスト用)
	sdk.persistIdentity()
	oldBuyer := sdk.buyerAgentID

	req := makeCallReq(map[string]any{"role": "buyer", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R8: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("R8: unexpected error: %s", toolResultText(result))
	}

	// buyer agent_id が変わったこと
	if sdk.buyerAgentID == oldBuyer {
		t.Errorf("R8: buyer agent_id should change with confirm=true")
	}

	// identity.json が更新されたこと
	idPath := sdk.identityFilePath()
	data, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatalf("R8: identity.json not found after regenerate: %v", err)
	}
	var cfg identityConfig
	json.Unmarshal(data, &cfg)
	if cfg.BuyerAgentID != sdk.buyerAgentID {
		t.Errorf("R8: identity.json buyer_agent_id not updated: %s vs %s", cfg.BuyerAgentID, sdk.buyerAgentID)
	}
	// created_at が引き継がれ、updated_at が新しくなっていること
	if cfg.CreatedAt == "" {
		t.Errorf("R8: created_at should be preserved")
	}
	if cfg.UpdatedAt == "" {
		t.Errorf("R8: updated_at should be set after regenerate")
	}

	// 新しい buyer keypair file が作成されたこと
	buyerKeyPath := sdk.mcpKeyFilePath("buyer", sdk.buyerAgentID)
	if _, err := os.Stat(buyerKeyPath); err != nil {
		t.Errorf("R8: buyer keypair file not created: %v", err)
	}

	// レスポンスに note と storage_backend が含まれること
	text := toolResultText(result)
	var out map[string]any
	json.Unmarshal([]byte(text), &out)
	if _, ok := out["note"]; !ok {
		t.Errorf("R8: response should contain note: %s", text)
	}
	if out["storage_backend"] != "file" {
		t.Errorf("R8: storage_backend should be 'file': %s", text)
	}
}

// ── ヘルパー単体テスト: saveMCPKey / loadMCPKey ──────────────────────────────

func TestMCPKey_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	path := filepath.Join(dir, "test-seller.key")
	if err := saveMCPKey(path, priv); err != nil {
		t.Fatalf("saveMCPKey: %v", err)
	}

	// 0600 パーミッション
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0600 {
		t.Errorf("keypair perm should be 0600, got %04o", info.Mode().Perm())
	}

	loaded, err := loadMCPKey(path)
	if err != nil {
		t.Fatalf("loadMCPKey: %v", err)
	}

	// ロードした keypair で同一署名が作れること
	testMsg := []byte("key round-trip test")
	sig1 := ed25519.Sign(priv, testMsg)
	sig2 := ed25519.Sign(loaded, testMsg)
	if string(sig1) != string(sig2) {
		t.Errorf("loaded keypair produces different signature")
	}

	// 公開鍵も一致すること
	origPubB64 := base64.RawURLEncoding.EncodeToString(pub)
	loadedPubB64 := base64.RawURLEncoding.EncodeToString(loaded.Public().(ed25519.PublicKey))
	if origPubB64 != loadedPubB64 {
		t.Errorf("public key mismatch after load")
	}
}

func TestMCPKey_InvalidPEM_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")
	os.WriteFile(path, []byte("not a PEM"), 0600)

	_, err := loadMCPKey(path)
	if err == nil {
		t.Errorf("expected error for invalid PEM")
	}
}

// ── ヘルパー単体テスト: saveIdentityConfig / loadIdentityConfig ────────────

func TestIdentityConfig_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")

	cfg := &identityConfig{
		SellerAgentID: "seller-test-001",
		SellerCred:    "sc-001",
		BuyerAgentID:  "buyer-test-001",
		BuyerCred:     "bc-001",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveIdentityConfig(path, cfg); err != nil {
		t.Fatalf("saveIdentityConfig: %v", err)
	}

	// 0600 パーミッション
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0600 {
		t.Errorf("identity.json perm should be 0600, got %04o", info.Mode().Perm())
	}
	// updated_at が設定されていること
	if cfg.UpdatedAt == "" {
		t.Errorf("saveIdentityConfig should set UpdatedAt")
	}

	loaded, err := loadIdentityConfig(path)
	if err != nil {
		t.Fatalf("loadIdentityConfig: %v", err)
	}
	if loaded.SellerAgentID != cfg.SellerAgentID {
		t.Errorf("SellerAgentID mismatch: %s vs %s", loaded.SellerAgentID, cfg.SellerAgentID)
	}
	if loaded.BuyerAgentID != cfg.BuyerAgentID {
		t.Errorf("BuyerAgentID mismatch")
	}
}

func TestIdentityConfig_NotFound_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := loadIdentityConfig(filepath.Join(dir, "no-such-file.json"))
	if err == nil {
		t.Errorf("expected error for missing file")
	}
}
