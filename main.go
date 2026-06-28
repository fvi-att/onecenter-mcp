// cmd/mcp/main.go — OneCenter MCP stdio server (Go)
// Claude Code が MCP client として起動・接続する形式 (OneCenter MCP stdio server; Buyer 汎用 client)
// Design: v2/design/20260607_mcp_client_server_dsl.lisp *mcp-spec* / v2/design/20260610_buyer_tools_dsl.lisp
//
// Usage (.mcp.json): command="go" args=["run", "./cmd/mcp"]
// Env:
//   OC_BUYER_AGENT_ID   — Claude Code 側の buyer agent ID (default: claude-code-buyer)
//   OC_API_KEY          — Seller の agent cred (default: oc_agt_seller_mock_key)
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
	"encoding/pem"
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

// ── identityConfig — ~/.onecenter/mcp/identity.json のスキーマ (v2-r23 / *mcp-identity-file-spec*) ──
// keypair (秘密鍵) は含めない — 漏洩時の被害を cred 再発行範囲に限定する。
type identityConfig struct {
	SellerAgentID string `json:"seller_agent_id"`
	SellerCred    string `json:"seller_cred"`
	BuyerAgentID  string `json:"buyer_agent_id"`
	BuyerCred     string `json:"buyer_cred"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ── PurchaseAgreement — *purchase-agreement* (v2-r6) ─────────────────────────
// フィールドはアルファベット順で定義 (canonical JSON のキー順 = struct 宣言順)。
// sig_a / sig_b は agreement struct の外に置き、署名ループを防ぐ。
type PurchaseAgreement struct {
	AgreedCents   int64  `json:"agreed_cents"`
	BuyerAgentID  string `json:"buyer_agent_id"`
	CallID        string `json:"call_id"`
	CapabilityID  string `json:"capability_id"`
	ExpiresAt     int64  `json:"expires_at"` // Unix 秒
	SellerAgentID string `json:"seller_agent_id"`
}

// InsufficientDcentError — POST /meter/calls 402 時に返るエラー (T3 settle-delegation)
type InsufficientDcentError struct {
	Body map[string]any
}

func (e *InsufficientDcentError) Error() string { return "insufficient_dcent_balance" }

// ── oc-sdk (Go inline) + PKI (*agent-pki-spec*) ──────────────────────────────

type ocSDK struct {
	apiKey         string
	agentID        string // Seller の agent_id (登録後に確定)
	capabilityID   string
	oncenterURL    string
	sessionID      string
	buyerAgentID   string
	buyerCred      string             // buyer agent cred (self-register 時に取得; buyer wallet 参照に使う)
	privKey        ed25519.PrivateKey // Seller 秘密鍵 (SigB)
	pubKeyB64      string             // base64url(Seller public key raw 32 bytes)
	buyerPrivKey   ed25519.PrivateKey // Buyer 秘密鍵 (SigA; v2-r19)
	buyerPubKeyB64 string             // base64url(Buyer public key raw 32 bytes)
	client         *http.Client

	// v2-pki-r2: identity フィールド (agentID/buyerAgentID/cred/keypair) を保護する。
	// regenerate_identity tool が write lock を取って agent_id + keypair を作り直す。
	idmu sync.RWMutex

	// v2-r23: identity 永続化 (*mcp-identity-persistence-spec*)
	// "file"=永続化済み / "ephemeral"=再起動で消える (OC_API_KEY env injection or 保存失敗)
	storageBackend string
	// identityDir: テスト注入用 base dir (空文字のとき ~/.onecenter を使う)
	identityDir string

	// v2-r17: P2P ファイル永続化のベースディレクトリ。
	// 空のとき: ~/.onecenter/p2p。テストでは tmpDir を注入して隔離する。
	p2pBaseDir string

	// B50: Demand Market ローカル DemandRecord 保存のベースディレクトリ (*demand-record-spec* :storage)。
	// 空のとき: ~/.onecenter (→ <base>/demand/<id>.json)。テストでは tmpDir を注入して隔離する。
	demandBaseDir string

	// v2-r16: Quotation P2P-local ストレージ (*quotation-spec* :storage)
	// OneCenter API には登録せず、Buyer runtime のローカルメモリで管理する。
	localQuotations map[string]map[string]any // quotation_id → quotation
	seenQIDs        map[string]struct{}       // replay 防止 (dedup)
	qmu             sync.RWMutex

	// Agreement P2P-local ストレージ (*purchase-agreement* :local-storage)
	// Buyer: handleCallCapability 後に保存 (call_id keyed; replay 防止・精算照合)
	// Seller: recordCallSync 成功後に保存 (課金ログ・dispute 証拠保全)
	localAgreements map[string]map[string]any // call_id → agreement record
	seenCallIDs     map[string]struct{}       // replay 防止 (call_id dedup)
	amu             sync.RWMutex
}

func newOcSDK() *ocSDK {
	oncenterURL := getenv("ONECENTER_URL", "http://localhost:8080")
	httpClient := &http.Client{Timeout: 10 * time.Second}

	// 初期 keypair を生成 (永続化モードでは loadPersistedIdentity が上書きする)
	sellerPub, sellerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("failed to generate Seller Ed25519 keypair: " + err.Error())
	}
	buyerPub, buyerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("failed to generate Buyer Ed25519 keypair: " + err.Error())
	}

	sdk := &ocSDK{
		capabilityID:    getenv("OC_CAPABILITY_ID", "00000000-0000-0000-0000-000000000001"),
		oncenterURL:     oncenterURL,
		sessionID:       "claude-code-stdio-session",
		privKey:         sellerPriv,
		pubKeyB64:       base64.RawURLEncoding.EncodeToString(sellerPub),
		buyerPrivKey:    buyerPriv,
		buyerPubKeyB64:  base64.RawURLEncoding.EncodeToString(buyerPub),
		client:          httpClient,
		storageBackend:  "ephemeral",
		localQuotations: make(map[string]map[string]any),
		seenQIDs:        make(map[string]struct{}),
		localAgreements: make(map[string]map[string]any),
		seenCallIDs:     make(map[string]struct{}),
	}

	apiKey := getenv("OC_API_KEY", "")
	if apiKey != "" {
		// OC_API_KEY 設定済み → env injection モード (CI; ephemeral; DT4)
		sdk.apiKey = apiKey
		sdk.agentID = getenv("OC_SELLER_AGENT_ID", "")
		sdk.buyerAgentID = getenv("OC_BUYER_AGENT_ID", "")
		if sdk.buyerAgentID == "" {
			if cred, id := selfRegister(httpClient, oncenterURL, "buyer", "onecenter-mcp-buyer"); id != "" {
				sdk.buyerAgentID = id
				sdk.buyerCred = cred
			} else {
				sdk.buyerAgentID = "claude-code-buyer"
			}
		}
	} else {
		// OC_API_KEY 未設定 → identity.json 永続化モード (flow mcp-identity-first-run / restart)
		if !sdk.loadPersistedIdentity() {
			// flow mcp-identity-first-run (F1 keypair 生成済み; F2 selfRegister; F3/F4 永続化; F5 pubkey 登録)
			sdk.apiKey, sdk.agentID = selfRegister(httpClient, oncenterURL, "seller", "onecenter-mcp-seller")
			if cred, id := selfRegister(httpClient, oncenterURL, "buyer", "onecenter-mcp-buyer"); id != "" {
				sdk.buyerAgentID = id
				sdk.buyerCred = cred
			} else {
				sdk.buyerAgentID = "claude-code-buyer"
			}
			// F3/F4: keypair + identity.json を永続化 (API 到達できた場合のみ)
			if sdk.agentID != "" && sdk.buyerAgentID != "claude-code-buyer" {
				sdk.storageBackend = sdk.persistIdentity()
			}
		}
		// restart path: loadPersistedIdentity が agentID/cred/keypair を設定済み
	}

	// F5/R4: 公開鍵を OneCenter に登録 (idempotent; 再起動時の 409 は無視)
	sdk.registerPubkey()
	sdk.registerBuyerPubkey()
	// T1: first-party capability を registry に seed (API MemStore は起動ごとにリセット)
	sdk.seedFirstPartyCapabilities()
	// v2-r17: ファイルから Quotation/Agreement を runtime memory に復元する
	sdk.hydrateFromFiles()
	return sdk
}

// selfRegister — OneCenter API に principal + agent を登録して cred と agentID を返す
// MemStore は起動のたびにリセットされるため、MCP サーバー起動ごとに再登録する。
// role は "seller" | "buyer"。buyer は実 UUID を得る目的で登録する (flow buyer-onboard)。
func selfRegister(client *http.Client, baseURL, role, name string) (cred, agentID string) {
	post := func(path string, body map[string]any) (map[string]any, error) {
		b, _ := json.Marshal(body)
		resp, err := client.Post(baseURL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		return result, nil
	}

	pr, err := post("/auth/signup", map[string]any{
		"type": "individual", "name": name, "email": name + "@onecenter.local",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] selfRegister(%s): signup failed: %v\n", role, err)
		return "", ""
	}
	principalID, _ := pr["id"].(string)

	ar, err := post("/agents", map[string]any{
		"principal_id": principalID, "role": role, "name": name,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] selfRegister(%s): create agent failed: %v\n", role, err)
		return "", ""
	}
	cred, _ = ar["cred"].(string)
	agentID, _ = ar["id"].(string)
	fmt.Fprintf(os.Stderr, "[oc-sdk] self-registered %s: agent_id=%s\n", role, agentID)
	return cred, agentID
}

// ── v2-r23: MCP identity 永続化ヘルパー (*mcp-identity-persistence-spec* / *mcp-key-file-spec*) ──

// identityFilePath — ~/.onecenter/mcp/identity.json または identityDir 配下のパスを返す
func (s *ocSDK) identityFilePath() string {
	base := identityBaseDir(s.identityDir)
	return filepath.Join(base, "mcp", "identity.json")
}

// mcpKeyFilePath — keypair ファイルパス (role: "seller"|"buyer"; agentID で一意化)
func (s *ocSDK) mcpKeyFilePath(role, agentID string) string {
	base := identityBaseDir(s.identityDir)
	return filepath.Join(base, "keys", "mcp-"+role+"-"+agentID+".key")
}

// saveMCPKey — Ed25519 秘密鍵を PEM ファイル (0600) に保存する
func saveMCPKey(path string, privKey ed25519.PrivateKey) error {
	block := &pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: privKey.Seed()}
	return writePrivateFile(path, pem.EncodeToMemory(block))
}

// loadMCPKey — PEM ファイルから Ed25519 秘密鍵をロードする
func loadMCPKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "ED25519 PRIVATE KEY" {
		return nil, fmt.Errorf("invalid PEM: %s", path)
	}
	if len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("invalid seed size %d (want %d): %s", len(block.Bytes), ed25519.SeedSize, path)
	}
	return ed25519.NewKeyFromSeed(block.Bytes), nil
}

// loadIdentityConfig — identity.json をロードする
func loadIdentityConfig(path string) (*identityConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg identityConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// saveIdentityConfig — identity.json を 0600 で書き出す
func saveIdentityConfig(path string, cfg *identityConfig) error {
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

// persistIdentity — keypair files + identity.json を書き出す (flow mcp-identity-first-run F3/F4)
// 成功すれば "file"、失敗すれば "ephemeral" を返す。
func (s *ocSDK) persistIdentity() string {
	sellerKeyPath := s.mcpKeyFilePath("seller", s.agentID)
	buyerKeyPath := s.mcpKeyFilePath("buyer", s.buyerAgentID)

	if err := saveMCPKey(sellerKeyPath, s.privKey); err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to save seller keypair: %v\n", err)
		return "ephemeral"
	}
	if err := saveMCPKey(buyerKeyPath, s.buyerPrivKey); err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to save buyer keypair: %v\n", err)
		return "ephemeral"
	}
	cfg := &identityConfig{
		SellerAgentID: s.agentID,
		SellerCred:    s.apiKey,
		BuyerAgentID:  s.buyerAgentID,
		BuyerCred:     s.buyerCred,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveIdentityConfig(s.identityFilePath(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to save identity.json: %v\n", err)
		return "ephemeral"
	}
	fmt.Fprintf(os.Stderr, "[oc-sdk] identity persisted: seller=%s buyer=%s path=%s\n",
		s.agentID, s.buyerAgentID, s.identityFilePath())
	return "file"
}

// loadPersistedIdentity — identity.json + keypair files をロードして sdk を更新する
// (flow mcp-identity-restart R1-R4)
// 成功すれば true (restart path)、失敗 or ファイル不在なら false (first-run path)。
func (s *ocSDK) loadPersistedIdentity() bool {
	cfg, err := loadIdentityConfig(s.identityFilePath())
	if err != nil {
		return false // identity.json 不在 → first-run
	}

	sellerPrivKey, err1 := loadMCPKey(s.mcpKeyFilePath("seller", cfg.SellerAgentID))
	buyerPrivKey, err2 := loadMCPKey(s.mcpKeyFilePath("buyer", cfg.BuyerAgentID))
	if err1 != nil || err2 != nil {
		// R2 fallback: keypair 欠損 → first-run にフォールスルー (đ 残高リセット警告)
		fmt.Fprintf(os.Stderr,
			"[oc-sdk] WARNING: keypair load failed (seller=%v buyer=%v) — regenerating identity. đ balance will reset.\n",
			err1, err2)
		return false
	}

	// R2/R3: identity を復元 (selfRegister をスキップ)
	s.agentID = cfg.SellerAgentID
	s.apiKey = cfg.SellerCred
	s.buyerAgentID = cfg.BuyerAgentID
	s.buyerCred = cfg.BuyerCred
	s.privKey = sellerPrivKey
	s.pubKeyB64 = base64.RawURLEncoding.EncodeToString(sellerPrivKey.Public().(ed25519.PublicKey))
	s.buyerPrivKey = buyerPrivKey
	s.buyerPubKeyB64 = base64.RawURLEncoding.EncodeToString(buyerPrivKey.Public().(ed25519.PublicKey))
	s.storageBackend = "file"
	fmt.Fprintf(os.Stderr, "[oc-sdk] identity loaded from file: seller=%s buyer=%s\n",
		s.agentID, s.buyerAgentID)
	return true
}

// registerPubkey — POST /agents/:id/pubkeys Seller 公開鍵登録 (*agent-pki-spec* registration)
func (s *ocSDK) registerPubkey() {
	body, _ := json.Marshal(map[string]any{
		"pubkey":    s.pubKeyB64,
		"algorithm": "ed25519",
	})
	url := fmt.Sprintf("%s/agents/%s/pubkeys", s.oncenterURL, s.agentID)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] seller pubkey registration skipped (API unreachable): %v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Fprintf(os.Stderr, "[oc-sdk] seller pubkey registered for agent %s (status %d)\n", s.agentID, resp.StatusCode)
}

// registerBuyerPubkey — POST /agents/:buyerID/pubkeys Buyer 公開鍵登録 (v2-r19 dual-sig SigA)
// POST /agents/:id/pubkeys は auth 不要 (main.go router rl のみ) のためベストエフォートで登録する。
func (s *ocSDK) registerBuyerPubkey() {
	body, _ := json.Marshal(map[string]any{
		"pubkey":    s.buyerPubKeyB64,
		"algorithm": "ed25519",
	})
	url := fmt.Sprintf("%s/agents/%s/pubkeys", s.oncenterURL, s.buyerAgentID)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] buyer pubkey registration skipped (API unreachable): %v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Fprintf(os.Stderr, "[oc-sdk] buyer pubkey registered for agent %s (status %d)\n", s.buyerAgentID, resp.StatusCode)
}

// regenerateIdentity — 新しい agent_id + Ed25519 keypair を生成して再登録する (v2-pki-r2)
//
// oc/regenerate-identity (*identity-regeneration-tool*): 起動時 (newOcSDK) に一度だけ行う
// self-register + keygen + pubkey 登録を runtime に再実行し、agent_id ごと identity を作り直す。
// role: "buyer" | "seller" | "both"。
//
// oc/rotate-keypair (同一 agent_id の鍵更新) と異なり、agent_id 自体を新規取得する。
// 新 buyer は airdrop 未受領のため fresh (operator から再 airdrop 可能; strategy S1 reward)。
//
// OneCenter API 到達不能で selfRegister が agent_id を返さない場合は in-memory state を
// 差し替えずエラーを返す (中途半端な identity を作らない)。
func (s *ocSDK) regenerateIdentity(role string) (map[string]any, error) {
	if role == "" {
		role = "buyer"
	}
	if role != "buyer" && role != "seller" && role != "both" {
		return nil, fmt.Errorf("invalid role %q (must be buyer|seller|both)", role)
	}

	s.idmu.Lock()
	defer s.idmu.Unlock()

	out := map[string]any{"role": role}

	if role == "buyer" || role == "both" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("buyer keypair generation failed: %w", err)
		}
		cred, id := selfRegister(s.client, s.oncenterURL, "buyer", "onecenter-mcp-buyer")
		if id == "" {
			return nil, fmt.Errorf("buyer self-register failed (OneCenter API unreachable?)")
		}
		s.buyerAgentID = id
		s.buyerCred = cred
		s.buyerPrivKey = priv
		s.buyerPubKeyB64 = base64.RawURLEncoding.EncodeToString(pub)
		s.registerBuyerPubkey()
		out["buyer_agent_id"] = id
		out["buyer_pubkey_hint"] = s.buyerPubKeyB64[:min(8, len(s.buyerPubKeyB64))]
	}

	if role == "seller" || role == "both" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("seller keypair generation failed: %w", err)
		}
		cred, id := selfRegister(s.client, s.oncenterURL, "seller", "onecenter-mcp-seller")
		if id == "" {
			return nil, fmt.Errorf("seller self-register failed (OneCenter API unreachable?)")
		}
		s.apiKey = cred
		s.agentID = id
		s.privKey = priv
		s.pubKeyB64 = base64.RawURLEncoding.EncodeToString(pub)
		s.registerPubkey()
		// 新 seller agent_id で first-party capability を再 seed する (seller_agent_id を新 ID に紐付け直す)
		s.seedFirstPartyCapabilities()
		out["seller_agent_id"] = id
		out["seller_pubkey_hint"] = s.pubKeyB64[:min(8, len(s.pubKeyB64))]
	}

	return out, nil
}

// signBuyerBytes — Buyer 秘密鍵で bytes に署名 → base64url (SigA; v2-r19)
func (s *ocSDK) signBuyerBytes(data []byte) string {
	digest := sha256.Sum256(data)
	sig := ed25519.Sign(s.buyerPrivKey, digest[:])
	return base64.RawURLEncoding.EncodeToString(sig)
}

// signBuyerAgreement — canonical_json(agreement) → sha256 → ed25519_sign(buyerPrivKey) → base64url (SigA)
func (s *ocSDK) signBuyerAgreement(agreement *PurchaseAgreement) (string, error) {
	canonical, err := json.Marshal(agreement)
	if err != nil {
		return "", err
	}
	return s.signBuyerBytes(canonical), nil
}

// seedFirstPartyCapabilities — T1: @onecenter/operator.* 3 件を registry に登録する。
// MemStore は再起動でリセットされるため、MCP サーバー起動ごとに実行する。
// (*first-party-capabilities* / LQ-first-party-seed / plan T1)
func (s *ocSDK) seedFirstPartyCapabilities() {
	type capDef struct {
		toolName, desc, pricing string
		priceDcents             int64
	}
	defs := []capDef{
		{"word_count", "Count words, sentences, and characters in text. Returns word/sentence/character counts.", "per-call", 5},
		{"echo_text", "Echo input text back with an optional prefix string.", "per-call", 10},
		{"billing_summary", "Return billing summary including đ balance and recent call history for the agent.", "free", 0},
	}

	for _, d := range defs {
		name := "@onecenter/operator." + d.toolName
		body, _ := json.Marshal(map[string]any{
			"name":            name,
			"description":     d.desc,
			"seller_agent_id": s.agentID,
			"mcp_endpoint":    "mcp://onecenter-operator/" + d.toolName,
			"protocol":        "mcp",
			"pricing_model":   d.pricing,
			"price_cents":     d.priceDcents,
		})
		resp, err := s.client.Post(s.oncenterURL+"/capabilities", "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oc-sdk] seedCapability %q failed: %v\n", name, err)
			continue
		}
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "[oc-sdk] seeded capability: %s (status %d)\n", name, resp.StatusCode)
	}
}

// signBytes — bytes → ed25519_sign → base64url (*agent-pki-spec*)
func (s *ocSDK) signBytes(data []byte) string {
	digest := sha256.Sum256(data)
	sig := ed25519.Sign(s.privKey, digest[:])
	return base64.RawURLEncoding.EncodeToString(sig)
}

// generateAgreement — v2-r6 PurchaseAgreement を生成する (Seller として; 既存 capabilityID 用)
func (s *ocSDK) generateAgreement(cents int64) (*PurchaseAgreement, error) {
	callID := uuid.New().String()
	return &PurchaseAgreement{
		AgreedCents:   cents,
		BuyerAgentID:  s.buyerAgentID,
		CallID:        callID,
		CapabilityID:  s.capabilityID,
		ExpiresAt:     time.Now().Add(60 * time.Second).Unix(),
		SellerAgentID: s.agentID,
	}, nil
}

// generateAgreementFor — 任意 capID / buyerID / sellerID / dcents で agreement を生成する (T3)
func (s *ocSDK) generateAgreementFor(capID, buyerAgentID, sellerAgentID string, dcents int64) *PurchaseAgreement {
	return &PurchaseAgreement{
		AgreedCents:   dcents,
		BuyerAgentID:  buyerAgentID,
		CallID:        uuid.New().String(),
		CapabilityID:  capID,
		ExpiresAt:     time.Now().Add(60 * time.Second).Unix(),
		SellerAgentID: sellerAgentID,
	}
}

// signAgreement — canonical_json(agreement) → sha256 → ed25519_sign → base64url (SigB)
func (s *ocSDK) signAgreement(agreement *PurchaseAgreement) (string, error) {
	canonical, err := json.Marshal(agreement)
	if err != nil {
		return "", err
	}
	return s.signBytes(canonical), nil
}

// B4: spend-cap チェック (同期; handler 実行前)
func (s *ocSDK) checkSpendCap(cents int) (allowed bool, remainingCents int, err error) {
	body, _ := json.Marshal(map[string]any{
		"buyer_agent_id": s.buyerAgentID,
		"cents":          cents,
	})
	resp, err := s.client.Post(s.oncenterURL+"/meter/spend-cap/check", "application/json", bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	var result struct {
		Allowed        bool `json:"allowed"`
		RemainingCents int  `json:"remaining_cents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, 0, err
	}
	return result.Allowed, result.RemainingCents, nil
}

