// demand_market_tools.go — Demand Market (検索ミス需要の有料公開) MCP tools (B50).
//
// Design: design/20260628_demand_market_dsl.lisp *demand-market-tools* / *demand-opt-in-upload* / DM-A1..A5
// 検索ミス (discover 0 件) を ① ローカル保存 → ② opt-in アップロード → ③ 他 Agent の đ unlock の
// ①② を担う MCP client 側のローカル機構。新規 HTTP endpoint は足さない (S-UD stake hedge):
// upload だけが remote 接触で、それも既存 POST /demand/signals (B19 RecordDemandSignal) を再利用する。
// unlock 課金 (③) は既存 seed-request rail (B20) をそのまま使う (API 側は実装済み)。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerDemandMarketTools registers save_demand_locally / list_local_demands / upload_demand / get_status.
func registerDemandMarketTools(s *server.MCPServer, oc *ocSDK) {
	// get_status — B58: dcent 残高 + 需要アップロード実績を一発確認する。billable=false。
	s.AddTool(
		mcp.NewTool("get_status",
			mcp.WithDescription("Show current đ balance and demand upload achievements (how many saved locally, how many shared with OneCenter). Free (no billing)."),
		),
		oc.handleGetStatus,
	)
	// save_demand_locally — discover 0 件の検索ミスをローカル DemandRecord に保存する (remote 非接触)。
	// billable=false。default off privacy: uploaded=false で保存し remote には何も送らない (DM-A1)。
	s.AddTool(
		mcp.NewTool("save_demand_locally",
			mcp.WithDescription("Save a search miss (discover returned 0 results) as a local DemandRecord at ~/.onecenter/demand/<id>.json (0600, uploaded=false). Nothing is sent remotely; sharing requires a later explicit upload_demand (DM-A1; B42 privacy default off)."),
			mcp.WithString("descriptor", mcp.Required(), mcp.Description("Semantic description of the miss (= the discover query)")),
			mcp.WithString("raw_friction", mcp.Description("Raw friction below query level ('got stuck here / wanted X'). Stays local; never uploaded.")),
			mcp.WithString("context", mcp.Description("Reproduction context (which work needed it). Stays local; never uploaded.")),
			mcp.WithNumber("unmet_value_dcents", mcp.Description("đ the user was willing to pay (max_price_dcents|0)")),
			mcp.WithBoolean("zero_seller", mcp.Description("Whether there were zero matching sellers (default true)")),
		),
		oc.handleSaveDemandLocally,
	)

	// list_local_demands — ローカル DemandRecord を列挙し、upload 前にユーザが内容を点検できるようにする。
	// billable=false。「見ずに同意」を避ける opt-in の前提 (DM-A2)。
	s.AddTool(
		mcp.NewTool("list_local_demands",
			mcp.WithDescription("List local DemandRecords so the user can review their contents before opting in to upload. Returns demand_record_id, descriptor, uploaded, created_at for each."),
		),
		oc.handleListLocalDemands,
	)

	// upload_demand — 指定 DemandRecord を既存 POST /demand/signals に共有して demand_signal 化する。
	// billable=false。共有するのは descriptor (+ unmet_value/zero_seller) のみ; raw_friction/context は送らない。
	// 機械的 opt-in gate: OC_DISCOVER_EMIT=on 相当の明示同意がないと remote に出さない (B42 hard-backstop)。
	s.AddTool(
		mcp.NewTool("upload_demand",
			mcp.WithDescription("Share a local DemandRecord to OneCenter (reuses existing POST /demand/signals; no new endpoint). Only descriptor + unmet_value + zero_seller are sent — raw_friction/context stay local. Requires explicit consent via OC_DISCOVER_EMIT=on (default off; B42). On success marks the record uploaded=true and records the demand_signal id."),
			mcp.WithString("demand_record_id", mcp.Required(), mcp.Description("Local DemandRecord id to upload")),
		),
		oc.handleUploadDemand,
	)
}

