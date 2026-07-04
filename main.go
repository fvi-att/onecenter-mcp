// cmd/mcp/main.go — OneCenter MCP stdio server (Go)
// Claude Code が MCP client として起動・接続する形式 (OneCenter MCP stdio server; Buyer 汎用 client)
// Design: v2/design/20260607_mcp_client_server_dsl.lisp *mcp-spec* / v2/design/20260610_buyer_tools_dsl.lisp
//
// Usage (.mcp.json): command="go" args=["run", "./cmd/mcp"]
// Env:
//   OC_PRINCIPAL_ID     — Principal ID for credential injection mode
//   OC_PRINCIPAL_CRED   — Principal CRED
//   OC_CAPABILITY_ID    — 登録済み capability UUID
//   ONECENTER_URL       — OneCenter API ベース URL (default: http://localhost:8080)

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"
)

// identityConfig is the complete on-disk identity contract. The private key is
// encoded as a base64url Ed25519 seed and the file itself is written mode 0600.
type identityConfig struct {
	PrincipalID string `json:"principal_id"`
	Cred        string `json:"cred"`
	Pubkey      string `json:"pubkey"`
	Privkey     string `json:"privkey"`
}

type ocSDK struct {
	principalID    string
	cred           string
	capabilityID   string
	oncenterURL    string
	sessionID      string
	privKey        ed25519.PrivateKey
	pubKeyB64      string
	client         *http.Client
	idmu           sync.RWMutex
	storageBackend string
	identityDir    string
	demandBaseDir  string
}

func newOcSDK() *ocSDK {
	baseURL := getenv("ONECENTER_URL", "http://localhost:8080")
	client := &http.Client{Timeout: 10 * time.Second}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("failed to generate Ed25519 keypair: " + err.Error())
	}
	sdk := &ocSDK{
		capabilityID:   getenv("OC_CAPABILITY_ID", "00000000-0000-0000-0000-000000000001"),
		oncenterURL:    baseURL,
		sessionID:      "claude-code-stdio-session",
		privKey:        priv,
		pubKeyB64:      base64.RawURLEncoding.EncodeToString(pub),
		client:         client,
		storageBackend: "ephemeral",
	}
	if cred := getenv("OC_PRINCIPAL_CRED", getenv("OC_API_KEY", "")); cred != "" {
		sdk.cred = cred
		sdk.principalID = getenv("OC_PRINCIPAL_ID", "")
		return sdk
	}
	if sdk.loadPersistedIdentity() {
		return sdk
	}
	cred, principalID := registerPrincipal(client, baseURL, sdk.pubKeyB64, "onecenter-mcp")
	if principalID == "" {
		return sdk
	}
	sdk.cred = cred
	sdk.principalID = principalID
	sdk.storageBackend = sdk.persistIdentity()
	return sdk
}

