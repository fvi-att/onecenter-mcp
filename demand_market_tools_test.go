// demand_market_tools_test.go — Demand Market local tools の統合テスト (B50)
//
// 受け入れ基準 (design/20260628_demand_market_dsl.lisp *demand-market-acceptance*):
//
//	DM-A1: discover 0 件 → save_demand_locally → ~/.onecenter/demand/<id>.json (0600, uploaded=false) に保存され
//	       remote には何も送られない (default off privacy)
//	DM-A2: list_local_demands で点検後、upload_demand (opt-in) を呼んだときだけ既存 POST /demand/signals に
//	       descriptor が共有され demand_signal 化する。raw_friction/context は送らない
//
// DM-A3/A4/A5 は API 側 (既存 GET /demand/board teaser / seed-request rail / same-principal void) の責務で
// happy_path / demand_seed テストが担保する。本ファイルは新規 MCP client 層 (3 tool) を検証する。
//
// テスト方式: buyer_tools_test.go の mockAPI / newTestSDK を再利用し、demandBaseDir に t.TempDir() を注入して隔離する。
package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDemandTestSDK — demandBaseDir を隔離 tmpdir に向けた test SDK。
func newDemandTestSDK(t *testing.T, ts *httptest.Server) *ocSDK {
	sdk := newTestSDK(ts)
	sdk.demandBaseDir = t.TempDir()
	return sdk
}

// ── DM-A1: save_demand_locally はローカル保存のみ・remote 非接触 ──────────────
func TestSaveDemandLocally_DM_A1(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	req := makeCallReq(map[string]any{
		"descriptor":         "PDF の表を抽出する API",
		"raw_friction":       "決算PDFの表を機械可読にしたいのに見つからなかった",
		"unmet_value_dcents": float64(120),
	})
	result, err := sdk.handleSaveDemandLocally(context.Background(), req)
	if err != nil {
		t.Fatalf("DM-A1: unexpected error: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("DM-A1: unexpected tool error: %s", toolResultText(result))
	}

	var out struct {
		DemandRecordID       string `json:"demand_record_id"`
		Saved                bool   `json:"saved"`
		Uploaded             bool   `json:"uploaded"`
		ConsentRequired      bool   `json:"consent_required"`
		NextAction           string `json:"next_action"`
		AssistantInstruction string `json:"assistant_instruction"`
	}
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("DM-A1: bad output JSON: %v", err)
	}
	if !out.Saved || out.Uploaded || out.DemandRecordID == "" {
		t.Fatalf("DM-A1: expected saved=true uploaded=false with id, got %+v", out)
	}
	if !out.ConsentRequired || out.NextAction != "ask_user_for_upload_consent" {
		t.Fatalf("B57: expected an explicit conversational opt-in next action, got %+v", out)
	}
	for _, required := range []string{"Ask the user", "chance to earn dcent", "Do not call upload_demand unless", "explicitly agrees", "demand_record_id"} {
		if !strings.Contains(out.AssistantInstruction, required) {
			t.Fatalf("B57: assistant instruction must contain %q, got %q", required, out.AssistantInstruction)
		}
	}
	if strings.Contains(out.AssistantInstruction, "PDF") || strings.Contains(out.AssistantInstruction, "SECRET") {
		t.Fatalf("B57: assistant instruction must not echo private demand contents: %q", out.AssistantInstruction)
	}

	// remote には何も送られない
	if len(mock.demandSignals) != 0 {
		t.Fatalf("DM-A1: expected 0 remote demand signals, got %d", len(mock.demandSignals))
	}

	// ファイルが <base>/demand/<id>.json に 0600 で存在し uploaded=false
	path := sdk.demandFilePath(out.DemandRecordID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("DM-A1: demand record file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("DM-A1: expected file mode 0600, got %o", perm)
	}
	raw, _ := os.ReadFile(path)
	var rec map[string]any
	json.Unmarshal(raw, &rec)
	if rec["uploaded"] != false {
		t.Fatalf("DM-A1: stored record should have uploaded=false, got %v", rec["uploaded"])
	}
	if rec["raw_friction"] != "決算PDFの表を機械可読にしたいのに見つからなかった" {
		t.Fatalf("DM-A1: raw_friction not persisted locally")
	}
}

func TestSaveDemandLocally_RequiresDescriptor(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	result, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{"descriptor": "   "}))
	if !toolResultIsError(result) {
		t.Fatalf("expected error for empty descriptor")
	}
}

