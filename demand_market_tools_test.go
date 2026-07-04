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
	"net/http"
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
		"descriptor":   "PDF の表を抽出する API",
		"raw_friction": "決算PDFの表を機械可読にしたいのに見つからなかった",
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
	if out.NextAction != "batch_review_at_next_pause" {
		t.Fatalf("DM-A1: expected next_action=batch_review_at_next_pause, got %+v", out)
	}
	for _, required := range []string{"upload_demand", "list_local_demands", "preview"} {
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
	if _, exists := rec["unmet_"+"value_dcents"]; exists {
		t.Fatalf("B88: local record retained removed buyer price: %v", rec)
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
		"descriptor":   "PDF 表抽出 API",
		"raw_friction": "SECRET-PROJECT の決算処理で詰まった",
		"context":      "social-graph 案件",
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
	if _, exists := sig["unmet_"+"value_dcents"]; exists {
		t.Fatalf("B88: upload sent removed buyer price: %v", sig)
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
	if mock.lastWalletPath != "/dcent/wallets/principal-1" {
		t.Errorf("B92: wallet path=%q, want principal_id", mock.lastWalletPath)
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

// ── B86: per-signal unlock 価格の復活。upload_demand で min_unlock_dcents を指定でき、
//
//	省略時は platform デフォルト (100đ) が POST body に載ること (DM-A6)。──────────
func TestUploadDemand_B86_PerSignalUnlockPrice(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	// (1) min_unlock_dcents を明示指定 → その値が POST body に載る。
	saveRes, err := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor": "機械翻訳 API (DE→JA 専門語対応)",
	}))
	if err != nil {
		t.Fatalf("B86: save error: %v", err)
	}
	if toolResultIsError(saveRes) {
		t.Fatalf("B86: unexpected save tool error: %s", toolResultText(saveRes))
	}
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	if err := json.Unmarshal([]byte(toolResultText(saveRes)), &saved); err != nil {
		t.Fatalf("B86: bad save output JSON: %v", err)
	}

	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id":  saved.DemandRecordID,
		"min_unlock_dcents": float64(250),
		"confirm":           true,
	}))
	if err != nil {
		t.Fatalf("B86: upload error: %v", err)
	}
	if toolResultIsError(upRes) {
		t.Fatalf("B86: unexpected upload tool error: %s", toolResultText(upRes))
	}
	if len(mock.demandSignals) != 1 {
		t.Fatalf("B86: expected 1 demand signal posted, got %d", len(mock.demandSignals))
	}
	if got := mock.demandSignals[0]["min_unlock_dcents"]; got != float64(250) {
		t.Fatalf("B86: posted signal must carry min_unlock_dcents=250, got %v", got)
	}

	// (2) min_unlock_dcents 省略 → platform デフォルト (100đ) が POST body に載る。
	saveRes2, _ := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor": "PDF 署名検証 API",
	}))
	var saved2 struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	json.Unmarshal([]byte(toolResultText(saveRes2)), &saved2)

	upRes2, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved2.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil {
		t.Fatalf("B86: upload (default) error: %v", err)
	}
	if toolResultIsError(upRes2) {
		t.Fatalf("B86: unexpected upload (default) tool error: %s", toolResultText(upRes2))
	}
	if len(mock.demandSignals) != 2 {
		t.Fatalf("B86: expected 2 demand signals posted, got %d", len(mock.demandSignals))
	}
	if got := mock.demandSignals[1]["min_unlock_dcents"]; got != float64(defaultMinUnlockDcents) {
		t.Fatalf("B86: omitted min_unlock_dcents must default to %d, got %v", defaultMinUnlockDcents, got)
	}
}

