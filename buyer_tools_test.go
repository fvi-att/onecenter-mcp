// buyer_tools_test.go — T3: discover_capability / call_capability の統合テスト
//
// 受け入れ基準 (*buyer-tools-acceptance* bt-r1):
//   D1: discover → 0 件 → demand_signal が /demand/board に記録される
//   D2: call_capability(word_count) → đ 課金 → billing_summary に反映
//   D3: discover 結果に price_dcents / reputation / sig_status / callable が含まれ cent/USD 換算なし
//   D4: 残高不足の call は 402 → {error: insufficient_dcent_balance} を返す (call 不記録)
//
// 設計根拠:
//   design/20260610_buyer_tools_dsl.lisp (bt-r1)
//   plan/20260610_ea_readiness_plan.lisp T3
//
// テスト方式:
//   httptest.NewServer で軽量モック API を起動 (onecenter-api への依存なし)。
//   ocSDK を直接インスタンス化してハンドラ関数を呼ぶ。

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ── test helpers ─────────────────────────────────────────────────────────────

// mockAPI — テスト用の最小 OneCenter API モック
type mockAPI struct {
	capabilities    []map[string]any
	demandSignals   []map[string]any
	callEvents      []map[string]any
	meterCallStatus int // デフォルト 202; 402 テスト用に上書き可

	// v2-pki-r2: regenerate_identity 用の self-register / pubkey 登録 / 再 seed カウンタ
	agentSeq        int      // signup/agents 連番 (新 agent_id を一意化)
	pubkeyRegs      int      // POST /agents/:id/pubkeys 回数
	seededCaps      int      // POST /capabilities 回数 (seller 再 seed)
	registeredRoles []string // POST /agents で受け取った role の履歴
}

func newMockAPI(caps []map[string]any) *mockAPI {
	return &mockAPI{
		capabilities:    caps,
		meterCallStatus: http.StatusAccepted,
	}
}

func (m *mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/capabilities":
		query := r.URL.Query().Get("semantic")
		var result []map[string]any
		for _, c := range m.capabilities {
			if query == "" || mockSemanticMatch(c, query) {
				result = append(result, c)
			}
		}
		if result == nil {
			result = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"capabilities": result, "total": len(result)})

	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/capabilities/"):
		id := strings.TrimPrefix(r.URL.Path, "/capabilities/")
		for _, c := range m.capabilities {
			if c["id"] == id {
				json.NewEncoder(w).Encode(c)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "not found"})

	case r.Method == "POST" && r.URL.Path == "/demand/signals":
		var d map[string]any
		json.NewDecoder(r.Body).Decode(&d)
		m.demandSignals = append(m.demandSignals, d)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(d)

	case r.Method == "POST" && r.URL.Path == "/meter/calls":
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		m.callEvents = append(m.callEvents, e)

		if m.meterCallStatus == http.StatusPaymentRequired {
			w.WriteHeader(http.StatusPaymentRequired)
			json.NewEncoder(w).Encode(map[string]any{
				"code":            "insufficient_dcent_balance",
				"balance_dcents":  int64(0),
				"required_dcents": int64(5),
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "recorded", "sig_verified": true})

	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/dcent/wallets/"):
		json.NewEncoder(w).Encode(map[string]any{
			"balance_dcents":          int64(995),
			"total_earned_dcents":     int64(5),
			"total_spent_dcents":      int64(5),
			"total_airdropped_dcents": int64(1000),
		})

	case r.Method == "POST" && r.URL.Path == "/meter/spend-cap/check":
		json.NewEncoder(w).Encode(map[string]any{"allowed": true, "remaining_cents": 999999})

	// ── v2-pki-r2: regenerate_identity が叩く self-register / pubkey / 再 seed ──
	case r.Method == "POST" && r.URL.Path == "/auth/signup":
		m.agentSeq++
		json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("principal-%d", m.agentSeq)})

	case r.Method == "POST" && r.URL.Path == "/agents":
		m.agentSeq++
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		role, _ := b["role"].(string)
		m.registeredRoles = append(m.registeredRoles, role)
		json.NewEncoder(w).Encode(map[string]any{
			"id":   fmt.Sprintf("%s-regen-%d", role, m.agentSeq),
			"cred": fmt.Sprintf("oc_agt_regen_%d", m.agentSeq),
		})

	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pubkeys"):
		m.pubkeyRegs++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"pubkey_id": fmt.Sprintf("pk-%d", m.pubkeyRegs), "status": "active"})

	case r.Method == "POST" && r.URL.Path == "/capabilities":
		m.seededCaps++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("cap-seed-%d", m.seededCaps)})

	case r.Method == "GET" && r.URL.Path == "/meter/calls":
		json.NewEncoder(w).Encode(map[string]any{"calls": m.callEvents, "total": len(m.callEvents)})

	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":"not found","path":%q}`, r.URL.Path)
	}
}

