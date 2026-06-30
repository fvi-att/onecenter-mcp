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
		NextAction           string `json:"next_action"`
		AssistantInstruction string `json:"assistant_instruction"`
	}
	if err := json.Unmarshal([]byte(toolResultText(result)), &out); err != nil {
		t.Fatalf("DM-A1: bad output JSON: %v", err)
	}
	if !out.Saved || out.Uploaded || out.DemandRecordID == "" {
		t.Fatalf("DM-A1: expected saved=true uploaded=false with id, got %+v", out)
	}
	if out.NextAction != "call_upload_demand_for_preview" {
		t.Fatalf("DM-A1: expected next_action=call_upload_demand_for_preview, got %+v", out)
	}
	for _, required := range []string{"upload_demand", "demand_record_id", "preview"} {
		if !strings.Contains(out.AssistantInstruction, required) {
			t.Fatalf("DM-A1: assistant instruction must contain %q, got %q", required, out.AssistantInstruction)
		}
	}
	if strings.Contains(out.AssistantInstruction, "PDF") || strings.Contains(out.AssistantInstruction, "SECRET") {
		t.Fatalf("DM-A1: assistant instruction must not echo private demand contents: %q", out.AssistantInstruction)
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

// ── DM-A2: upload_demand preview → confirm → descriptor が共有される ──────────
func TestUploadDemand_DM_A2_PreviewThenConfirm(t *testing.T) {
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

	// confirm なし → プレビューを返す (remote 非接触)
	previewRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
	}))
	if err != nil {
		t.Fatalf("DM-A2 preview: unexpected error: %v", err)
	}
	if toolResultIsError(previewRes) {
		t.Fatalf("DM-A2 preview: unexpected tool error: %s", toolResultText(previewRes))
	}
	previewText := toolResultText(previewRes)
	// confirm_required アクションが含まれること
	if !strings.Contains(previewText, "confirm_required") {
		t.Fatalf("DM-A2 preview: expected confirm_required action, got: %s", previewText)
	}
	// will_send に descriptor が省略なく含まれること
	if !strings.Contains(previewText, "PDF 表抽出 API") {
		t.Fatalf("DM-A2 preview: descriptor must appear in will_send without omission")
	}
	// will_encrypt_on_server に raw_friction が含まれること (B71 deliver-proxy: server が暗号化する)
	if !strings.Contains(previewText, "will_encrypt_on_server") {
		t.Fatalf("DM-A2 preview: will_encrypt_on_server must appear in preview to inform user of encryption")
	}
	// プレビュー時点では remote に送らない
	if len(mock.demandSignals) != 0 {
		t.Fatalf("DM-A2 preview: no signal should be posted before confirm, got %d", len(mock.demandSignals))
	}

	// confirm=true → 実際に POST /demand/signals
	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil {
		t.Fatalf("DM-A2 confirm: upload error: %v", err)
	}
	if toolResultIsError(upRes) {
		t.Fatalf("DM-A2 confirm: unexpected upload tool error: %s", toolResultText(upRes))
	}

	// 既存 POST /demand/signals に 1 件共有された
	if len(mock.demandSignals) != 1 {
		t.Fatalf("DM-A2 confirm: expected 1 demand signal posted, got %d", len(mock.demandSignals))
	}
	sig := mock.demandSignals[0]
	if sig["semantic_descriptor"] != "PDF 表抽出 API" {
		t.Fatalf("DM-A2 confirm: descriptor not shared correctly: %v", sig["semantic_descriptor"])
	}
	if sig["zero_seller"] != true {
		t.Fatalf("DM-A2 confirm: expected zero_seller=true, got %v", sig["zero_seller"])
	}
	// B71 deliver-proxy: raw_friction と context も API に送る (server 側で暗号化して content_encrypted に保管)。
	if sig["raw_friction"] != "SECRET-PROJECT の決算処理で詰まった" {
		t.Fatalf("DM-A2 confirm: raw_friction must be sent for deliver-proxy (B71): %v", sig["raw_friction"])
	}
	if sig["context"] != "social-graph 案件" {
		t.Fatalf("DM-A2 confirm: context must be sent for deliver-proxy (B71): %v", sig["context"])
	}

	// レコードは uploaded=true に書き戻される
	path := sdk.demandFilePath(saved.DemandRecordID)
	raw, _ := os.ReadFile(path)
	var rec map[string]any
	json.Unmarshal(raw, &rec)
	if rec["uploaded"] != true {
		t.Fatalf("DM-A2 confirm: record should be marked uploaded=true, got %v", rec["uploaded"])
	}

	// 二重 upload は弾く (confirm=true でも)
	dupRes, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if !toolResultIsError(dupRes) {
		t.Fatalf("DM-A2: re-upload should error")
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
	listRes, _ := sdk.handleListLocalDemands(context.Background(), makeCallReq(nil))
	var listed struct {
		Records []struct {
			DemandRecordID string `json:"demand_record_id"`
		} `json:"records"`
	}
	json.Unmarshal([]byte(toolResultText(listRes)), &listed)
	sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": listed.Records[0].DemandRecordID,
		"confirm":          true,
	}))

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

