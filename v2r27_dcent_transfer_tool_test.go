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
			"from_principal_id":           "principal-1",
			"to_principal_id":             gotBody["to_principal_id"],
			"dcents":                      gotBody["dcents"],
			"sender_balance_dcents_after": 75,
		})
	}))
	defer ts.Close()

	sdk := &ocSDK{
		cred:        "oc_prn_test",
		principalID: "principal-1",
		oncenterURL: ts.URL,
		client:      ts.Client(),
	}

	result, err := sdk.handleTransferDcent(context.Background(), makeCallReq(map[string]any{
		"to_principal_id": "principal-2",
		"dcents":          float64(25),
		"memo":            "tip",
		"transfer_id":     "transfer-001",
	}))
	if err != nil {
		t.Fatalf("transfer_dcent returned error: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("transfer_dcent tool error: %s", toolResultText(result))
	}
	if gotAuth != "Bearer oc_prn_test" {
		t.Fatalf("expected principal bearer auth, got %q", gotAuth)
	}
	if gotBody["to_principal_id"] != "principal-2" || gotBody["dcents"] != float64(25) || gotBody["memo"] != "tip" {
		t.Fatalf("unexpected transfer body: %v", gotBody)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if out["from_principal_id"] != "principal-1" || out["to_principal_id"] != "principal-2" {
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
		cred:        "oc_prn_test",
		oncenterURL: ts.URL,
		client:      ts.Client(),
	}

	result, err := sdk.handleTransferDcent(context.Background(), makeCallReq(map[string]any{
		"to_principal_id": "principal-2",
		"dcents":          float64(25),
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