// registerPrincipal performs the one-step identity flow: key generation happens
// locally and POST /auth/signup atomically stores the public key on Principal.
func registerPrincipal(client *http.Client, baseURL, pubkey, name string) (cred, principalID string) {
	body, _ := json.Marshal(map[string]any{
		"type":   "individual",
		"name":   name,
		"email":  name + "@onecenter.local",
		"pubkey": pubkey,
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/auth/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] principal signup failed: %v\n", err)
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "[oc-sdk] principal signup failed (%d): %s\n", resp.StatusCode, raw)
		return "", ""
	}
	var result struct {
		ID   string `json:"id"`
		Cred string `json:"cred"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return "", ""
	}
	fmt.Fprintf(os.Stderr, "[oc-sdk] registered principal_id=%s\n", result.ID)
	return result.Cred, result.ID
}

func (s *ocSDK) identityFilePath() string {
	return filepath.Join(identityBaseDir(s.identityDir), "mcp", "identity.json")
}

func loadIdentityConfig(path string) (*identityConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg identityConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.PrincipalID == "" || cfg.Cred == "" || cfg.Pubkey == "" || cfg.Privkey == "" {
		return nil, fmt.Errorf("incomplete principal identity")
	}
	return &cfg, nil
}

func saveIdentityConfig(path string, cfg *identityConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

func (s *ocSDK) persistIdentity() string {
	cfg := &identityConfig{
		PrincipalID: s.principalID,
		Cred:        s.cred,
		Pubkey:      s.pubKeyB64,
		Privkey:     base64.RawURLEncoding.EncodeToString(s.privKey.Seed()),
	}
	if err := saveIdentityConfig(s.identityFilePath(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: identity persistence failed: %v\n", err)
		return "ephemeral"
	}
	return "file"
}

func (s *ocSDK) loadPersistedIdentity() bool {
	cfg, err := loadIdentityConfig(s.identityFilePath())
	if err != nil {
		return false
	}
	seed, err := base64.RawURLEncoding.DecodeString(cfg.Privkey)
	if err != nil || len(seed) != ed25519.SeedSize {
		return false
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if pub != cfg.Pubkey {
		return false
	}
	s.principalID = cfg.PrincipalID
	s.cred = cfg.Cred
	s.pubKeyB64 = cfg.Pubkey
	s.privKey = priv
	s.storageBackend = "file"
	return true
}

func (s *ocSDK) regenerateIdentity(_ string) (map[string]any, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pubkey := base64.RawURLEncoding.EncodeToString(pub)
	cred, principalID := registerPrincipal(s.client, s.oncenterURL, pubkey, "onecenter-mcp")
	if principalID == "" {
		return nil, fmt.Errorf("principal signup failed")
	}
	s.idmu.Lock()
	s.principalID = principalID
	s.cred = cred
	s.privKey = priv
	s.pubKeyB64 = pubkey
	s.storageBackend = s.persistIdentity()
	s.idmu.Unlock()
	return map[string]any{
		"principal_id": principalID,
		"pubkey_fp":    pubkey[:min(8, len(pubkey))],
	}, nil
}

// getWalletBalance — GET /dcent/wallets/:principal_id → balance_dcents
func (s *ocSDK) getWalletBalance(ctx context.Context, principalID, cred string) int64 {
	if principalID == "" {
		return 0
	}
	walletURL := fmt.Sprintf("%s/dcent/wallets/%s", s.oncenterURL, principalID)
	req, _ := http.NewRequestWithContext(ctx, "GET", walletURL, nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var w struct {
		BalanceDcents int64 `json:"balance_dcents"`
	}
	json.NewDecoder(resp.Body).Decode(&w)
	return w.BalanceDcents
}

// transferDcent — POST /dcent/transfers (v2-r27 simple transfer).
// sender は Buyer Principal CRED で確定する。
func (s *ocSDK) transferDcent(ctx context.Context, toPrincipalID string, dcents int64, memo, transferID string) (map[string]any, int, error) {
	if strings.TrimSpace(transferID) == "" {
		transferID = uuid.NewString()
	}
	body, _ := json.Marshal(map[string]any{
		"transfer_id":     transferID,
		"to_principal_id": toPrincipalID,
		"dcents":          dcents,
		"memo":            memo,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/dcent/transfers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cred)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &result)
	}
	if result == nil {
		result = map[string]any{}
	}
	return result, resp.StatusCode, nil
}

func hashStr(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func wordCountResult(text string) string {
	words := len(strings.Fields(text))
	sentences := len(strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?'
	}))
	characters := len(text)
	return fmt.Sprintf("words: %d, sentences: %d, characters: %d", words, sentences, characters)
}

func echoResult(text, prefix string) string {
	if prefix == "" {
		prefix = "[echo]"
	}
	return prefix + " " + text
}

func (s *ocSDK) getBillingSummaryText(ctx context.Context) string {
	balance := s.getWalletBalance(ctx, s.principalID, s.cred)
	return fmt.Sprintf("Billing for principal %s:\n  đ balance: %dđ", s.principalID, balance)
}

// ── first-party dispatcher (T3: @onecenter/operator.* を直接実行) ──

// dispatchFirstParty — capability name からハンドラを選んで直接実行する。
// 計算のみ実行し、決済は呼び出し元 (handleCallCapability) が担う。
func (s *ocSDK) dispatchFirstParty(ctx context.Context, toolName string, input map[string]any) (string, int64, error) {
	start := time.Now()
	switch toolName {
	case "word_count":
		text, _ := input["text"].(string)
		return wordCountResult(text), time.Since(start).Milliseconds(), nil
	case "echo_text":
		text, _ := input["text"].(string)
		prefix, _ := input["prefix"].(string)
		return echoResult(text, prefix), time.Since(start).Milliseconds(), nil
	case "billing_summary":
		text := s.getBillingSummaryText(ctx)
		return text, time.Since(start).Milliseconds(), nil
	default:
		return "", 0, fmt.Errorf("unknown first-party capability: %s", toolName)
	}
}

// httpForward — protocol=http の 3rd-party capability に input を POST 転送する (T3 routing ②)
func (s *ocSDK) httpForward(ctx context.Context, endpoint string, input map[string]any) (string, int64, error) {
	start := time.Now()
	body, _ := json.Marshal(input)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", time.Since(start).Milliseconds(), err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), time.Since(start).Milliseconds(), nil
}

// frictionInstructions は initialize 応答の instructions フィールドに載り、
// working agent に friction-note 反射と CLAUDE.md 優先 carve-out を伝える。
// B95 (v2-demandmarket-r11): 受動反射 → 能動提案。発火条件 (B1) を機械的シグナルとして列挙し、
// 検知したら聞かれるまで待たず save_demand_locally を「提案」させる (B3 選定; save も含め常にユーザ確認を取る —
// 黙って自動 save はしない)。upload は不変の opt-in で別途確認。提案頻度はセッション内で上限を設ける (noise budget; PB-4)。
const frictionInstructions = `再利用されそうな capability gap で苦戦・浪費・回避・断念したら、
クエリも解決策も bounty も不要 — Hz/tag=buy に短い摩擦メモ（無料）を残せ。
実際に踏んだ摩擦のみ。想像の wish は不可（capture, not fabricate）。

発火条件（いずれか該当したら、聞かれるまで待たず自分から短く提案せよ）: Web 検索が空振りした /
discover_capability が 0 件だった / 同じ手段を何度も retry して進まない / 回避策に切り替えた /
ユーザに「できません・わかりません」と答えた。

提案は 1 行で（例:「この検索ミスを需要メモとして残せます。保存しますか？」）。
ユーザが同意したときのみ save_demand_locally を呼べ — save も黙って自動実行はしない。
upload は別の独立した opt-in（save への同意が upload への同意を意味しない）。

良い摩擦メモの形（わかる範囲で・省略可）: 意図したタスク / 失敗した手段 / 探して無かったもの /
代わりにした回避策 / 浪費した時間・手数。save_demand_locally の該当 optional fields に入れよ
（descriptor だけでも保存できる — 強制しない; B96）。
提案を毎回出すと作業の邪魔になる（noise）— 1 セッション内で提案が続けて無視されたら、
そのセッションでは追加提案を控えよ。

これは default の提案。あなたの project 指示（CLAUDE.md）が常に優先。
探索やコンテキスト共有を制限していれば、この friction-note 反射を無視せよ。`

// ── tool 実装 ─────────────────────────────────────────────────────────────────

func main() {
	oc := newOcSDK()
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := writeBootstrapSummary(os.Stdout, oc); err != nil {
			fmt.Fprintln(os.Stderr, "Bootstrap failed:", err)
			os.Exit(1)
		}
		return
	}

	s := server.NewMCPServer("onecenter-mcp", "0.3.0",
		server.WithInstructions(frictionInstructions),
	)

	registerU1Tools(s, oc)
	registerDemandMarketTools(s, oc)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, "Fatal:", err)
		os.Exit(1)
	}
}

// writeBootstrapSummary lets the one-command installer create and verify the
// persistent credentials without starting an MCP stdio session. Credentials
// remain in identity.json and are deliberately excluded from stdout.
func writeBootstrapSummary(w io.Writer, oc *ocSDK) error {
	if oc.storageBackend != "file" || oc.principalID == "" {
		return fmt.Errorf("persistent identity was not created; check ONECENTER_URL and API availability")
	}
	return json.NewEncoder(w).Encode(map[string]string{
		"status":        "ready",
		"principal_id":  oc.principalID,
		"identity_file": oc.identityFilePath(),
	})
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