// mockSemanticMatch — テスト用: query の各トークンが name/description に部分一致するか
func mockSemanticMatch(cap map[string]any, query string) bool {
	name, _ := cap["name"].(string)
	desc, _ := cap["description"].(string)
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if strings.Contains(strings.ToLower(name), t) || strings.Contains(strings.ToLower(desc), t) {
			return true
		}
	}
	return false
}

// testCaps — word_count capability (first-party)
var testWordCountCap = map[string]any{
	"id":              "cap-word-count-001",
	"name":            "@onecenter/operator.word_count",
	"description":     "Count words sentences characters in text",
	"protocol":        "mcp",
	"pricing_model":   "per-call",
	"price_cents":     int64(5),
	"sig_status":      "no-pubkey",
	"seller_agent_id": "seller-001",
	"mcp_endpoint":    "mcp://onecenter-operator/word_count",
	"reputation": map[string]any{
		"success_rate": 1.0, "p50_latency_ms": 1, "p95_latency_ms": 2, "volume_30d": 0,
	},
}

// newTestSDK — テスト用 ocSDK (mock API server を向く; Seller/Buyer Ed25519 keypair を生成; v2-r19 dual-sig)
func newTestSDK(ts *httptest.Server) *ocSDK {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	buyerPub, buyerPriv, _ := ed25519.GenerateKey(rand.Reader)
	buyerPubB64 := base64.RawURLEncoding.EncodeToString(buyerPub)
	return &ocSDK{
		apiKey:          "test-key",
		agentID:         "seller-001",
		buyerAgentID:    "buyer-001",
		oncenterURL:     ts.URL,
		sessionID:       "test-session",
		privKey:         priv,
		pubKeyB64:       pubB64,
		buyerPrivKey:    buyerPriv,
		buyerPubKeyB64:  buyerPubB64,
		client:          ts.Client(),
		localQuotations: make(map[string]map[string]any),
		seenQIDs:        make(map[string]struct{}),
		localAgreements: make(map[string]map[string]any),
		seenCallIDs:     make(map[string]struct{}),
	}
}

// toolResultText — mcp.CallToolResult からテキストを抽出するヘルパー
func toolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

// toolResultIsError — IsError フラグを返す
func toolResultIsError(result *mcp.CallToolResult) bool {
	if result == nil {
		return false
	}
	return result.IsError
}

// makeCallReq — mcp.CallToolRequest を args map から作るヘルパー
func makeCallReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// ── D1: discover → 0 件 → 明示opt-in時だけ demand_signal 記録 ───────────────

func TestDiscoverCapability_D1_ZeroResult_RecordsDemandSignal(t *testing.T) {
	t.Setenv("OC_DISCOVER_EMIT", "on")
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{"query": "PDF の表を抽出する"})
	result, err := sdk.handleDiscoverCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D1: unexpected error: %v", err)
	}

	// demand signal が記録されたこと
	if len(mock.demandSignals) != 1 {
		t.Errorf("D1: expected 1 demand signal, got %d", len(mock.demandSignals))
	}
	sig := mock.demandSignals[0]
	if sig["zero_seller"] != true {
		t.Errorf("D1: demand signal should have zero_seller=true, got %v", sig["zero_seller"])
	}

	// レスポンスに demand_recorded=true が含まれること
	text := toolResultText(result)
	if !strings.Contains(text, "demand_recorded") {
		t.Errorf("D1: response should mention demand_recorded, got: %s", text)
	}
}

func TestDiscoverCapability_D1_MaxPricePropagatedToDemand(t *testing.T) {
	t.Setenv("OC_DISCOVER_EMIT", "on")
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{"query": "blockchain NFT", "max_price_dcents": float64(100)})
	_, err := sdk.handleDiscoverCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D1-price: %v", err)
	}

	if len(mock.demandSignals) != 1 {
		t.Fatalf("D1-price: expected 1 demand signal, got %d", len(mock.demandSignals))
	}
	// unmet_value_cents (max_price_dcents) が転記されること (flow discover-to-demand :filter-as-value)
	unmet := mock.demandSignals[0]["unmet_value_cents"]
	if unmet == nil {
		t.Errorf("D1-price: demand signal should have unmet_value_cents")
	}
}

