// quotation_tools.go — U2 P2P quotation MCP tools (Buyer<->Seller Quotation->agreement), split from main.go (B17).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerU2QuotationTools registers the U2/quotation toolset on the MCP server.
func registerU2QuotationTools(s *server.MCPServer, oc *ocSDK) {
	// create_quotation — Seller が Buyer への Quotation を作成・ローカルファイルに保存する (v2-r17)
	// billable=false (Quotation 作成は無課金)
	// 戻り値の quotation_json を Buyer に P2P transport で送り、Buyer は receive_quotation で受け取る
	s.AddTool(
		mcp.NewTool("create_quotation",
			mcp.WithDescription("Create a Quotation for a Buyer (call=purchase pre-agreement). Persists to local file (v2-r17 P2P-local: sent/<quotation_id>.json). Returns quotation_json to send to Buyer via P2P transport."),
			mcp.WithString("to_buyer_agent_id", mcp.Required(), mcp.Description("Buyer agent_id")),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("P2P session ID")),
			mcp.WithString("format", mcp.Required(), mcp.Description("'json' or 'freeform'")),
			mcp.WithNumber("total_dcents", mcp.Required(), mcp.Description("Total price in đ")),
			mcp.WithString("tasks_json", mcp.Description("JSON array of task objects (format=json only)")),
			mcp.WithString("freeform_note", mcp.Description("Free-form description (format=freeform)")),
			mcp.WithString("traceroute_id", mcp.Description("v2-r18: Agent chain trace ID (uuid). Buyer generates on first Quotation; Seller includes as-is to propagate traceability.")),
		),
		oc.handleCreateQuotation,
	)

	// receive_quotation — Buyer が Seller からの Quotation を受信・評価する (*quotation-spec* / v2-r16)
	// billable=false (Quotation 受信は無課金; *t3-billing-with-quotation* :billing-subjects)
	// v2-r17: P2P-local ファイル永続化 (received/<quotation_id>.json; 0600)
	s.AddTool(
		mcp.NewTool("receive_quotation",
			mcp.WithDescription("Receive and evaluate a Quotation from a Seller (call=purchase pre-agreement). Validates the quotation JSON and stores it locally (v2-r17 P2P-local: received/<quotation_id>.json). Accept by calling call_capability; reject or counter via P2P session."),
			mcp.WithString("quotation_json", mcp.Required(), mcp.Description("Quotation JSON string from the Seller (*quotation-spec* :fields-json)")),
		),
		oc.handleReceiveQuotation,
	)
}

