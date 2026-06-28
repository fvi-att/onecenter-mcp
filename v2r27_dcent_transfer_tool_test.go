package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTransferDcentTool_R27SimpleTransfer(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/dcent/transfers" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
			return
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode transfer body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"transfer_id":                 gotBody["transfer_id"],
			"from_agent_id":               "buyer-001",
			"to_agent_id":                 gotBody["to_agent_id"],
			"dcents":                      gotBody["dcents"],
			"sender_balance_dcents_after": 75,
		})
	}))
	defer ts.Close()

	sdk := &ocSDK{
		apiKey:       "oc_agt_seller",
		buyerCred:    "oc_agt_buyer",
		buyerAgentID: "buyer-001",
		oncenterURL:  ts.URL,
		client:       ts.Client(),
	}

	result, err := sdk.handleTransferDcent(context.Background(), makeCallReq(map[string]any{
		"to_agent_id": "receiver-001",
		"dcents":      float64(25),
		"memo":        "tip",
		"transfer_id": "transfer-001",
	}))
	if err != nil {
		t.Fatalf("transfer_dcent returned error: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("transfer_dcent tool error: %s", toolResultText(result))
	}
	if gotAuth != "Bearer oc_agt_buyer" {
		t.Fatalf("expected buyer bearer auth, got %q", gotAuth)
	}
	if gotBody["from_agent_id"] != nil {
		t.Fatalf("from_agent_id must not be sent; got body=%v", gotBody)
	}
	if gotBody["to_agent_id"] != "receiver-001" || gotBody["dcents"] != float64(25) || gotBody["memo"] != "tip" {
		t.Fatalf("unexpected transfer body: %v", gotBody)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if out["from_agent_id"] != "buyer-001" || out["to_agent_id"] != "receiver-001" {
		t.Fatalf("unexpected tool output: %v", out)
	}
}

func TestTransferDcentTool_R27InsufficientBalance(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]any{
			"code":            "insufficient_dcent_balance",
			"balance_dcents":  3,
			"required_dcents": 25,
		})
	}))
	defer ts.Close()

	sdk := &ocSDK{
		buyerCred:   "oc_agt_buyer",
		oncenterURL: ts.URL,
		client:      ts.Client(),
	}

	result, err := sdk.handleTransferDcent(context.Background(), makeCallReq(map[string]any{
		"to_agent_id": "receiver-001",
		"dcents":      float64(25),
	}))
	if err != nil {
		t.Fatalf("transfer_dcent returned error: %v", err)
	}
	if !toolResultIsError(result) {
		t.Fatalf("expected tool error, got %s", toolResultText(result))
	}
	if text := toolResultText(result); text == "" || !json.Valid([]byte(text)) {
		t.Fatalf("expected JSON error body, got %q", text)
	}
}