// ── B96 (A1): save_demand_locally accepts optional schema-of-a-good-friction fields
// and upload_demand carries them (as an opaque JSON string) in the same encrypted channel
// as raw_friction/context — never in the plaintext descriptor/zero_seller/min_unlock_dcents. ──
func TestSaveAndUploadDemand_B96_StructuredFields(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	saveRes, err := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor":      "決算PDFの表を機械可読にするAPI",
		"raw_friction":    "決算PDFの表を抽出しようとして詰まった",
		"intended_task":   "四半期決算資料からP/Lの表を抽出する",
		"failed_approach": "pdftotext と汎用OCRを試した",
		"missing_thing":   "表構造を保持したままJSON化するAPI",
		"workaround":      "手で転記した",
		"waste_amount":    "約2時間",
	}))
	if err != nil {
		t.Fatalf("B96: save error: %v", err)
	}
	if toolResultIsError(saveRes) {
		t.Fatalf("B96: unexpected save tool error: %s", toolResultText(saveRes))
	}
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	if err := json.Unmarshal([]byte(toolResultText(saveRes)), &saved); err != nil {
		t.Fatalf("B96: bad save output JSON: %v", err)
	}

	// preview (confirm=false): structured fields must appear only under will_encrypt_on_server,
	// never under will_send (which is sent as plaintext to the server).
	previewRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
	}))
	if err != nil {
		t.Fatalf("B96: preview error: %v", err)
	}
	var preview struct {
		WillSend            map[string]any `json:"will_send"`
		WillEncryptOnServer map[string]any `json:"will_encrypt_on_server"`
	}
	if err := json.Unmarshal([]byte(toolResultText(previewRes)), &preview); err != nil {
		t.Fatalf("B96: bad preview output JSON: %v", err)
	}
	if _, ok := preview.WillSend["intended_task"]; ok {
		t.Fatalf("B96: structured fields must not appear in plaintext will_send: %+v", preview.WillSend)
	}
	structured, ok := preview.WillEncryptOnServer["structured"].(map[string]any)
	if !ok {
		t.Fatalf("B96: expected will_encrypt_on_server.structured map, got %+v", preview.WillEncryptOnServer)
	}
	if structured["intended_task"] != "四半期決算資料からP/Lの表を抽出する" {
		t.Fatalf("B96: preview structured.intended_task mismatch: %+v", structured)
	}

	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil {
		t.Fatalf("B96: upload error: %v", err)
	}
	if toolResultIsError(upRes) {
		t.Fatalf("B96: unexpected upload tool error: %s", toolResultText(upRes))
	}
	if len(mock.demandSignals) != 1 {
		t.Fatalf("B96: expected 1 demand signal posted, got %d", len(mock.demandSignals))
	}
	posted := mock.demandSignals[0]
	if _, exposed := posted["intended_task"]; exposed {
		t.Fatalf("B96: posted body must not expose structured fields as top-level plaintext: %+v", posted)
	}
	structuredJSON, _ := posted["structured"].(string)
	if structuredJSON == "" {
		t.Fatalf("B96: posted body must carry a non-empty opaque 'structured' JSON string: %+v", posted)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(structuredJSON), &decoded); err != nil {
		t.Fatalf("B96: posted structured field is not valid JSON: %v", err)
	}
	if decoded["failed_approach"] != "pdftotext と汎用OCRを試した" {
		t.Fatalf("B96: posted structured.failed_approach mismatch: %+v", decoded)
	}
}

// ── B96: save_demand_locally still works with structured fields entirely omitted
// (optional, F1 exhaust protection — never forced). ────────────────────────────
func TestSaveAndUploadDemand_B96_StructuredFieldsOmitted(t *testing.T) {
	mock := newMockAPI(nil)
	ts := httptest.NewServer(mock)
	defer ts.Close()
	sdk := newDemandTestSDK(t, ts)

	saveRes, err := sdk.handleSaveDemandLocally(context.Background(), makeCallReq(map[string]any{
		"descriptor": "PDF の表を抽出する API",
	}))
	if err != nil || toolResultIsError(saveRes) {
		t.Fatalf("B96: save without structured fields must still succeed: err=%v result=%s", err, toolResultText(saveRes))
	}
	var saved struct {
		DemandRecordID string `json:"demand_record_id"`
	}
	json.Unmarshal([]byte(toolResultText(saveRes)), &saved)

	upRes, err := sdk.handleUploadDemand(context.Background(), makeCallReq(map[string]any{
		"demand_record_id": saved.DemandRecordID,
		"confirm":          true,
	}))
	if err != nil || toolResultIsError(upRes) {
		t.Fatalf("B96: upload without structured fields must still succeed: err=%v result=%s", err, toolResultText(upRes))
	}
	posted := mock.demandSignals[0]
	if structuredJSON, _ := posted["structured"].(string); structuredJSON != "" {
		t.Fatalf("B96: structured must be an empty string when no structured fields were provided, got %q", structuredJSON)
	}
}

// ── B96: unlock_demand returns the emitter's structured fields alongside raw_friction
// (deliver-proxy passthrough; server never parses the opaque JSON string). ────────
func TestUnlockDemand_B96_ReturnsStructuredAlongsideRawFriction(t *testing.T) {
	structuredJSON := `{"intended_task":"四半期決算資料からP/Lの表を抽出する","failed_approach":"pdftotext"}`
	mux := http.NewServeMux()
	mux.HandleFunc("/demand/signals/sig-1/seed-requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"seed_request_id": "sr-1",
			"status":          "delivered",
			"seed_request": map[string]any{
				"seed": map[string]any{
					"io":         "決算PDFの表を抽出しようとして詰まった",
					"context":    "四半期決算資料作業中",
					"structured": structuredJSON,
				},
			},
		})
	})
	mux.HandleFunc("/demand/seed-requests/sr-1/accept", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "released"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	sdk := newTestSDK(ts)

	res, err := sdk.handleUnlockDemand(context.Background(), makeCallReq(map[string]any{
		"signal_id":     "sig-1",
		"bounty_dcents": float64(150),
	}))
	if err != nil {
		t.Fatalf("B96: unlock error: %v", err)
	}
	if toolResultIsError(res) {
		t.Fatalf("B96: unexpected unlock tool error: %s", toolResultText(res))
	}
	var out struct {
		RawFriction string         `json:"raw_friction"`
		Structured  map[string]any `json:"structured"`
	}
	if err := json.Unmarshal([]byte(toolResultText(res)), &out); err != nil {
		t.Fatalf("B96: bad unlock output JSON: %v", err)
	}
	if out.RawFriction == "" {
		t.Fatalf("B96: expected non-empty raw_friction")
	}
	if out.Structured["intended_task"] != "四半期決算資料からP/Lの表を抽出する" {
		t.Fatalf("B96: expected structured.intended_task in unlock response, got %+v", out.Structured)
	}
	if out.Structured["failed_approach"] != "pdftotext" {
		t.Fatalf("B96: expected structured.failed_approach in unlock response, got %+v", out.Structured)
	}
}