// recordCall — v2-r19: agreement + SigA (Buyer) + SigB (Seller) dual-sig で POST /meter/calls (fire-and-forget)
func (s *ocSDK) recordCall(toolName string, cents int, inputHash, outputHash string, latencyMs int64) {
	go func() {
		agreement, err := s.generateAgreement(int64(cents))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oc-sdk] generateAgreement failed: %v\n", err)
			return
		}
		sigA, err := s.signBuyerAgreement(agreement) // SigA: Buyer 署名
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oc-sdk] signBuyerAgreement failed: %v\n", err)
			return
		}
		sigB, err := s.signAgreement(agreement) // SigB: Seller countersign
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oc-sdk] signAgreement failed: %v\n", err)
			return
		}

		payload := map[string]any{
			"agreement":       agreement,
			"sig_a":           sigA,
			"sig_b":           sigB,
			"agreed_cents":    int64(cents),
			"currency":        "dcent",
			"call_id":         agreement.CallID,
			"capability_id":   s.capabilityID,
			"tool_name":       toolName,
			"buyer_agent_id":  s.buyerAgentID,
			"seller_agent_id": s.agentID,
			"input_hash":      inputHash,
			"output_hash":     outputHash,
			"pricing_model":   "per-call",
			"success":         true,
			"latency_ms":      latencyMs,
			"mcp_session_id":  s.sessionID,
			"occurred_at":     time.Now().UTC().Format(time.RFC3339),
			"sdk_version":     "0.5.0-go-r22",
			"pubkey_hint":     s.pubKeyB64[:min(8, len(s.pubKeyB64))],
		}

		body, _ := json.Marshal(payload)
		resp, err := s.client.Post(s.oncenterURL+"/meter/calls", "application/json", bytes.NewReader(body))
		if err == nil {
			var res map[string]any
			json.NewDecoder(resp.Body).Decode(&res)
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "[oc-sdk] call recorded: tool=%s agreed_cents=%d sig_verified=%v\n",
				toolName, cents, res["sig_verified"])
		} else {
			fmt.Fprintf(os.Stderr, "[oc-sdk] recordCall POST failed: %v\n", err)
		}
	}()
}