func TestDiscoverEmitMode_FailClosedAndExplicit(t *testing.T) {
	tests := []struct {
		value string
		want  discoverEmitMode
	}{
		{"", discoverEmitOff},
		{"off", discoverEmitOff},
		{"unexpected", discoverEmitOff},
		{"private", discoverEmitPrivate},
		{"on", discoverEmitOn},
		{"TRUE", discoverEmitOn},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("OC_DISCOVER_EMIT", tt.value)
			if got := currentDiscoverEmitMode(); got != tt.want {
				t.Fatalf("mode=%q: got %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestRecordDemandSignal_PrivacyModesDoNotPost(t *testing.T) {
	for _, mode := range []string{"", "off", "private", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OC_DISCOVER_EMIT", mode)
			mock := newMockAPI(nil)
			ts := httptest.NewServer(mock)
			defer ts.Close()
			sdk := newTestSDK(ts)

			if sdk.recordDemandSignal(context.Background(), "PRIVATE project acquisition", 100) {
				t.Fatalf("mode=%q unexpectedly reported a remote demand signal", mode)
			}
			if len(mock.demandSignals) != 0 {
				t.Fatalf("mode=%q posted %d demand signals", mode, len(mock.demandSignals))
			}
		})
	}
}

// ── D2: call_capability → đ 課金 → billing 反映 ──────────────────────────────

func TestCallCapability_D2_FirstParty_WordCount(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{
		"capability_id": "cap-word-count-001",
		"input":         map[string]any{"text": "hello world foo bar"},
	})
	result, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D2: unexpected error: %v", err)
	}

	// POST /meter/calls が呼ばれたこと (đ 課金)
	if len(mock.callEvents) != 1 {
		t.Errorf("D2: expected 1 call event, got %d", len(mock.callEvents))
	}
	ce := mock.callEvents[0]
	if ce["currency"] != "dcent" {
		t.Errorf("D2: call event currency should be dcent, got %v", ce["currency"])
	}
	if ce["agreed_cents"] != float64(5) {
		t.Errorf("D2: agreed_cents should be 5, got %v", ce["agreed_cents"])
	}

	// レスポンスに charged_dcents と word count 結果が含まれること
	text := toolResultText(result)
	if !strings.Contains(text, "charged_dcents") {
		t.Errorf("D2: response should contain charged_dcents: %s", text)
	}
	if !strings.Contains(text, "words") {
		t.Errorf("D2: response should contain word count result: %s", text)
	}

	// no-conversion: cent/USD 換算フィールドが含まれていないこと
	if strings.Contains(strings.ToLower(text), "usd") || strings.Contains(text, "dollar") {
		t.Errorf("D2: response should NOT contain USD/dollar conversion (falsifier-3): %s", text)
	}
}

func TestCallCapability_D2_Free_NoBilling(t *testing.T) {
	freeCapability := map[string]any{
		"id": "cap-billing-001", "name": "@onecenter/operator.billing_summary",
		"description": "billing summary", "protocol": "mcp", "pricing_model": "free",
		"price_cents": int64(0), "sig_status": "no-pubkey", "seller_agent_id": "seller-001",
		"mcp_endpoint": "mcp://onecenter-operator/billing_summary",
		"reputation":   map[string]any{"success_rate": 1.0},
	}
	mock := newMockAPI([]map[string]any{freeCapability})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{
		"capability_id": "cap-billing-001",
		"input":         map[string]any{},
	})
	_, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D2-free: %v", err)
	}

	// free capability は meter/calls を呼ばない
	if len(mock.callEvents) != 0 {
		t.Errorf("D2-free: free capability should not post meter/calls, got %d events", len(mock.callEvents))
	}
}

// ── D3: discover 結果のフィールド検証 ────────────────────────────────────────

func TestDiscoverCapability_D3_ResponseFields(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{"query": "word count"})
	result, err := sdk.handleDiscoverCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D3: %v", err)
	}

	text := toolResultText(result)

	// 必須フィールドが含まれること
	for _, field := range []string{"price_dcents", "reputation", "sig_status", "callable"} {
		if !strings.Contains(text, field) {
			t.Errorf("D3: response should contain %q field: %s", field, text)
		}
	}

	// cent/USD 換算フィールドが含まれていないこと (falsifier-3)
	for _, forbidden := range []string{"price_usd", "price_dollar", "usd_equivalent", "price_cents_usd"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("D3: response should NOT contain %q (falsifier-3 no-conversion)", forbidden)
		}
	}

	// callable フラグが first-party に対して true であること
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("D3: response is not valid JSON: %s\nerr: %v", text, err)
	}
	caps, _ := out["capabilities"].([]any)
	if len(caps) == 0 {
		t.Fatal("D3: no capabilities in response")
	}
	firstCap := caps[0].(map[string]any)
	if firstCap["callable"] != true {
		t.Errorf("D3: @onecenter/operator.word_count should be callable=true, got %v", firstCap["callable"])
	}
}