// ── list_local_demands — 保存済みを点検用に列挙する ──────────────────────────
func TestListLocalDemands(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	for _, d := range []string{"PDF 表抽出", "Excel→JSON 変換"} {
		if _, err := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{"descriptor": d})); err != nil {
			t.Fatalf("save failed: %v", err)
		}
	}

	result, err := sdk.handleListLocalDemands(context.Background(), makeCallReq(nil))
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	var out struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("bad list JSON: %v", err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(out.Records))
	}
	for _, r := range out.Records {
		if r["descriptor"] == nil || r["uploaded"] != false {
			t.Fatalf("record missing descriptor or wrong uploaded flag: %+v", r)
		}
	}
}

// ── DM-A2: upload_demand (opt-in) でのみ descriptor が共有される ──────────────
func TestUploadDemand_DM_A2_OptInShares(t *testing.T) {
	t.Setenv("OC_DISCOVER_EMIT", "on") // 明示同意
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	saveRes, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor":         "PDF 表抽出 API",
		"raw_friction":       "SECRET-PROJECT の決算処理で詰まった",
		"context":            "social-graph 案件",
		"unmet_value_dcents": float64(200),
	}))
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	json.Unmarshal([]byte(toolResultText(saveRes)), &saved)

	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
	}))
	if err != nil {
		t.Fatalf("DM-A2: upload error: %v", err)
	}
	if toolResultIsError(upRes) {
		t.Fatalf("DM-A2: unexpected upload tool error: %s", toolResultText(upRes))
	}

	// 既存 POST /demand/signals に 1 件共有された
	if len(mock.demandSignals) != 1 {
		t.Fatalf("DM-A2: expected 1 demand signal posted, got %d", len(mock.demandSignals))
	}
	sig := mock.demandSignals[0]
	if sig["semantic_descriptor"] != "PDF 表抽出 API" {
		t.Fatalf("DM-A2: descriptor not shared correctly: %v", sig["semantic_descriptor"])
	}
	if sig["zero_seller"] != true {
		t.Fatalf("DM-A2: expected zero_seller=true, got %v", sig["zero_seller"])
	}
	// raw_friction / context は送らない (privacy)
	if _, leaked := sig["raw_friction"]; leaked {
		t.Fatalf("DM-A2: raw_friction must NOT be uploaded")
	}
	if _, leaked := sig["context"]; leaked {
		t.Fatalf("DM-A2: context must NOT be uploaded")
	}

	// レコードは uploaded=true に書き戻される
	path := sdk.demandFilePath(saved.DemandRecordID)
	raw, _ := os.ReadFile(path)
	var rec map[string]any
	json.Unmarshal(raw, &rec)
	if rec["uploaded"] != true {
		t.Fatalf("DM-A2: record should be marked uploaded=true, got %v", rec["uploaded"])
	}

	// 二重 upload は弾く
	dupRes, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{"demand_record_id": saved.DemandRecordID}))
	if !toolResultIsError(dupRes) {
		t.Fatalf("DM-A2: re-upload should error")
	}
}