// handleGetStatus — B58: dcent 残高 + ローカル需要アップロード実績を返す。remote 呼び出し失敗時もローカル集計だけ返す。
func (s *ocSDK) handleGetStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	balanceDcents := s.getWalletBalance(ctx, s.buyerAgentID)

	dir := filepath.Dir(s.demandFilePath("_"))
	loaded := loadP2PDir(dir)
	var uploaded, pending int
	for _, r := range loaded {
		if up, _ := r["uploaded"].(bool); up {
			uploaded++
		} else {
			pending++
		}
	}

	out := map[string]any{
		"balance_dcents":         balanceDcents,
		"demands_saved_locally":  len(loaded),
		"demands_uploaded":       uploaded,
		"demands_pending_upload": pending,
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// demandFilePath — DemandRecord のローカル保存パス (<base>/demand/<id>.json; *demand-record-spec* :storage)。
// base は demandBaseDir (テスト注入) があればそれ、無ければ ~/.onecenter。
func (s *ocSDK) demandFilePath(id string) string {
	base := s.demandBaseDir
	if base == "" {
		base = mustOneCenterDataDir()
	}
	return filepath.Join(base, "demand", id+".json")
}

// handleSaveDemandLocally — DM-A1: ローカル保存のみ。remote 非接触。
func (s *ocSDK) handleSaveDemandLocally(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	descriptor, _ := args["descriptor"].(string)
	if strings.TrimSpace(descriptor) == "" {
		return mcp.NewToolResultError(`{"error":"missing_required_field","message":"descriptor is required"}`), nil
	}
	rawFriction, _ := args["raw_friction"].(string)
	ctxStr, _ := args["context"].(string)
	var unmetValue int64
	if v, ok := args["unmet_value_dcents"].(float64); ok && v > 0 {
		unmetValue = int64(v)
	}
	zeroSeller := true
	if v, ok := args["zero_seller"].(bool); ok {
		zeroSeller = v
	}

	id := uuid.New().String()
	rec := map[string]any{
		"id":                 id,
		"descriptor":         descriptor,
		"raw_friction":       rawFriction,
		"context":            ctxStr,
		"unmet_value_dcents": unmetValue,
		"zero_seller":        zeroSeller,
		"uploaded":           false,
		"uploaded_signal_id": "",
		"created_at":         time.Now().UTC().Format(time.RFC3339),
	}
	// saveP2PFile は 0700 dir 自動作成 + 0600 ファイル書き込み (p2p_store.go; Quotation と同型)。
	if err := saveP2PFile(s.demandFilePath(id), rec); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"save_failed","message":%q}`, err.Error())), nil
	}

	// privacy: 検索ミスの本文 (descriptor/raw_friction) を stdout/stderr に出さない (B42)。
	// MCP clients should turn the local save into a conversational opt-in instead of
	// waiting for the user to discover upload_demand on their own. Keep the instruction
	// free of demand contents: the record id is sufficient for the follow-up tool call.
	out := map[string]any{
		"demand_record_id": id,
		"saved":            true,
		"uploaded":         false,
		"consent_required": true,
		"next_action":      "ask_user_for_upload_consent",
		"assistant_instruction": "Ask the user: \"Share this demand with OneCenter for a chance to earn dcent if another agent unlocks it?\" " +
			"Do not call upload_demand unless the user explicitly agrees. If they agree, call upload_demand with the demand_record_id from this result.",
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// handleListLocalDemands — upload 前の点検用列挙。
func (s *ocSDK) handleListLocalDemands(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dir := filepath.Dir(s.demandFilePath("_"))
	loaded := loadP2PDir(dir)
	records := make([]map[string]any, 0, len(loaded))
	for id, r := range loaded {
		records = append(records, map[string]any{
			"demand_record_id": id,
			"descriptor":       r["descriptor"],
			"uploaded":         r["uploaded"],
			"created_at":       r["created_at"],
		})
	}
	out := map[string]any{"records": records}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// handleUploadDemand — DM-A2: opt-in 同意 (OC_DISCOVER_EMIT=on) のときだけ既存 POST /demand/signals へ
// descriptor を共有し demand_signal 化する。raw_friction/context は送らない (privacy)。
func (s *ocSDK) handleUploadDemand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id, _ := args["demand_record_id"].(string)
	if strings.TrimSpace(id) == "" {
		return mcp.NewToolResultError(`{"error":"missing_required_field","message":"demand_record_id is required"}`), nil
	}

	// 機械的 opt-in gate (B42 hard-backstop): 明示同意がないと remote に一切出さない (fail-closed)。
	if currentDiscoverEmitMode() != discoverEmitOn {
		return mcp.NewToolResultError(`{"error":"consent_required","message":"set OC_DISCOVER_EMIT=on to consent to sharing this demand record (default off; B42 privacy)"}`), nil
	}

	path := s.demandFilePath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		return mcp.NewToolResultError(`{"error":"not_found","message":"demand_record not found"}`), nil
	}
	var rec map[string]any
	if json.Unmarshal(raw, &rec) != nil {
		return mcp.NewToolResultError(`{"error":"corrupt_record","message":"failed to parse demand_record"}`), nil
	}
	if up, _ := rec["uploaded"].(bool); up {
		return mcp.NewToolResultError(`{"error":"already_uploaded","message":"demand_record already uploaded"}`), nil
	}

	// 共有するのは descriptor + unmet_value + zero_seller のみ (*demand-signal-spec*)。
	body, _ := json.Marshal(map[string]any{
		"semantic_descriptor": rec["descriptor"],
		"unmet_value_cents":   rec["unmet_value_dcents"],
		"zero_seller":         rec["zero_seller"],
	})
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/demand/signals", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"api_unreachable","message":%q}`, err.Error())), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"upload_failed","status":%d}`, resp.StatusCode)), nil
	}
	var sig struct {
		ID string `json:"id"`
	}
	respRaw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(respRaw, &sig)

	// 書き戻し: uploaded=true + demand_signal 参照 (突合用)。signal id は uuid 文字列。
	rec["uploaded"] = true
	rec["uploaded_signal_id"] = sig.ID
	if err := saveP2PFile(path, rec); err != nil {
		fmt.Fprintf(os.Stderr, "[demand-market] writeback failed: %v\n", err)
	}

	out := map[string]any{
		"demand_record_id": id,
		"uploaded":         true,
		"demand_signal_id": sig.ID,
		"board_url":        s.oncenterURL + "/demand/board",
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}