// recordCallSync — 同期版 recordCall。POST /meter/calls の応答を返す。
// 402 (残高不足) の場合は *InsufficientDcentError を返す (T3 settle-delegation)。
// 戻り値の string は使用した call_id (Buyer 側の Agreement ローカル保存に利用)。
func (s *ocSDK) recordCallSync(ctx context.Context, toolName, capID, buyerAgentID, sellerAgentID string, dcents int64, inputHash, outputHash string, latencyMs int64) (string, error) {
	agreement := s.generateAgreementFor(capID, buyerAgentID, sellerAgentID, dcents)
	sigA, err := s.signBuyerAgreement(agreement) // SigA: Buyer 署名 (v2-r19)
	if err != nil {
		return "", err
	}
	sigB, err := s.signAgreement(agreement) // SigB: Seller countersign
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"agreement":       agreement,
		"sig_a":           sigA,
		"sig_b":           sigB,
		"agreed_cents":    dcents,
		"currency":        "dcent",
		"call_id":         agreement.CallID,
		"capability_id":   capID,
		"tool_name":       toolName,
		"buyer_agent_id":  buyerAgentID,
		"seller_agent_id": sellerAgentID,
		"input_hash":      inputHash,
		"output_hash":     outputHash,
		"pricing_model":   "per-call",
		"success":         true,
		"latency_ms":      latencyMs,
		"mcp_session_id":  s.sessionID,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339),
		"sdk_version":     "0.5.0-go-t3-r19",
		"pubkey_hint":     s.pubKeyB64[:min(8, len(s.pubKeyB64))],
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/meter/calls", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPaymentRequired {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", &InsufficientDcentError{Body: errBody}
	}

	// 202 Accepted 以外は settle 失敗。401 (dual_sig_required / invalid_agreement_signature)、
	// 409 (duplicate_call_id)、410 (agreement_expired) などを *握り潰さず* エラーとして返す。
	// (これを返さないと call_capability が record されていない call を success=true と誤報告する)
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST /meter/calls failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	// *purchase-agreement* :local-storage — Seller runtime: POST 成功後に保存 (v2-r17: ファイル永続化)
	// キーは "seller:<call_id>" — Buyer 側 ("buyer:<call_id>") と共存できるようにする
	s.amu.Lock()
	if _, seen := s.seenCallIDs[agreement.CallID]; !seen {
		s.seenCallIDs[agreement.CallID] = struct{}{}
		rec := map[string]any{
			"role":            "seller",
			"call_id":         agreement.CallID,
			"capability_id":   capID,
			"agreed_dcents":   dcents,
			"buyer_agent_id":  buyerAgentID,
			"seller_agent_id": sellerAgentID,
			"expires_at":      agreement.ExpiresAt,
			"sig_b":           sigB,
			"settled_at":      time.Now().UTC().Format(time.RFC3339),
		}
		s.localAgreements["seller:"+agreement.CallID] = rec
		s.amu.Unlock()
		// v2-r17: ファイル永続化 (~/.onecenter/p2p/<agent_id>/agreements/seller/<call_id>.json; 0600)
		if path := s.p2pFilePath("agreements", "seller", agreement.CallID); path != "" {
			if err := saveP2PFile(path, rec); err != nil {
				fmt.Fprintf(os.Stderr, "[p2p-store] save seller agreement failed: %v\n", err)
			}
		}
	} else {
		s.amu.Unlock()
	}
	return agreement.CallID, nil
}

