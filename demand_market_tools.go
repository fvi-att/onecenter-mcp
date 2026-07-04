// demand_market_tools.go — Demand Market (検索ミス需要の有料公開) MCP tools (B50/B71).
//
// Design: design/20260628_demand_market_dsl.lisp *demand-market-tools* / *demand-opt-in-upload* / DM-A1..A7
// 検索ミス = タスク遂行失敗＝リソース不足の発見 (C-abstract-search-miss; discover 0 件はその代表例; B66)。
// これを ① ローカル保存 → ② opt-in アップロード → ③ 他 Agent の đ unlock の
// ①② を担う MCP client 側のローカル機構。新規 HTTP endpoint は足さない (S-UD stake hedge):
// upload だけが remote 接触で、それも既存 POST /demand/signals (B19 RecordDemandSignal) を再利用する。
// unlock 課金 (③) は list_demand_board + unlock_demand ツール (B71) で実現し、
// 既存 seed-request rail (B20) + API-side deliver proxy (B70) を使う。
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

// defaultMinUnlockDcents — upload_demand で min_unlock_dcents を省略したときに適用する
// platform デフォルトの unlock 床値 (B86; API 側 models.DefaultMinUnlockDcents と一致させる)。
const defaultMinUnlockDcents int64 = 100

// registerDemandMarketTools registers save_demand_locally / list_local_demands / upload_demand /
// list_demand_board / unlock_demand / get_status.
func registerDemandMarketTools(s *server.MCPServer, oc *ocSDK) {
	// get_status — B58: dcent 残高 + 需要アップロード実績を一発確認する。billable=false。
	s.AddTool(
		mcp.NewTool("get_status",
			mcp.WithDescription("Show current đ balance and demand upload achievements (how many saved locally, how many shared with OneCenter). Free (no billing)."),
		),
		oc.handleGetStatus,
	)
	// save_demand_locally — タスク遂行失敗＝リソース不足の発見 (検索ミス) をローカル DemandRecord に保存する (remote 非接触)。
	// 検索ミスは discover 0 件に限らない (C-abstract-search-miss; design SECTION 0 / B66)。
	// billable=false。default off privacy: uploaded=false で保存し remote には何も送らない (DM-A1)。
	s.AddTool(
		mcp.NewTool("save_demand_locally",
			mcp.WithDescription("Save a 'search miss' — ANY moment where you tried to accomplish a task and discovered a needed service, content, or resource is missing from the world. This is NOT limited to OneCenter discover returning 0 results: a fruitless web search, a missing/nonexistent API, unavailable data, or no suitable tool/listing all count equally. Mechanical trigger conditions (B95/B1) — the moment any of these happens, proactively propose (in one short line, without waiting to be asked) saving it as a demand record: a web search comes up empty, discover_capability returns 0 results, you retry the same approach repeatedly without progress, you switch to a workaround, or you tell the user 'I can't do this / I don't know'. Only call this tool after the user agrees to the proposal — never call it silently; agreeing to save does not imply agreeing to upload_demand, which is a separate later opt-in (DM-A1; B42 privacy default off)."),
			mcp.WithString("descriptor", mcp.Required(), mcp.Description("Semantic description of what was needed but missing (the task you could not complete for lack of a resource)")),
			mcp.WithString("raw_friction", mcp.Description("Raw friction below query level ('got stuck here / wanted X'). *Agent Instruction: Automatically flesh out and detail the exact pain point based on the user's brief prompt. NEVER include personal or confidential information.* Stays local; never uploaded.")),
			mcp.WithString("context", mcp.Description("Reproduction context (which work needed it). *Agent Instruction: Automatically provide a comprehensive background of the user's situation. NEVER include personal or confidential information.* Stays local; never uploaded.")),
			mcp.WithBoolean("zero_seller", mcp.Description("Whether there were zero matching sellers (default true)")),
			mcp.WithString("intended_task", mcp.Description("Optional (B96 schema-of-a-good-friction): what task were you trying to accomplish? Omit if not readily known — never fabricate.")),
			mcp.WithString("failed_approach", mcp.Description("Optional: which approach/tool did you try that failed?")),
			mcp.WithString("missing_thing", mcp.Description("Optional: what exactly was missing or nonexistent?")),
			mcp.WithString("workaround", mcp.Description("Optional: what did you do instead (the workaround)?")),
			mcp.WithString("waste_amount", mcp.Description("Optional: how much time/effort did this cost (rough estimate)?")),
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
	// billable=false。confirm=false (省略可) → 投稿内容のプレビューを返し AI にユーザー確認を求めさせる。
	// confirm=true → 実際に POST /demand/signals へ送信し uploaded=true に書き戻す。
	// deliver-proxy (B71): raw_friction と context も API に送り server が暗号化して保管する (opt-in 済み前提)。
	s.AddTool(
		mcp.NewTool("upload_demand",
			mcp.WithDescription("Share a local DemandRecord to OneCenter (POST /demand/signals). descriptor, raw_friction, and context are sent — the server encrypts raw_friction/context for the deliver-proxy so buyers can unlock without the uploader being online. Omit confirm (or confirm=false) to preview what will be shared. Pass confirm=true to execute. On success marks uploaded=true and records the demand_signal id. Upload is always opt-in — never call this automatically. Propose it in a single batched review (via list_local_demands) at a natural pause in the work, not once per save_demand_locally call, so the proposal doesn't become noise (PB-4 noise budget)."),
			mcp.WithString("demand_record_id", mcp.Required(), mcp.Description("Local DemandRecord id to upload")),
			mcp.WithNumber("min_unlock_dcents", mcp.Description("đ floor a buyer must pay to unlock this demand's raw friction (the uploader sets the price). Optional; omit to apply the platform default of 100đ.")),
			mcp.WithBoolean("confirm", mcp.Description("true = execute upload; omit or false = preview only (safety default)")),
		),
		oc.handleUploadDemand,
	)

	// list_demand_board — GET /demand/board → teaser 一覧 (無料; auth 不要)。
	// Hiro が đ を払う前に『どんな需要が存在するか』をブラウズするためのツール (DM-A7; B71)。
	s.AddTool(
		mcp.NewTool("list_demand_board",
			mcp.WithDescription("Browse the demand board on OneCenter — see what AI agents kept failing to find. Returns one teaser per demand signal (signal_id, descriptor, min_unlock_dcents — the per-signal đ price to unlock its raw friction); raw friction details are paywalled. Free; no billing."),
		),
		oc.handleListDemandBoard,
	)

	// unlock_demand — đ を払って need の raw_friction を 1 呼び出しで取得する (DM-A7; B71 / B74 signal キー)。
	// 内部: CreateSeedRequest → (API deliver-proxy auto-deliver) → AcceptSeedRequest → TransferDcent。
	// billable=true。bounty_dcents が signal の min_unlock_dcents を下回る場合は API が 422 で拒否 (B86)。
	s.AddTool(
		mcp.NewTool("unlock_demand",
			mcp.WithDescription("Pay đ to unlock the raw friction details behind a demand signal in one call. Internally creates a seed-request, the API auto-delivers the encrypted content, then accepts and transfers đ to the uploader. Returns raw_friction you can use to spec a capability. Requires sufficient đ balance."),
			mcp.WithString("signal_id", mcp.Required(), mcp.Description("Demand signal id from list_demand_board")),
			mcp.WithNumber("bounty_dcents", mcp.Required(), mcp.Description("đ to offer; must be >= the signal's min_unlock_dcents (from list_demand_board). A bounty below the floor is rejected with 422.")),
		),
		oc.handleUnlockDemand,
	)
}

// handleGetStatus — B58: dcent 残高 + ローカル需要アップロード実績を返す。remote 呼び出し失敗時もローカル集計だけ返す。
func (s *ocSDK) handleGetStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	balanceDcents := s.getWalletBalance(ctx, s.principalID, s.cred)

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
	zeroSeller := true
	if v, ok := args["zero_seller"].(bool); ok {
		zeroSeller = v
	}
	// B96 (A1): schema-of-a-good-friction optional fields. All optional — omitting them still
	// saves fine (F1 exhaust protection; capture-not-fabricate).
	intendedTask, _ := args["intended_task"].(string)
	failedApproach, _ := args["failed_approach"].(string)
	missingThing, _ := args["missing_thing"].(string)
	workaround, _ := args["workaround"].(string)
	wasteAmount, _ := args["waste_amount"].(string)

	id := uuid.New().String()
	rec := map[string]any{
		"id":                 id,
		"descriptor":         descriptor,
		"raw_friction":       rawFriction,
		"context":            ctxStr,
		"zero_seller":        zeroSeller,
		"uploaded":           false,
		"uploaded_signal_id": "",
		"intended_task":      intendedTask,
		"failed_approach":    failedApproach,
		"missing_thing":      missingThing,
		"workaround":         workaround,
		"waste_amount":       wasteAmount,
		"created_at":         time.Now().UTC().Format(time.RFC3339),
	}
	// saveP2PFile は 0700 dir 自動作成 + 0600 ファイル書き込み (p2p_store.go; Quotation と同型)。
	if err := saveP2PFile(s.demandFilePath(id), rec); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"save_failed","message":%q}`, err.Error())), nil
	}

	// privacy: 検索ミスの本文 (descriptor/raw_friction) を stdout/stderr に出さない (B42)。
	// upload_demand を confirm なしで呼ぶとユーザーに見せるべき投稿内容プレビューが返る。
	out := map[string]any{
		"demand_record_id": id,
		"saved":            true,
		"uploaded":         false,
		"next_action":      "batch_review_at_next_pause",
		"assistant_instruction": "Do not propose upload right now — that would interrupt the current work (noise budget; PB-4). " +
			"At the next natural pause (session end, or a big milestone), call list_local_demands once to review " +
			"all pending records together, then call upload_demand (without confirm) per record the user agrees to share " +
			"to preview it before asking for final confirmation.",
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

// handleUploadDemand — DM-A2: 確認フロー付き upload。
// confirm=false (省略可) → 投稿内容のプレビューを返し AI にユーザー確認を求めさせる。
// confirm=true → 実際に POST /demand/signals へ送信し uploaded=true に書き戻す。
// 共有するのは descriptor + zero_seller + opt-in 済み raw_friction/context。
// OC_NO_UPLOAD_DEMAND=1 が設定されている場合はアップロードをブロックする (opt-out)。
func (s *ocSDK) handleUploadDemand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if os.Getenv("OC_NO_UPLOAD_DEMAND") == "1" {
		return mcp.NewToolResultError(`{"error":"upload_disabled","message":"upload disabled (OC_NO_UPLOAD_DEMAND=1)"}`), nil
	}

	args := req.GetArguments()
	id, _ := args["demand_record_id"].(string)
	if strings.TrimSpace(id) == "" {
		return mcp.NewToolResultError(`{"error":"missing_required_field","message":"demand_record_id is required"}`), nil
	}
	confirm, _ := args["confirm"].(bool)

	// B86: アップロード者が unlock 床値を設定する (per-signal min_unlock_dcents 復活)。
	// 省略 (<=0) のときは platform デフォルト (100đ) を適用する。
	minUnlockDcents := defaultMinUnlockDcents
	if v, ok := args["min_unlock_dcents"].(float64); ok && v > 0 {
		minUnlockDcents = int64(v)
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

	// 送信フィールドを組み立てる。
	// opt-in 済みのため raw_friction/context も含める (deliver-proxy; B70/B71)。
	// server が AES-256-GCM で暗号化して content_encrypted に保管 — 平文は API response に出ない。
	willSend := map[string]any{
		"descriptor":        rec["descriptor"],
		"zero_seller":       rec["zero_seller"],
		"min_unlock_dcents": minUnlockDcents, // B86: 買い手が unlock に払う床値 (đ)
	}
	// B96 (A1/A4): optional schema-of-a-good-friction fields — 存在すれば raw_friction/context と
	// 同じ暗号化 blob (opaque JSON string) で一緒に送る。server はこの文字列を解釈しない。
	structuredFields := map[string]any{
		"intended_task":   rec["intended_task"],
		"failed_approach": rec["failed_approach"],
		"missing_thing":   rec["missing_thing"],
		"workaround":      rec["workaround"],
		"waste_amount":    rec["waste_amount"],
	}
	hasStructured := false
	for _, v := range structuredFields {
		if s, _ := v.(string); s != "" {
			hasStructured = true
			break
		}
	}
	var structuredJSON string
	if hasStructured {
		if b, err := json.Marshal(structuredFields); err == nil {
			structuredJSON = string(b)
		}
	}
	willSendEncrypted := map[string]any{
		"raw_friction": rec["raw_friction"],
		"context":      rec["context"],
		"structured":   structuredFields,
	}

	// confirm=false → プレビューを返してユーザー確認を促す (安全デフォルト)。
	if !confirm {
		out := map[string]any{
			"action":                 "confirm_required",
			"will_send":              willSend,
			"will_encrypt_on_server": willSendEncrypted,
			"demand_record_id":       id,
			"assistant_instruction": "Show the 'will_send' content to the user EXACTLY as provided — " +
				"do not omit, summarize, or shorten the descriptor text. " +
				"Also show 'will_encrypt_on_server' (raw_friction and context) which the server will encrypt for the deliver-proxy. " +
				"Then ask: 'Upload this demand to OneCenter? raw_friction will be server-side encrypted and only returned to a paying buyer. This may earn đ if another agent unlocks it.' " +
				"Call upload_demand with confirm=true ONLY if the user explicitly agrees.",
		}
		outJSON, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(outJSON)), nil
	}

	// confirm=true → 実際に POST /demand/signals へ送信する。
	// raw_friction と context も送信 — server 側で暗号化して content_encrypted に保管される (deliver-proxy)。
	body, _ := json.Marshal(map[string]any{
		"semantic_descriptor": willSend["descriptor"],
		"zero_seller":         willSend["zero_seller"],
		"min_unlock_dcents":   minUnlockDcents, // B86: per-signal unlock 床値 (省略時 100đ)
		"raw_friction":        rec["raw_friction"],
		"context":             rec["context"],
		"structured":          structuredJSON, // B96 (A1/A4): opaque JSON string; empty if no structured fields
	})
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", s.oncenterURL+"/demand/signals", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)
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

// handleListDemandBoard — GET /demand/board → teaser 一覧 (無料; DM-A7; B71)。
// Hiro が đ を払う前に需要の存在を確認するためのツール。teaser のみ返す (raw_friction は含まない)。
func (s *ocSDK) handleListDemandBoard(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.oncenterURL+"/demand/board", nil)
	req.Header.Set("Authorization", "Bearer "+s.cred)
	resp, err := s.client.Do(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"api_unreachable","message":%q}`, err.Error())), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"board_unavailable","status":%d}`, resp.StatusCode)), nil
	}

	// B74: board は生 demand_signal 一覧を flat に返す (clustering 撤去)。
	// B86: unlock 価格は per-signal の min_unlock_dcents (各 signal が保持)。
	// top-level unlock_price_dcents は省略時デフォルトの参考値。
	var board struct {
		DemandSignals     []map[string]any `json:"demand_signals"`
		UnlockPriceDcents int64            `json:"unlock_price_dcents"`
	}
	json.Unmarshal(raw, &board)

	// teaser のみを返す (P-paywall-raw-is-the-moat: raw_friction は paywall 後)。
	teasers := make([]map[string]any, 0, len(board.DemandSignals))
	for _, sig := range board.DemandSignals {
		teasers = append(teasers, map[string]any{
			"signal_id":         sig["id"],
			"descriptor":        sig["semantic_descriptor"],
			"zero_seller":       sig["zero_seller"],
			"min_unlock_dcents": sig["min_unlock_dcents"], // B86: その signal を unlock する đ 床値
		})
	}
	out := map[string]any{
		"demand_signals":      teasers,
		"total":               len(teasers),
		"unlock_price_dcents": board.UnlockPriceDcents,
		"assistant_instruction": "Show each demand signal as a demand card: descriptor and min_unlock_dcents. " +
			"Each signal sets its own unlock price (min_unlock_dcents); the top-level unlock_price_dcents is just the default applied when an uploader omits a price. " +
			"To get raw friction details, call unlock_demand with the signal_id and bounty_dcents >= that signal's min_unlock_dcents.",
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// handleUnlockDemand — đ を払って need の raw_friction を 1 呼び出しで取得する (DM-A7; B71 / B74 signal キー)。
// 内部シーケンス:
//  1. POST /demand/signals/:signal_id/seed-requests {bounty_dcents} → (API deliver-proxy)
//  2. POST /demand/seed-requests/:id/accept → TransferDcent
//
// bounty_dcents が signal の min_unlock_dcents を下回る場合は create 時に 422。残高不足は 402 返す。
func (s *ocSDK) handleUnlockDemand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	signalID, _ := args["signal_id"].(string)
	var bountyDcents int64
	if v, ok := args["bounty_dcents"].(float64); ok {
		bountyDcents = int64(v)
	}
	if strings.TrimSpace(signalID) == "" || bountyDcents <= 0 {
		return mcp.NewToolResultError(`{"error":"invalid_args","message":"signal_id and bounty_dcents > 0 required"}`), nil
	}

	// Step 1: CreateSeedRequest — API auto-delivers via deliver-proxy (B70/B71).
	createBody, _ := json.Marshal(map[string]any{"bounty_dcents": bountyDcents})
	createReq, _ := http.NewRequestWithContext(ctx, "POST",
		s.oncenterURL+"/demand/signals/"+signalID+"/seed-requests",
		bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+s.cred)
	createResp, err := s.client.Do(createReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"api_unreachable","message":%q}`, err.Error())), nil
	}
	defer createResp.Body.Close()
	createRaw, _ := io.ReadAll(createResp.Body)
	if createResp.StatusCode == http.StatusUnprocessableEntity {
		// 422 includes bounty_below_minimum or no seller error.
		return mcp.NewToolResultError(string(createRaw)), nil
	}
	if createResp.StatusCode != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"create_seed_request_failed","status":%d,"detail":%s}`,
			createResp.StatusCode, createRaw)), nil
	}

	var srResult struct {
		SeedRequestID string `json:"seed_request_id"`
		Status        string `json:"status"`
		SeedRequest   struct {
			Seed *struct {
				IO         string `json:"io"`
				Context    string `json:"context"`
				Structured string `json:"structured"` // B96 (A1/A4): opaque JSON string, if the emitter provided it
			} `json:"seed"`
		} `json:"seed_request"`
	}
	json.Unmarshal(createRaw, &srResult)

	if srResult.Status != "delivered" {
		return mcp.NewToolResultError(fmt.Sprintf(
			`{"error":"deliver_proxy_unavailable","message":"demand signal has no encrypted content; auto-deliver failed (status=%s)"}`,
			srResult.Status)), nil
	}

	// Step 2: AcceptSeedRequest → TransferDcent (bounty_dcents goes to the emitter).
	acceptReq, _ := http.NewRequestWithContext(ctx, "POST",
		s.oncenterURL+"/demand/seed-requests/"+srResult.SeedRequestID+"/accept",
		bytes.NewReader([]byte("{}")))
	acceptReq.Header.Set("Content-Type", "application/json")
	acceptReq.Header.Set("Authorization", "Bearer "+s.cred)
	acceptResp, err := s.client.Do(acceptReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"api_unreachable","message":%q}`, err.Error())), nil
	}
	defer acceptResp.Body.Close()
	acceptRaw, _ := io.ReadAll(acceptResp.Body)
	if acceptResp.StatusCode == http.StatusPaymentRequired {
		return mcp.NewToolResultError(`{"error":"insufficient_balance","message":"not enough đ to pay the bounty"}`), nil
	}
	if acceptResp.StatusCode == http.StatusConflict {
		return mcp.NewToolResultError(string(acceptRaw)), nil
	}
	if acceptResp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf(`{"error":"accept_failed","status":%d,"detail":%s}`,
			acceptResp.StatusCode, acceptRaw)), nil
	}

	// raw_friction は SeedContent.IO に格納されている (deliver-proxy が IO フィールドに詰めた)。
	rawFriction := ""
	ctxStr := ""
	// B96 (A1/A4): structured は opaque JSON string。unlock_demand は解釈せずそのまま構造体に unmarshal して返す
	// (server は名付け・要約しない; B-mechanical-only は client 側でも維持)。
	var structured map[string]any
	if s := srResult.SeedRequest.Seed; s != nil {
		rawFriction = s.IO
		ctxStr = s.Context
		if s.Structured != "" {
			_ = json.Unmarshal([]byte(s.Structured), &structured)
		}
	}

	out := map[string]any{
		"seed_request_id":  srResult.SeedRequestID,
		"raw_friction":     rawFriction,
		"context":          ctxStr,
		"structured":       structured, // B96: intended_task/failed_approach/missing_thing/workaround/waste_amount if provided
		"unlock_confirmed": true,
		"assistant_instruction": "Present raw_friction to the user as the unlocked demand details. " +
			"This is the raw experience the original agent recorded when they hit this wall. " +
			"If 'structured' is present, it holds the emitter's schema-of-a-good-friction breakdown " +
			"(intended_task/failed_approach/missing_thing/workaround/waste_amount) — use it to spec a capability that solves this need.",
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}