func (s *ocSDK) handleCreateQuotation(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	toBuyerAgentID, _ := args["to_buyer_agent_id"].(string)
	sessionID, _ := args["session_id"].(string)
	format, _ := args["format"].(string)
	if toBuyerAgentID == "" || sessionID == "" || format == "" {
		return mcp.NewToolResultError(`{"error":"missing_required_field","message":"to_buyer_agent_id, session_id, format are required"}`), nil
	}
	if format != "json" && format != "freeform" {
		return mcp.NewToolResultError(`{"error":"invalid_format","message":"format must be 'json' or 'freeform'"}`), nil
	}

	totalDcentsRaw, _ := args["total_dcents"].(float64)
	totalDcents := int64(totalDcentsRaw)
	if totalDcents < 0 {
		return mcp.NewToolResultError(`{"error":"invalid_total_dcents","message":"total_dcents must be >= 0"}`), nil
	}

	freeformNote, _ := args["freeform_note"].(string)

	// tasks: JSON array string → parse
	var tasks []any
	if tasksRaw, ok := args["tasks_json"].(string); ok && tasksRaw != "" {
		if err := json.Unmarshal([]byte(tasksRaw), &tasks); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf(`{"error":"tasks_parse_error","message":%q}`, err.Error())), nil
		}
		// subtotal 整合性確認 (format=json)
		if format == "json" {
			var sum int64
			for _, task := range tasks {
				if t, ok := task.(map[string]any); ok {
					if sub, ok := t["subtotal_dcents"].(float64); ok {
						sum += int64(sub)
					}
				}
			}
			if sum != totalDcents {
				return mcp.NewToolResultError(
					fmt.Sprintf(`{"error":"subtotal_mismatch","sum_subtotals":%d,"total_dcents":%d}`, sum, totalDcents),
				), nil
			}
		}
	}

	quotationID := uuid.New().String()
	now := time.Now().UTC()
	q := map[string]any{
		"id":                   quotationID,
		"session_id":           sessionID,
		"from_seller_agent_id": s.agentID,
		"to_buyer_agent_id":    toBuyerAgentID,
		"issued_at":            now.Unix(),
		"expires_at":           now.Add(600 * time.Second).Unix(), // *quotation-spec*: 推奨 now+600s
		"format":               format,
		"total_dcents":         totalDcents,
		"status":               "pending",
	}
	// v2-r18: traceroute_id — Buyer が発番して Seller に渡す; Seller は Quotation に含めて伝播させる
	if tracerouteID, ok := args["traceroute_id"].(string); ok && tracerouteID != "" {
		q["traceroute_id"] = tracerouteID
	}
	if len(tasks) > 0 {
		q["tasks"] = tasks
	}
	if freeformNote != "" {
		q["freeform_note"] = freeformNote
	}

	// P2P-local 保存: Seller runtime のローカルメモリに保存。
	// seenQIDs は *受信側* の replay 防止集合 (receive_quotation が使う) なので、
	// Seller の発行 (sent) では汚染しない — さもないと同一プロセスが両ロールを兼ねる構成で
	// receive_quotation が自分の発行した quotation を duplicate と誤判定する。
	s.qmu.Lock()
	s.localQuotations["sent:"+quotationID] = q
	s.qmu.Unlock()

	// v2-r17: ファイル永続化 (~/.onecenter/p2p/<agent_id>/quotations/sent/<quotation_id>.json; 0600)
	if path := s.p2pFilePath("quotations", "sent", quotationID); path != "" {
		if err := saveP2PFile(path, q); err != nil {
			fmt.Fprintf(os.Stderr, "[p2p-store] save sent quotation failed: %v\n", err)
		}
	}

	// Seller が Buyer に渡す素材 (P2P transport で転送する JSON 文字列)
	quotationJSON, _ := json.MarshalIndent(q, "", "  ")
	out := map[string]any{
		"quotation_id":   quotationID,
		"total_dcents":   totalDcents,
		"expires_at":     q["expires_at"],
		"status":         "sent",
		"quotation_json": string(quotationJSON),
		"instructions":   fmt.Sprintf("Quotation created (ID: %s). Send quotation_json to Buyer via P2P transport (receive_quotation tool). Stored locally at sent/%s.json (v2-r17).", quotationID, quotationID),
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── receive_quotation — Buyer が Seller からの Quotation を受信・評価する (v2-r16) ──
//
// v2-r16 変更: OneCenter API への登録を廃止し、Buyer runtime ローカルメモリに保存する。
// (*quotation-spec* :storage — "P2P-local: Quotation は OneCenter API に登録しない。")
//
// billable=false (*t3-billing-with-quotation* :billing-subjects: receive_quotation=0)
// バリデーション:
//
//	① JSON parse
//	② 必須フィールド確認 (id / expires_at / total_dcents / format)
//	③ expires_at > now()
//	④ total_dcents ≥ 0
//	⑤ format=json のとき subtotal 整合性
//	⑥ quotation_id dedup (ローカル seenQIDs; replay 防止)
//
// 保存: s.localQuotations (runtime memory; P2P-local)
func (s *ocSDK) handleReceiveQuotation(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	quotationJSON, _ := args["quotation_json"].(string)
	if quotationJSON == "" {
		return mcp.NewToolResultError(`{"error":"quotation_parse_error","message":"quotation_json is required"}`), nil
	}

	// ① JSON parse
	var q map[string]any
	if err := json.Unmarshal([]byte(quotationJSON), &q); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"quotation_parse_error","message":%q}`, err.Error())), nil
	}

	// ② 必須フィールド確認
	for _, field := range []string{"id", "expires_at", "total_dcents", "format"} {
		if _, ok := q[field]; !ok {
			return mcp.NewToolResultError(fmt.Sprintf(`{"error":"missing_required_field","field":%q}`, field)), nil
		}
	}

	// ③ expires_at > now()
	expiresAtVal := q["expires_at"]
	var expiresAtUnix int64
	switch v := expiresAtVal.(type) {
	case float64:
		expiresAtUnix = int64(v)
	case int64:
		expiresAtUnix = v
	default:
		return mcp.NewToolResultError(`{"error":"invalid_expires_at","message":"expires_at must be a Unix timestamp"}`), nil
	}
	if time.Now().Unix() >= expiresAtUnix {
		return mcp.NewToolResultError(`{"error":"quotation_expired","message":"Quotation has already expired"}`), nil
	}

	// ④ total_dcents ≥ 0
	totalDcents, _ := q["total_dcents"].(float64)
	if totalDcents < 0 {
		return mcp.NewToolResultError(`{"error":"invalid_total_dcents","message":"total_dcents must be >= 0"}`), nil
	}

	// ⑤ format=json のとき subtotal 整合性
	if q["format"] == "json" {
		if tasks, ok := q["tasks"].([]any); ok {
			var sum float64
			for _, task := range tasks {
				if t, ok := task.(map[string]any); ok {
					if sub, ok := t["subtotal_dcents"].(float64); ok {
						sum += sub
					}
				}
			}
			if sum != totalDcents {
				return mcp.NewToolResultError(
					fmt.Sprintf(`{"error":"subtotal_mismatch","sum_subtotals":%.0f,"total_dcents":%.0f}`, sum, totalDcents),
				), nil
			}
		}
	}

	quotationID, _ := q["id"].(string)

	// ⑥ dedup — ローカル seenQIDs で replay 防止 (*quotation-spec* :storage replay 防止)
	s.qmu.Lock()
	if _, seen := s.seenQIDs[quotationID]; seen {
		s.qmu.Unlock()
		return mcp.NewToolResultError(`{"error":"duplicate_quotation_id","message":"quotation_id already received"}`), nil
	}
	s.seenQIDs[quotationID] = struct{}{}

	// P2P-local 保存: Buyer runtime のローカルメモリに status=pending で保存 (v2-r17: ファイル永続化)
	entry := make(map[string]any, len(q)+2)
	for k, v := range q {
		entry[k] = v
	}
	entry["status"] = "pending"
	entry["received_at"] = time.Now().Unix()
	s.localQuotations[quotationID] = entry
	s.qmu.Unlock()
	// v2-r17: ファイル永続化 (~/.onecenter/p2p/<agent_id>/quotations/received/<quotation_id>.json; 0600)
	if path := s.p2pFilePath("quotations", "received", quotationID); path != "" {
		if err := saveP2PFile(path, entry); err != nil {
			fmt.Fprintf(os.Stderr, "[p2p-store] save received quotation failed: %v\n", err)
		}
	}

	// 受信完了 — Buyer LLM が評価できるよう Quotation 情報を返す
	out := map[string]any{
		"received":     true,
		"quotation_id": quotationID,
		"total_dcents": int64(totalDcents),
		"format":       q["format"],
		"expires_at":   expiresAtUnix,
		"status":       "pending",
		// v2-r16: OneCenter API への PATCH ではなく、P2P セッション内での応答を指示する
		"instructions": fmt.Sprintf(
			"Quotation received locally (ID: %s). Review and decide: accept → call the capability via call_capability with the agreed price; reject → reply to Seller via P2P session with reason; counter → reply with a new price proposal. Quotation is stored in Buyer runtime memory only (v2-r16 P2P-local).",
			quotationID,
		),
	}
	if note, ok := q["freeform_note"].(string); ok && note != "" {
		out["freeform_note"] = note
	}
	if tasks, ok := q["tasks"]; ok {
		out["tasks"] = tasks
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── main ──────────────────────────────────────────────────────────────────────