// getWalletBalance — GET /dcent/wallets/:agentID → balance_dcents
func (s *ocSDK) getWalletBalance(ctx context.Context, agentID string) int64 {
	walletURL := fmt.Sprintf("%s/dcent/wallets/%s", s.oncenterURL, agentID)
	req, _ := http.NewRequestWithContext(ctx, "GET", walletURL, nil)
	// wallet GET は本人 cred (cred→agent_id 一致) のみ可。buyer の残高は buyer cred で読む。
	// (seller cred で buyer wallet を読むと 403 → 残高 0 と誤報告される)
	cred := s.apiKey
	if agentID == s.buyerAgentID && s.buyerCred != "" {
		cred = s.buyerCred
	}
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
// sender は buyer agent-cred で確定するため、from_agent_id は送らない。
func (s *ocSDK) transferDcent(ctx context.Context, toAgentID string, dcents int64, memo, transferID string) (map[string]any, int, error) {
	if strings.TrimSpace(transferID) == "" {
		transferID = uuid.NewString()
	}
	body, _ := json.Marshal(map[string]any{
		"transfer_id": transferID,
		"to_agent_id": toAgentID,
		"dcents":      dcents,
		"memo":        memo,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/dcent/transfers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cred := s.buyerCred
	if cred == "" {
		if s.buyerAgentID != "" && s.agentID != "" && s.buyerAgentID != s.agentID {
			return nil, 0, fmt.Errorf("buyer credential is required for transfer_dcent")
		}
		cred = s.apiKey
	}
	req.Header.Set("Authorization", "Bearer "+cred)

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

type discoverEmitMode string

const (
	discoverEmitOff     discoverEmitMode = "off"
	discoverEmitPrivate discoverEmitMode = "private"
	discoverEmitOn      discoverEmitMode = "on"
)

// currentDiscoverEmitMode is fail-closed: only an explicit on value permits
// sending a discover query to OneCenter. private records neither remotely nor
// locally today, so raw project context cannot leak into an artifact.
func currentDiscoverEmitMode() discoverEmitMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OC_DISCOVER_EMIT"))) {
	case "on", "1", "true", "yes":
		return discoverEmitOn
	case "private":
		return discoverEmitPrivate
	default:
		return discoverEmitOff
	}
}

// recordDemandSignal — POST /demand/signals (discover 不発時の GA1 燃料; flow discover-to-demand).
// Sending the raw query requires explicit OC_DISCOVER_EMIT=on consent.
func (s *ocSDK) recordDemandSignal(ctx context.Context, query string, maxPriceDcents int64) bool {
	if currentDiscoverEmitMode() != discoverEmitOn {
		return false
	}
	body, _ := json.Marshal(map[string]any{
		"semantic_descriptor": query,
		"unmet_value_cents":   maxPriceDcents,
		"zero_seller":         true,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/demand/signals", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := s.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oc-sdk] recordDemandSignal failed: %v\n", err)
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusAccepted
}

func hashStr(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// ── v2-r17: P2P ファイル永続化ヘルパー ──────────────────────────────────────
//
// 保存パス規則:
//   Quotation Buyer  received: <base>/<agent_id>/quotations/received/<quotation_id>.json
//   Quotation Seller sent:     <base>/<agent_id>/quotations/sent/<quotation_id>.json
//   Agreement Seller:          <base>/<agent_id>/agreements/seller/<call_id>.json
//   Agreement Buyer:           <base>/<agent_id>/agreements/buyer/<call_id>.json

// p2pFilePath — ファイルパスを構築する (docType: "quotations"|"agreements"; role: "received"|"sent"|"seller"|"buyer")
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

// getBillingSummaryText — GET /dcent/wallets/:agentID + /meter/calls → summary text
func (s *ocSDK) getBillingSummaryText(ctx context.Context, agentID string) string {
	walletURL := fmt.Sprintf("%s/dcent/wallets/%s", s.oncenterURL, agentID)
	walletReq, _ := http.NewRequestWithContext(ctx, "GET", walletURL, nil)
	walletReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	var balanceDcents int64
	walletResp, err := s.client.Do(walletReq)
	if err == nil {
		defer walletResp.Body.Close()
		var wData struct {
			BalanceDcents int64 `json:"balance_dcents"`
		}
		json.NewDecoder(walletResp.Body).Decode(&wData)
		balanceDcents = wData.BalanceDcents
	}

	resp, err := s.client.Get(s.oncenterURL + "/meter/calls")
	if err != nil {
		return "Could not reach OneCenter API. Start it with: cd v2/src/onecenter-api && go run ."
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var data struct {
		Calls []struct {
			CentsCharged  int    `json:"cents_charged"`
			ToolName      string `json:"tool_name"`
			BuyerAgentID  string `json:"buyer_agent_id"`
			SellerAgentID string `json:"seller_agent_id"`
		} `json:"calls"`
	}
	json.Unmarshal(raw, &data)

	var totalSpent, totalEarned int
	var lines []string
	for _, c := range data.Calls {
		if c.SellerAgentID == s.agentID {
			totalEarned += c.CentsCharged
			lines = append(lines, fmt.Sprintf("  [seller] %s: +%dđ", c.ToolName, c.CentsCharged))
		} else if c.BuyerAgentID == agentID {
			totalSpent += c.CentsCharged
			lines = append(lines, fmt.Sprintf("  [buyer]  %s: -%dđ", c.ToolName, c.CentsCharged))
		}
	}

	detail := strings.Join(lines, "\n")
	if detail == "" {
		detail = "  (no calls yet)"
	}
	return fmt.Sprintf("Billing for agent %s:\n  đ balance: %dđ  earned: +%dđ  spent: -%dđ\n  calls: %d\n%s",
		agentID, balanceDcents, totalEarned, totalSpent, len(lines), detail)
}

// ── first-party dispatcher (T3: @onecenter/operator.* を直接実行) ──

// dispatchFirstParty — capability name からハンドラを選んで直接実行する。
// 計算のみ実行し、課金 (recordCall) は呼び出し元 (handleCallCapability) が担う。
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
		text := s.getBillingSummaryText(ctx, s.buyerAgentID)
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
const frictionInstructions = `再利用されそうな capability gap で苦戦・浪費・回避・断念したら、
クエリも解決策も bounty も不要 — Hz/tag=buy に短い摩擦メモ（無料）を残せ。
実際に踏んだ摩擦のみ。想像の wish は不可（capture, not fabricate）。

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
	registerU2QuotationTools(s, oc)
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
	if oc.storageBackend != "file" || oc.agentID == "" || oc.buyerAgentID == "" {
		return fmt.Errorf("persistent identity was not created; check ONECENTER_URL and API availability")
	}
	return json.NewEncoder(w).Encode(map[string]string{
		"status":        "ready",
		"seller_agent":  oc.agentID,
		"buyer_agent":   oc.buyerAgentID,
		"identity_file": oc.identityFilePath(),
	})
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
