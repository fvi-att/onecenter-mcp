// regenerate_identity_test.go — v2-pki-r2: regenerate_identity tool の統合テスト
//
// 設計根拠:
//   design/20260607_agent_pki_dsl.lisp SECTION 6.5 (*identity-regeneration-tool*) /
//   oc/regenerate-identity (SECTION 6)
//
// 受け入れ観点:
//   R1: role=buyer (default) → buyer agent_id + keypair が変わり、seller は不変。pubkey 再登録。
//   R2: role=seller          → seller agent_id + keypair が変わり、first-party capability 再 seed。buyer 不変。
//   R3: role=both            → buyer/seller 両方が変わる。
//   R4: invalid role         → regenerate_failed エラー (state を差し替えない)。
//   R5: API 到達不能          → regenerate_failed (中途半端な identity を作らない)。
//   R6: 再生成後の call_capability が *新しい* buyer identity で署名・記録される。
//
// テスト方式: buyer_tools_test.go の mockAPI / newTestSDK / helper を共有する。

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── R1: role=buyer (default) ─────────────────────────────────────────────────

func TestRegenerateIdentity_R1_Buyer(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID
	oldBuyerPub := sdk.buyerPubKeyB64
	oldSeller := sdk.agentID
	oldSellerPub := sdk.pubKeyB64

	req := makeCallReq(map[string]any{"role": "buyer", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R1: unexpected error: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("R1: tool returned error: %s", toolResultText(result))
	}

	// buyer agent_id + keypair が変わったこと
	if sdk.buyerAgentID == oldBuyer {
		t.Errorf("R1: buyer agent_id should change, still %s", sdk.buyerAgentID)
	}
	if sdk.buyerPubKeyB64 == oldBuyerPub {
		t.Errorf("R1: buyer keypair should change")
	}
	// seller は不変 (role=buyer)
	if sdk.agentID != oldSeller || sdk.pubKeyB64 != oldSellerPub {
		t.Errorf("R1: seller identity should be unchanged for role=buyer")
	}
	// buyer cred が取得されたこと (新 buyer wallet 参照に必要)
	if sdk.buyerCred == "" {
		t.Errorf("R1: buyer cred should be set after regenerate")
	}
	// pubkey が登録されたこと
	if mock.pubkeyRegs != 1 {
		t.Errorf("R1: expected 1 pubkey registration, got %d", mock.pubkeyRegs)
	}
	// seller を再 seed していないこと
	if mock.seededCaps != 0 {
		t.Errorf("R1: role=buyer should not re-seed capabilities, got %d", mock.seededCaps)
	}
	// 登録 role が buyer であること
	if len(mock.registeredRoles) != 1 || mock.registeredRoles[0] != "buyer" {
		t.Errorf("R1: expected one buyer registration, got %v", mock.registeredRoles)
	}
	// レスポンスに新 buyer_agent_id が含まれること
	text := toolResultText(result)
	if !strings.Contains(text, sdk.buyerAgentID) {
		t.Errorf("R1: response should contain new buyer_agent_id %s: %s", sdk.buyerAgentID, text)
	}
	if !strings.Contains(text, "buyer_pubkey_hint") {
		t.Errorf("R1: response should contain buyer_pubkey_hint: %s", text)
	}
}

func TestRegenerateIdentity_R1_DefaultRoleIsBuyer(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID
	oldSeller := sdk.agentID

	// role 省略 → buyer がデフォルト; confirm=true で実行
	req := makeCallReq(map[string]any{"confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("default-role: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("default-role: tool error: %s", toolResultText(result))
	}
	if sdk.buyerAgentID == oldBuyer {
		t.Errorf("default-role: buyer should regenerate by default")
	}
	if sdk.agentID != oldSeller {
		t.Errorf("default-role: seller should be unchanged by default")
	}
}

// ── R2: role=seller → seller 再生成 + first-party 再 seed ─────────────────────

func TestRegenerateIdentity_R2_Seller_ReSeeds(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldSeller := sdk.agentID
	oldSellerPub := sdk.pubKeyB64
	oldBuyer := sdk.buyerAgentID

	req := makeCallReq(map[string]any{"role": "seller", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R2: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("R2: tool error: %s", toolResultText(result))
	}

	if sdk.agentID == oldSeller || sdk.pubKeyB64 == oldSellerPub {
		t.Errorf("R2: seller identity should change")
	}
	// buyer は不変
	if sdk.buyerAgentID != oldBuyer {
		t.Errorf("R2: buyer should be unchanged for role=seller")
	}
	// first-party capability を再 seed したこと (3 件; word_count/echo_text/billing_summary)
	if mock.seededCaps != 3 {
		t.Errorf("R2: expected 3 re-seeded capabilities, got %d", mock.seededCaps)
	}
	if len(mock.registeredRoles) != 1 || mock.registeredRoles[0] != "seller" {
		t.Errorf("R2: expected one seller registration, got %v", mock.registeredRoles)
	}
	text := toolResultText(result)
	if !strings.Contains(text, "seller_agent_id") {
		t.Errorf("R2: response should contain seller_agent_id: %s", text)
	}
}

// ── R3: role=both → 両方再生成 ──────────────────────────────────────────────

func TestRegenerateIdentity_R3_Both(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID
	oldSeller := sdk.agentID

	req := makeCallReq(map[string]any{"role": "both", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R3: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("R3: tool error: %s", toolResultText(result))
	}

	if sdk.buyerAgentID == oldBuyer {
		t.Errorf("R3: buyer agent_id should change")
	}
	if sdk.agentID == oldSeller {
		t.Errorf("R3: seller agent_id should change")
	}
	// buyer pubkey + seller pubkey = 2 回登録
	if mock.pubkeyRegs != 2 {
		t.Errorf("R3: expected 2 pubkey registrations (buyer+seller), got %d", mock.pubkeyRegs)
	}
	roles := strings.Join(mock.registeredRoles, ",")
	if !strings.Contains(roles, "buyer") || !strings.Contains(roles, "seller") {
		t.Errorf("R3: expected both buyer and seller registrations, got %v", mock.registeredRoles)
	}
}

// ── R4: invalid role → エラー (state を差し替えない) ─────────────────────────

func TestRegenerateIdentity_R4_InvalidRole(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	oldBuyer := sdk.buyerAgentID
	oldSeller := sdk.agentID

	req := makeCallReq(map[string]any{"role": "admin", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R4: unexpected Go error: %v", err)
	}
	if !toolResultIsError(result) {
		t.Errorf("R4: expected error for invalid role")
	}
	text := toolResultText(result)
	if !strings.Contains(text, "regenerate_failed") {
		t.Errorf("R4: error should be regenerate_failed: %s", text)
	}
	// identity は差し替わっていないこと
	if sdk.buyerAgentID != oldBuyer || sdk.agentID != oldSeller {
		t.Errorf("R4: identity must be unchanged on invalid role")
	}
	// 何も登録していないこと
	if mock.pubkeyRegs != 0 || len(mock.registeredRoles) != 0 {
		t.Errorf("R4: invalid role should not register anything")
	}
}

// ── R5: API 到達不能 → regenerate_failed (state 差し替えなし) ─────────────────

func TestRegenerateIdentity_R5_APIUnreachable(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	sdk := newTestSDK(ts)
	ts.Close() // 即 close → selfRegister が失敗する

	oldBuyer := sdk.buyerAgentID

	req := makeCallReq(map[string]any{"role": "buyer", "confirm": true})
	result, err := sdk.handleRegenerateIdentity(context.Background(), req)
	if err != nil {
		t.Fatalf("R5: unexpected Go error: %v", err)
	}
	if !toolResultIsError(result) {
		t.Errorf("R5: expected regenerate_failed when API unreachable")
	}
	// buyer agent_id は差し替わっていないこと (中途半端な identity を作らない)
	if sdk.buyerAgentID != oldBuyer {
		t.Errorf("R5: buyer agent_id must be unchanged when self-register fails, got %s", sdk.buyerAgentID)
	}
}

// ── R6: 再生成後の call_capability が新 buyer identity で記録される ──────────

func TestRegenerateIdentity_R6_NewIdentityUsedInCall(t *testing.T) {
	mock := newMockAPI([]map[string]any{testWordCountCap})
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newTestSDK(ts)

	// buyer を再生成
	if _, err := sdk.handleRegenerateIdentity(context.Background(), makeCallReq(map[string]any{"role": "buyer", "confirm": true})); err != nil {
		t.Fatalf("R6 regenerate: %v", err)
	}
	newBuyer := sdk.buyerAgentID

	// 再生成後に call_capability を実行
	callReq := makeCallReq(map[string]any{
		"capability_id": "cap-word-count-001",
		"input":         map[string]any{"text": "hello world foo"},
	})
	if _, err := sdk.handleCallCapability(context.Background(), callReq); err != nil {
		t.Fatalf("R6 call: %v", err)
	}

	if len(mock.callEvents) != 1 {
		t.Fatalf("R6: expected 1 call event, got %d", len(mock.callEvents))
	}
	ce := mock.callEvents[0]
	// call event の buyer_agent_id が *新しい* identity であること
	if ce["buyer_agent_id"] != newBuyer {
		t.Errorf("R6: call event buyer_agent_id should be new identity %s, got %v", newBuyer, ce["buyer_agent_id"])
	}
	// dual-sig (SigA + SigB) が付与されていること
	if ce["sig_a"] == nil || ce["sig_b"] == nil {
		t.Errorf("R6: call event should carry sig_a and sig_b after regenerate")
	}
}