// ── B67: OC_NO_UPLOAD_DEMAND opt-out ─────────────────────────────────────────
func TestUploadDemand_B67_OptOut(t *testing.T) {
	t.Setenv("OC_NO_UPLOAD_DEMAND", "1")

	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	saveRes, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor": "テスト需要",
	}))
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	json.Unmarshal([]byte(toolResultText(saveRes)), &saved)

	res, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if !toolResultIsError(res) {
		t.Fatalf("B67: expected upload_disabled error when OC_NO_UPLOAD_DEMAND=1")
	}
	if !strings.Contains(toolResultText(res), "OC_NO_UPLOAD_DEMAND=1") {
		t.Fatalf("B67: error message must mention OC_NO_UPLOAD_DEMAND=1, got: %s", toolResultText(res))
	}
	if len(mock.demandSignals) != 0 {
		t.Fatalf("B67: no signal must be posted when upload disabled, got %d", len(mock.demandSignals))
	}
}

func TestUploadDemand_B67_DefaultAllows(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	saveRes, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor": "デフォルト許可テスト",
	}))
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	json.Unmarshal([]byte(toolResultText(saveRes)), &saved)

	res, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil {
		t.Fatalf("B67 default: unexpected error: %v", err)
	}
	if toolResultIsError(res) {
		t.Fatalf("B67 default: upload must succeed without OC_NO_UPLOAD_DEMAND, got: %s", toolResultText(res))
	}
	if len(mock.demandSignals) != 1 {
		t.Fatalf("B67 default: expected 1 signal posted, got %d", len(mock.demandSignals))
	}
}

func TestUploadDemand_NotFound(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	res, _ := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": "does-not-exist",
		"confirm":          true,
	}))
	if !toolResultIsError(res) {
		t.Fatalf("expected not_found error for missing record")
	}
	// 念のため: tmpdir に余計なファイルが無いこと
	dir := filepath.Dir(sdk.demandFilePath("_"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("expected empty demand dir, got %d entries", len(entries))
	}
}

// ── B76: unlock 価格は platform 固定値。アップロード者は価格を設定しない ──────────
// save_demand_locally に min_unlock_dcents を渡しても保存されず、upload の POST body にも含まれないこと。
func TestUploadDemand_B76_NoPerSignalUnlockPrice(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	// save_demand_locally — 旧 min_unlock_dcents 引数を渡しても無視されること。
	saveRes, err := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor":         "機械翻訳 API (DE→JA 専門語対応)",
		"unmet_value_dcents": float64(50),
		"min_unlock_dcents":  float64(100), // 旧パラメータ; 無視される
	}))
	if err != nil {
		t.Fatalf("B76: save error: %v", err)
	}
	if toolResultIsError(saveRes) {
		t.Fatalf("B76: unexpected save tool error: %s", toolResultText(saveRes))
	}
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	if err := json.Unmarshal([]byte(toolResultText(saveRes)), &saved); err != nil {
		t.Fatalf("B76: bad save output JSON: %v", err)
	}

	// 保存レコードに min_unlock_dcents フィールドが無いこと。
	path := sdk.demandFilePath(saved.DemandRecordID)
	raw, _ := os.ReadFile(path)
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("B76: bad record JSON: %v", err)
	}
	if _, present := rec["min_unlock_dcents"]; present {
		t.Fatalf("B76: stored record must not carry min_unlock_dcents, got %v", rec["min_unlock_dcents"])
	}

	// upload_demand (confirm=true) → POST body に min_unlock_dcents が含まれないこと。
	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil {
		t.Fatalf("B76: upload error: %v", err)
	}
	if toolResultIsError(upRes) {
		t.Fatalf("B76: unexpected upload tool error: %s", toolResultText(upRes))
	}
	if len(mock.demandSignals) != 1 {
		t.Fatalf("B76: expected 1 demand signal posted, got %d", len(mock.demandSignals))
	}
	sig := mock.demandSignals[0]
	if _, present := sig["min_unlock_dcents"]; present {
		t.Fatalf("B76: posted signal must not carry min_unlock_dcents, got %v", sig["min_unlock_dcents"])
	}
}