func TestDiscoverCapability_D3_MCPThirdParty_CallableFalse(t *testing.T) {
	thirdPartyCap := map[string]any{
		"id": "cap-3rdparty-001", "name": "@example/some-service", "description": "some service",
		"protocol": "mcp", "pricing_model": "per-call", "price_cents": int64(20),
		"sig_status": "no-pubkey", "seller_agent_id": "other-seller-001",
		"mcp_endpoint": "mcp://example.com/service",
		"reputation":   map[string]any{"success_rate": 0.9},
	}
	mock := newMockAPI([]map[string]any{thirdPartyCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{"query": "some service"})
	result, err := sdk.handleDiscoverCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D3-3rdparty: %v", err)
	}

	text := toolResultText(result)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("D3-3rdparty: not valid JSON: %s\nerr: %v", text, err)
	}
	caps, _ := out["capabilities"].([]any)
	if len(caps) == 0 {
		t.Fatal("D3-3rdparty: no caps returned")
	}
	cap0 := caps[0].(map[string]any)
	if cap0["callable"] != false {
		t.Errorf("D3-3rdparty: mcp 3rd-party should be callable=false, got %v", cap0["callable"])
	}
}

// ── D4: 残高不足 → 402 → insufficient_dcent_balance エラー ──────────────────

func TestCallCapability_D4_InsufficientBalance(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	mock.meterCallStatus = http.StatusPaymentRequired // 402 を返す
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{
		"capability_id": "cap-word-count-001",
		"input":         map[string]any{"text": "test"},
	})
	result, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("D4: unexpected Go error: %v", err)
	}

	// IsError=true で返ること
	if !toolResultIsError(result) {
		t.Errorf("D4: expected error result for insufficient balance")
	}

	text := toolResultText(result)
	if !strings.Contains(text, "insufficient_dcent_balance") {
		t.Errorf("D4: error should mention insufficient_dcent_balance: %s", text)
	}
	// hint が含まれること (operator への追加 airdrop 要求方法)
	if !strings.Contains(text, "airdrop") {
		t.Errorf("D4: error should mention airdrop hint: %s", text)
	}
}

// ── 追加: routing テスト ──────────────────────────────────────────────────────

func TestCallCapability_MCPThirdParty_Returns_NotSupported(t *testing.T) {
	thirdPartyCap := map[string]any{
		"id": "cap-3rdparty-001", "name": "@example/3rd-party-service",
		"description": "3rd party service", "protocol": "mcp",
		"pricing_model": "per-call", "price_cents": int64(10),
		"seller_agent_id": "other-seller", "mcp_endpoint": "mcp://example.com/service",
		"reputation": map[string]any{"success_rate": 0.9},
	}
	mock := newMockAPI([]map[string]any{thirdPartyCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{
		"capability_id": "cap-3rdparty-001",
		"input":         map[string]any{},
	})
	result, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("mcp-3rdparty: %v", err)
	}

	// T4 defer: protocol_not_supported_yet エラーが返ること
	if !toolResultIsError(result) {
		t.Errorf("mcp-3rdparty: expected error for unsupported protocol")
	}
	text := toolResultText(result)
	if !strings.Contains(text, "protocol_not_supported_yet") {
		t.Errorf("mcp-3rdparty: expected protocol_not_supported_yet error: %s", text)
	}
}

func TestCallCapability_MaxPriceCap_Rejected(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap}) // word_count = 5đ
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	// max_price_dcents=3 で 5đ の capability を call → 拒否
	req := makeCallReq(map[string]any{
		"capability_id":    "cap-word-count-001",
		"input":            map[string]any{"text": "test"},
		"max_price_dcents": float64(3),
	})
	result, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("max-price: %v", err)
	}

	if !toolResultIsError(result) {
		t.Errorf("max-price: expected error when price exceeds cap")
	}
	text := toolResultText(result)
	if !strings.Contains(text, "price_exceeds_cap") {
		t.Errorf("max-price: expected price_exceeds_cap error: %s", text)
	}
}

func TestCallCapability_MissingCapabilityID(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	req := makeCallReq(map[string]any{"input": map[string]any{}})
	result, err := sdk.handleCallCapability(context.Background(), req)
	if err != nil {
		t.Fatalf("missing-cap-id: %v", err)
	}
	if !toolResultIsError(result) {
		t.Errorf("missing-cap-id: expected error for missing capability_id")
	}
}

// ── pure function テスト ──────────────────────────────────────────────────────

func TestWordCountResult(t *testing.T) {
	result := wordCountResult("hello world. bye!")
	if !strings.Contains(result, "words:") || !strings.Contains(result, "sentences:") {
		t.Errorf("wordCountResult: unexpected format: %s", result)
	}
	// "hello world. bye!" = 3 words
	if !strings.Contains(result, "words: 3") {
		t.Errorf("wordCountResult: expected 3 words, got: %s", result)
	}
}

func TestEchoResult(t *testing.T) {
	if echoResult("hello", "[test]") != "[test] hello" {
		t.Errorf("echoResult with prefix failed")
	}
	if echoResult("hello", "") != "[echo] hello" {
		t.Errorf("echoResult with empty prefix should use [echo]")
	}
}