// ── DM-A2 (privacy gate): 同意がないと upload は remote 非接触 ────────────────
func TestUploadDemand_ConsentRequired(t *testing.T) {
	for _, mode := range []string{"", "off", "private"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OC_DISCOVER_EMIT", mode)
			mock := newMockAPI(nil)
			ts := httptest.NewServer(mock)
			defer ts.Close()
			sdk := newDemandTestSDK(t, ts)

			saveRes, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{"descriptor": "x"}))
			var saved struct {
				DemandRecordID string `json:"demand_record_id"`
			}
			json.Unmarshal([]byte(toolResultText(saveRes)), &saved)

			res, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{"demand_record_id": saved.DemandRecordID}))
			if !toolResultIsError(res) {
				t.Fatalf("mode=%q: upload should be blocked without consent", mode)
			}
			if !strings.Contains(toolResultText(res), "consent_required") {
				t.Fatalf("mode=%q: expected consent_required error, got %s", mode, toolResultText(res))
			}
			if len(mock.demandSignals) != 0 {
				t.Fatalf("mode=%q: nothing should be posted remotely, got %d", mode, len(mock.demandSignals))
			}
		})
	}
}

// ── B58: get_status — dcent 残高 + 需要アップロード実績 ──────────────────────────
func TestGetStatus_B58(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	// 0件の状態で呼んでも正常に返す
	result, err := sdk.handleGetStatus(context.Background(), makeCallReq(nil))
	if err != nil {
		t.Fatalf("B58: unexpected error: %v", err)
	}
	if toolResultIsError(result) {
		t.Fatalf("B58: unexpected tool error: %s", toolResultText(result))
	}
	var out struct {
		BalanceDcents        int64 `json:"balance_dcents"`
		DemandsSavedLocally  int   `json:"demands_saved_locally"`
		DemandsUploaded      int   `json:"demands_uploaded"`
		DemandsPendingUpload int   `json:"demands_pending_upload"`
	}
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("B58: bad output JSON: %v", err)
	}
	// mock wallet returns 995 balance
	if out.BalanceDcents != 995 {
		t.Errorf("B58: expected balance_dcents=995, got %d", out.BalanceDcents)
	}
	if out.DemandsSavedLocally != 0 || out.DemandsUploaded != 0 || out.DemandsPendingUpload != 0 {
		t.Errorf("B58: expected empty demands, got %+v", out)
	}

	// 2件保存・1件アップロード後の集計を確認
	for _, d := range []string{"PDF 表抽出", "Excel→JSON 変換"} {
		sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{"descriptor": d}))
	}
	t.Setenv("OC_DISCOVER_EMIT", "on")
	listRes, _ := sdk.handleListLocalDemands(context.Background(), makeCallReq(nil))
	var listed struct {
		Records []struct {
			DemandRecordID string `json:"demand_record_id"`
		} `json:"records"`
	}
	json.Unmarshal([]byte(toolResultText(listRes)), &listed)
	sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{"demand_record_id": listed.Records[0].DemandRecordID}))

	result2, _ := sdk.handleGetStatus(context.Background(), makeCallReq(nil))
	var out2 struct {
		BalanceDcents        int64 `json:"balance_dcents"`
		DemandsSavedLocally  int   `json:"demands_saved_locally"`
		DemandsUploaded      int   `json:"demands_uploaded"`
		DemandsPendingUpload int   `json:"demands_pending_upload"`
	}
	json.Unmarshal([]byte(toolResultText(result2)), &out2)
	if out2.DemandsSavedLocally != 2 {
		t.Errorf("B58: expected 2 saved, got %d", out2.DemandsSavedLocally)
	}
	if out2.DemandsUploaded != 1 {
		t.Errorf("B58: expected 1 uploaded, got %d", out2.DemandsUploaded)
	}
	if out2.DemandsPendingUpload != 1 {
		t.Errorf("B58: expected 1 pending, got %d", out2.DemandsPendingUpload)
	}
}

func TestUploadDemand_NotFound(t *testing.T) {
	t.Setenv("OC_DISCOVER_EMIT", "on")
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	res, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{"demand_record_id": "does-not-exist"}))
	if !toolResultIsError(res) {
		t.Fatalf("expected not_found error for missing record")
	}
	// 念のため: tmpdir に余計なファイルが無いこと
	dir := filepath.Dir(sdk.demandFilePath("_"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("expected empty demand dir, got %d entries", len(entries))
	}
}
