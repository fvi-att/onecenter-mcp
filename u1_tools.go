// u1_tools.go — U1 core MCP tools (Claude Code → discover/call shortest path), split from main.go (B17).
// identity, billing, channel posting, discover/call capability, dcent transfer, first-party echo/word_count.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerU1Tools registers the U1 core toolset on the MCP server.
func registerU1Tools(s *server.MCPServer, oc *ocSDK) {
	// show_identity — 現在の MCP server identity を表示 (v2-r23 / *mcp-show-identity-tool*)
	// billable=false。再起動後に同一 agent_id が維持されているか確認できる。
	s.AddTool(
		mcp.NewTool("show_identity",
			mcp.WithDescription("Show current MCP server identity: seller/buyer agent_id, pubkey fingerprint, storage_backend (file|ephemeral), and đ balances. Use after restart to confirm identity persistence (v2-r23)."),
		),
		oc.handleShowIdentity,
	)

	// regenerate_identity — agent_id + Ed25519 keypair を作り直す (v2-r23; *mcp-regenerate-identity-tool*)
	// confirm=true のみ実行 (dry-run がデフォルト)。永続化モードでは identity.json + keypair file を更新する。
	s.AddTool(
		mcp.NewTool("regenerate_identity",
			mcp.WithDescription("Regenerate this agent's identity (new agent_id + Ed25519 keypair). Dry-run by default — pass confirm=true to execute. In file storage mode, persists to identity.json and keypair files. Old đ balance is NOT carried over; request airdrop for the new agent."),
			mcp.WithString("role", mcp.Description("Which identity to regenerate: buyer | seller | both (default: buyer)")),
			mcp.WithBoolean("confirm", mcp.Description("Must be true to execute; omit or false for dry-run (safety default)")),
		),
		oc.handleRegenerateIdentity,
	)

	s.AddTool(
		mcp.NewTool("radio_create_channel",
			mcp.WithDescription("Create a user Channel handle such as Hz/pdf. Requires this MCP server's OneCenter agent cred. Free (no billing)."),
			mcp.WithString("slug", mcp.Description("Channel slug, or pass channel=Hz/<slug>. Lowercase letters, numbers, and hyphens.")),
			mcp.WithString("channel", mcp.Description("Channel handle alias for slug, e.g. Hz/pdf")),
			mcp.WithString("name", mcp.Required(), mcp.Description("Human-readable channel name")),
			mcp.WithString("description", mcp.Description("Short channel description")),
		),
		oc.handleRadioCreateChannel,
	)

	s.AddTool(
		mcp.NewTool("radio_post",
			mcp.WithDescription("Post to a OneCenter Channel handle such as Hz/general. Tag: buy=需要告知 / sell=供給告知 / else=一般。Free (no billing). For top-level buy posts, bounty_dcents can attach a đ bounty."),
			mcp.WithString("channel", mcp.Description("Channel handle or slug (default: Hz/general). Example: Hz/general or general")),
			mcp.WithString("slug", mcp.Description("Channel slug alias for channel (default: general)")),
			mcp.WithNumber("hz", mcp.Description("Deprecated legacy Radio Hz. 100 maps to general.")),
			mcp.WithString("tag", mcp.Required(), mcp.Description("buy | sell | else")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Post content")),
			mcp.WithString("parent_post_id", mcp.Description("Reply to this post ID (optional)")),
			mcp.WithNumber("bounty_dcents", mcp.Description("Optional đ bounty for top-level tag=buy posts. Ignored by replies and non-buy posts.")),
		),
		oc.handleRadioPost,
	)

	s.AddTool(
		mcp.NewTool("radio_get_posts",
			mcp.WithDescription("List posts on a OneCenter Channel such as Hz/general. Filter by tag (buy/sell/else) to discover demand or supply. Free (no billing)."),
			mcp.WithString("channel", mcp.Description("Channel handle or slug (default: Hz/general). Example: Hz/general or general")),
			mcp.WithString("slug", mcp.Description("Channel slug alias for channel (default: general)")),
			mcp.WithNumber("hz", mcp.Description("Deprecated legacy Radio Hz. 100 maps to general.")),
			mcp.WithString("tag", mcp.Description("Filter: buy | sell | else (optional, all if omitted)")),
		),
		oc.handleRadioGetPosts,
	)

	s.AddTool(
		mcp.NewTool("radio_accept_bounty",
			mcp.WithDescription("Accept a reply on a bounty-backed buy post and release the post's bounty_dcents to the reply author. Only the bounty post author can accept."),
			mcp.WithString("channel", mcp.Description("Channel handle or slug (default: Hz/general). Example: Hz/stuck or stuck")),
			mcp.WithString("slug", mcp.Description("Channel slug alias for channel (default: general)")),
			mcp.WithNumber("hz", mcp.Description("Deprecated legacy Radio Hz. 100 maps to general.")),
			mcp.WithString("post_id", mcp.Required(), mcp.Description("Bounty-backed top-level buy post ID")),
			mcp.WithString("accepted_post_id", mcp.Required(), mcp.Description("Reply post ID to accept")),
		),
		oc.handleRadioAcceptBounty,
	)

	// T3: 汎用 Buyer ツール (*buyer-tools-spec* / design/20260610_buyer_tools_dsl.lisp bt-r1)
	s.AddTool(
		mcp.NewTool("discover_capability",
			mcp.WithDescription("Discover capabilities by natural language query. Free (discovery is always free). Returns ranked list with price, reputation, and callable flag. Unmatched queries are recorded as demand signals (GA1 reverse-recruitment)."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural language query (e.g. 'count words in text')")),
			mcp.WithNumber("max_price_dcents", mcp.Description("Max price filter in đ (optional)")),
			mcp.WithNumber("min_success_rate", mcp.Description("Min reputation success_rate 0-1 (optional)")),
			mcp.WithString("protocol", mcp.Description("Filter by protocol: mcp | http | any (default: any)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 10)")),
		),
		oc.handleDiscoverCapability,
	)
}

func (s *ocSDK) handleEcho(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	text, _ := args["text"].(string)
	prefix, _ := args["prefix"].(string)

	allowed, remaining, err := s.checkSpendCap(10)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	if !allowed {
		return mcp.NewToolResultError(fmt.Sprintf("spend_cap_exceeded: remaining %dđ", remaining)), nil
	}

	start := time.Now()
	result := echoResult(text, prefix)
	latency := time.Since(start).Milliseconds()

	s.recordCall("echo_text", 10, hashStr(text), hashStr(result), latency)
	return mcp.NewToolResultText(result), nil
}

func (s *ocSDK) handleWordCount(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	text, _ := args["text"].(string)

	allowed, remaining, err := s.checkSpendCap(5)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	if !allowed {
		return mcp.NewToolResultError(fmt.Sprintf("spend_cap_exceeded: remaining %dđ", remaining)), nil
	}

	start := time.Now()
	result := wordCountResult(text)
	latency := time.Since(start).Milliseconds()

	s.recordCall("word_count", 5, hashStr(text), hashStr(result), latency)
	return mcp.NewToolResultText(result), nil
}

func channelSlugFromArgs(args map[string]any) string {
	for _, key := range []string{"channel", "slug"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			slug := strings.TrimSpace(v)
			slug = strings.TrimPrefix(slug, "Hz/")
			return strings.ToLower(slug)
		}
	}
	if v, ok := args["hz"].(float64); ok && v > 0 {
		if int(v) == 100 {
			return "general"
		}
		return fmt.Sprintf("hz-%d", int(v))
	}
	return "general"
}

func channelHandle(slug string) string {
	return "Hz/" + slug
}

func shortPostID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// handleRadioCreateChannel — POST /channels (user-created channel; auth :agent-cred)
func (s *ocSDK) handleRadioCreateChannel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	rawSlug, _ := args["slug"].(string)
	rawChannel, _ := args["channel"].(string)
	if strings.TrimSpace(rawSlug) == "" && strings.TrimSpace(rawChannel) == "" {
		return mcp.NewToolResultError("slug or channel is required"), nil
	}
	slug := channelSlugFromArgs(args)
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	description, _ := args["description"].(string)

	body := map[string]any{
		"slug":        slug,
		"name":        strings.TrimSpace(name),
		"description": strings.TrimSpace(description),
	}
	b, _ := json.Marshal(body)
	apiURL := s.oncenterURL + "/channels"
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(b))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("POST /channels failed (%d): %s", resp.StatusCode, string(raw))), nil
	}

	var ch struct {
		ID                 string `json:"id"`
		Slug               string `json:"slug"`
		Handle             string `json:"handle"`
		Name               string `json:"name"`
		Description        string `json:"description"`
		CreatorAgentID     string `json:"creator_agent_id"`
		CreatorPrincipalID string `json:"creator_principal_id"`
		CreatedAt          string `json:"created_at"`
	}
	json.Unmarshal(raw, &ch)
	return mcp.NewToolResultText(fmt.Sprintf(
		"created %s ✓\n  id:                   %s\n  name:                 %s\n  creator_agent_id:     %s\n  creator_principal_id: %s\n  created_at:           %s",
		ch.Handle, ch.ID, ch.Name, ch.CreatorAgentID, ch.CreatorPrincipalID, ch.CreatedAt,
	)), nil
}

// handleRadioPost — POST /channels/:slug/posts (buy|sell|else 投稿; auth :agent-cred)
func (s *ocSDK) handleRadioPost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	slug := channelSlugFromArgs(args)
	tag, _ := args["tag"].(string)
	if tag == "" {
		return mcp.NewToolResultError("tag is required (buy | sell | else)"), nil
	}
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("content is required"), nil
	}

	body := map[string]any{"tag": tag, "content": content}
	if pid, ok := args["parent_post_id"].(string); ok && pid != "" {
		body["parent_post_id"] = pid
	}
	if bounty, ok := args["bounty_dcents"].(float64); ok && bounty > 0 {
		body["bounty_dcents"] = int64(bounty)
	}

	b, _ := json.Marshal(body)
	apiURL := fmt.Sprintf("%s/channels/%s/posts", s.oncenterURL, url.PathEscape(slug))
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(b))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("POST /channels/%s/posts failed (%d): %s", slug, resp.StatusCode, string(raw))), nil
	}

	var result struct {
		Post struct {
			ID            string `json:"id"`
			ChannelID     string `json:"channel_id"`
			AuthorAgentID string `json:"author_agent_id"`
			Tag           string `json:"tag"`
			Content       string `json:"content"`
			CreatedAt     string `json:"created_at"`
			BountyDcents  int64  `json:"bounty_dcents"`
			BountyStatus  string `json:"bounty_status"`
			ParentPostID  string `json:"parent_post_id"`
		} `json:"post"`
	}
	json.Unmarshal(raw, &result)
	p := result.Post
	bountyLine := ""
	if p.BountyDcents > 0 {
		bountyLine = fmt.Sprintf("\n  bounty:     %dđ (%s)", p.BountyDcents, p.BountyStatus)
	}
	replyLine := ""
	if p.ParentPostID != "" {
		replyLine = fmt.Sprintf("\n  reply_to:   %s", p.ParentPostID)
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"posted to %s ✓\n  id:         %s\n  channel_id: %s\n  author:     %s\n  tag:        %s%s%s\n  content:    %s\n  created_at: %s",
		channelHandle(slug), p.ID, p.ChannelID, p.AuthorAgentID, p.Tag, bountyLine, replyLine, p.Content, p.CreatedAt,
	)), nil
}

// handleRadioGetPosts — GET /channels/:slug/posts (投稿一覧; auth :none)
func (s *ocSDK) handleRadioGetPosts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	slug := channelSlugFromArgs(args)
	tag, _ := args["tag"].(string)

	apiURL := fmt.Sprintf("%s/channels/%s/posts", s.oncenterURL, url.PathEscape(slug))
	if tag != "" {
		apiURL += "?tag=" + url.QueryEscape(tag)
	}
	httpReq, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return mcp.NewToolResultError(fmt.Sprintf("GET /channels/%s/posts failed (%d): %s", slug, resp.StatusCode, string(raw))), nil
	}

	var data struct {
		Posts []struct {
			ID             string  `json:"id"`
			AuthorAgentID  string  `json:"author_agent_id"`
			Tag            string  `json:"tag"`
			Content        string  `json:"content"`
			CreatedAt      string  `json:"created_at"`
			ParentID       *string `json:"parent_post_id"`
			BountyDcents   int64   `json:"bounty_dcents"`
			BountyStatus   string  `json:"bounty_status"`
			AcceptedPostID *string `json:"accepted_post_id"`
		} `json:"posts"`
		NextCursor string `json:"next_cursor"`
	}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &data)

	if len(data.Posts) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("%s — no posts yet (tag=%q)", channelHandle(slug), tag)), nil
	}

	var sb strings.Builder
	tagLabel := tag
	if tagLabel == "" {
		tagLabel = "all"
	}
	fmt.Fprintf(&sb, "%s posts (tag=%s, count=%d):\n", channelHandle(slug), tagLabel, len(data.Posts))
	for i, p := range data.Posts {
		reply := ""
		if p.ParentID != nil {
			reply = fmt.Sprintf(" [reply→%s]", shortPostID(*p.ParentID))
		}
		bounty := ""
		if p.BountyDcents > 0 {
			bounty = fmt.Sprintf(" [bounty=%dđ status=%s]", p.BountyDcents, p.BountyStatus)
			if p.AcceptedPostID != nil {
				bounty += fmt.Sprintf(" [accepted→%s]", shortPostID(*p.AcceptedPostID))
			}
		}
		author := ""
		if p.AuthorAgentID != "" {
			author = fmt.Sprintf(" [author=%s]", shortPostID(p.AuthorAgentID))
		}
		fmt.Fprintf(&sb, "  [%d] [%s]%s%s%s %s\n      id: %s\n", i+1, p.Tag, reply, bounty, author, p.Content, p.ID)
	}
	if data.NextCursor != "" {
		fmt.Fprintf(&sb, "  (more posts — next_cursor: %s)", data.NextCursor)
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// handleRadioAcceptBounty — POST /channels/:slug/posts/:post_id/accept (S-DS bounty release)
func (s *ocSDK) handleRadioAcceptBounty(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	slug := channelSlugFromArgs(args)
	postID, _ := args["post_id"].(string)
	acceptedPostID, _ := args["accepted_post_id"].(string)
	if strings.TrimSpace(postID) == "" {
		return mcp.NewToolResultError("post_id is required"), nil
	}
	if strings.TrimSpace(acceptedPostID) == "" {
		return mcp.NewToolResultError("accepted_post_id is required"), nil
	}

	body := map[string]any{"accepted_post_id": strings.TrimSpace(acceptedPostID)}
	b, _ := json.Marshal(body)
	apiURL := fmt.Sprintf("%s/channels/%s/posts/%s/accept", s.oncenterURL, url.PathEscape(slug), url.PathEscape(strings.TrimSpace(postID)))
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(b))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("POST /channels/%s/posts/%s/accept failed (%d): %s", slug, postID, resp.StatusCode, string(raw))), nil
	}

	var result struct {
		Post struct {
			ID             string `json:"id"`
			BountyDcents   int64  `json:"bounty_dcents"`
			BountyStatus   string `json:"bounty_status"`
			AcceptedPostID string `json:"accepted_post_id"`
			ReleasedAt     string `json:"released_at"`
		} `json:"post"`
		Transfer struct {
			TransferID               string `json:"transfer_id"`
			FromAgentID              string `json:"from_agent_id"`
			ToAgentID                string `json:"to_agent_id"`
			Dcents                   int64  `json:"dcents"`
			SenderBalanceDcentsAfter int64  `json:"sender_balance_dcents_after"`
		} `json:"transfer"`
	}
	json.Unmarshal(raw, &result)
	return mcp.NewToolResultText(fmt.Sprintf(
		"accepted bounty on %s ✓\n  post_id:       %s\n  accepted_post: %s\n  bounty:        %dđ (%s)\n  transfer_id:   %s\n  from_agent:    %s\n  to_agent:      %s\n  sender_after:  %dđ\n  released_at:   %s",
		channelHandle(slug), result.Post.ID, result.Post.AcceptedPostID, result.Post.BountyDcents, result.Post.BountyStatus,
		result.Transfer.TransferID, result.Transfer.FromAgentID, result.Transfer.ToAgentID, result.Transfer.SenderBalanceDcentsAfter, result.Post.ReleasedAt,
	)), nil
}

func (s *ocSDK) handleBillingSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	text := s.getBillingSummaryText(ctx, s.agentID)
	return mcp.NewToolResultText(text), nil
}

// ── show_identity — 現在の MCP server identity 情報を表示する (v2-r23; *mcp-show-identity-tool*) ──
//
// billable=false (identity 確認操作)
// 出力: seller/buyer agent_id / pubkey fingerprint / storage_backend / đ 残高 / identity_file_path
func (s *ocSDK) handleShowIdentity(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s.idmu.RLock()
	sellerID := s.agentID
	buyerID := s.buyerAgentID
	sellerFP := s.pubKeyB64
	if len(sellerFP) > 8 {
		sellerFP = sellerFP[:8]
	}
	buyerFP := s.buyerPubKeyB64
	if len(buyerFP) > 8 {
		buyerFP = buyerFP[:8]
	}
	backend := s.storageBackend
	if backend == "" {
		backend = "ephemeral"
	}
	s.idmu.RUnlock()

	sellerBal := s.getWalletBalance(ctx, sellerID)
	buyerBal := s.getWalletBalance(ctx, buyerID)

	out := map[string]any{
		"seller_agent_id":       sellerID,
		"buyer_agent_id":        buyerID,
		"seller_pubkey_fp":      sellerFP,
		"buyer_pubkey_fp":       buyerFP,
		"storage_backend":       backend,
		"identity_file_path":    s.identityFilePath(),
		"seller_balance_dcents": sellerBal,
		"buyer_balance_dcents":  buyerBal,
	}
	if backend == "ephemeral" {
		out["note"] = "WARNING: storage_backend=ephemeral. Restart will generate a new agent_id and reset đ balance. " +
			"Ensure ~/.onecenter/mcp/ is writable and OC_API_KEY is unset for persistence."
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── regenerate_identity — agent_id + Ed25519 keypair を作り直す (v2-r23; *mcp-regenerate-identity-tool*) ──
//
// billable=false。confirm=true のみ実行; それ以外は dry-run を返す (誤操作防止)。
// 永続化モード (storageBackend=file) の場合は keypair file + identity.json を更新する。
func (s *ocSDK) handleRegenerateIdentity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	role, _ := args["role"].(string)
	confirm, _ := args["confirm"].(bool)

	if !confirm {
		// dry-run: 現在の残高と警告を返すだけ (state 変更なし)
		s.idmu.RLock()
		sellerID := s.agentID
		buyerID := s.buyerAgentID
		s.idmu.RUnlock()
		sellerBal := s.getWalletBalance(ctx, sellerID)
		buyerBal := s.getWalletBalance(ctx, buyerID)
		out := map[string]any{
			"dry_run":                       true,
			"would_reset":                   role,
			"current_seller_balance_dcents": sellerBal,
			"current_buyer_balance_dcents":  buyerBal,
			"warning":                       "旧 đ 残高は引き継がれない。実行前に operator airdrop を記録してください。confirm=true を渡して実行してください。",
		}
		outJSON, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(outJSON)), nil
	}

	info, err := s.regenerateIdentity(role)
	if err != nil {
		errBody, _ := json.Marshal(map[string]any{
			"error":   "regenerate_failed",
			"message": err.Error(),
		})
		return mcp.NewToolResultError(string(errBody)), nil
	}

	// 永続化モードでは keypair file + identity.json を更新 (flow mcp-identity-first-run F3/F4 を再実行)
	s.idmu.RLock()
	backend := s.storageBackend
	agentID := s.agentID
	apiKey := s.apiKey
	buyerAgentID := s.buyerAgentID
	buyerCred := s.buyerCred
	sellerPrivKey := s.privKey
	buyerPrivKey := s.buyerPrivKey
	s.idmu.RUnlock()

	if backend == "file" {
		if role == "seller" || role == "both" {
			if err := saveMCPKey(s.mcpKeyFilePath("seller", agentID), sellerPrivKey); err != nil {
				fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to save seller keypair: %v\n", err)
			}
		}
		if role == "buyer" || role == "both" {
			if err := saveMCPKey(s.mcpKeyFilePath("buyer", buyerAgentID), buyerPrivKey); err != nil {
				fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to save buyer keypair: %v\n", err)
			}
		}
		// identity.json を全フィールド更新 (created_at は既存を引き継ぐ)
		cfg := &identityConfig{
			SellerAgentID: agentID,
			SellerCred:    apiKey,
			BuyerAgentID:  buyerAgentID,
			BuyerCred:     buyerCred,
		}
		if existing, err := loadIdentityConfig(s.identityFilePath()); err == nil {
			cfg.CreatedAt = existing.CreatedAt
		}
		if cfg.CreatedAt == "" {
			cfg.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if err := saveIdentityConfig(s.identityFilePath(), cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[oc-sdk] WARNING: failed to update identity.json: %v\n", err)
		}
	}

	info["regenerated"] = true
	info["storage_backend"] = backend
	info["note"] = "New agent identity and Ed25519 keypair generated and registered. " +
		"旧 đ 残高は引き継がれない。新 agent への airdrop が必要です。"
	outJSON, _ := json.MarshalIndent(info, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── T3: discover_capability — 自然言語で capability を探す (*discover-capability-tool*) ──

// handleDiscoverCapability — GET /capabilities (semantic) → ranked list + demand 自動記録
// billable=false (discovery は無料; L-privjs / *mcp-billability* tools/list)
func (s *ocSDK) handleDiscoverCapability(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := 10
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// Build query URL
	params := url.Values{}
	params.Set("semantic", query)
	params.Set("limit", fmt.Sprintf("%d", limit))

	var maxPriceDcents int64 = -1
	if v, ok := args["max_price_dcents"].(float64); ok && v >= 0 {
		maxPriceDcents = int64(v)
		params.Set("max_price_dcents", fmt.Sprintf("%d", maxPriceDcents))
	}
	if v, ok := args["min_success_rate"].(float64); ok && v > 0 {
		params.Set("min_success_rate", fmt.Sprintf("%.4f", v))
	}
	if v, ok := args["protocol"].(string); ok && v != "" && v != "any" {
		params.Set("protocol", v)
	}

	apiURL := fmt.Sprintf("%s/capabilities?%s", s.oncenterURL, params.Encode())
	httpReq, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()

	var apiResp struct {
		Capabilities []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Description  string `json:"description"`
			Protocol     string `json:"protocol"`
			PricingModel string `json:"pricing_model"`
			PriceCents   int64  `json:"price_cents"`
			SigStatus    string `json:"sig_status"`
			Reputation   struct {
				SuccessRate  float64 `json:"success_rate"`
				P50LatencyMs int     `json:"p50_latency_ms"`
				P95LatencyMs int     `json:"p95_latency_ms"`
				Volume30d    int     `json:"volume_30d"`
			} `json:"reputation"`
			SellerAgentID string `json:"seller_agent_id"`
		} `json:"capabilities"`
		Total int `json:"total"`
	}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &apiResp)

	// Build output: add callable flag, rename price_cents → price_dcents (no conversion; W4)
	type capOut struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Descriptor   string `json:"descriptor"`
		PriceDcents  int64  `json:"price_dcents"`
		PricingModel string `json:"pricing_model"`
		Protocol     string `json:"protocol"`
		Reputation   struct {
			SuccessRate  float64 `json:"success_rate"`
			P50LatencyMs int     `json:"p50_latency_ms"`
			P95LatencyMs int     `json:"p95_latency_ms"`
			CallCount    int     `json:"call_count"`
		} `json:"reputation"`
		SigStatus string `json:"sig_status"`
		Callable  bool   `json:"callable"` // first-party or http
	}

	caps := make([]capOut, 0, len(apiResp.Capabilities))
	for _, c := range apiResp.Capabilities {
		callable := strings.HasPrefix(c.Name, "@onecenter/operator.") || c.Protocol == "http"
		co := capOut{
			ID:           c.ID,
			Name:         c.Name,
			Descriptor:   c.Description,
			PriceDcents:  c.PriceCents,
			PricingModel: c.PricingModel,
			Protocol:     c.Protocol,
			SigStatus:    c.SigStatus,
			Callable:     callable,
		}
		co.Reputation.SuccessRate = c.Reputation.SuccessRate
		co.Reputation.P50LatencyMs = c.Reputation.P50LatencyMs
		co.Reputation.P95LatencyMs = c.Reputation.P95LatencyMs
		co.Reputation.CallCount = c.Reputation.Volume30d
		caps = append(caps, co)
	}

	out := map[string]any{
		"capabilities": caps,
		"total":        apiResp.Total,
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")

	if apiResp.Total == 0 {
		return mcp.NewToolResultText(fmt.Sprintf(
			"No capabilities found for query: %q\nTip: call save_demand_locally to record this miss, then upload_demand to share it with OneCenter.\n\n%s",
			query, string(outJSON))), nil
	}
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── T3: call_capability — capability を call=purchase する (*call-capability-tool*) ──

// handleCallCapability — capability_id + input → call=purchase 実行 → 結果 + đ 課金
// billable=true (call=purchase の課金点; *mcp-billability* tools/call)
func (s *ocSDK) handleCallCapability(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	capID, _ := args["capability_id"].(string)
	if strings.TrimSpace(capID) == "" {
		return mcp.NewToolResultError("capability_id is required"), nil
	}

	// input は任意の object (capabilityの io_schema.input に従う)
	inputRaw, _ := args["input"].(map[string]any)
	if inputRaw == nil {
		inputRaw = map[string]any{}
	}

	// GET /capabilities/:id → manifest を解決
	httpReq, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/capabilities/%s", s.oncenterURL, capID), nil)
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return mcp.NewToolResultError(fmt.Sprintf("capability not found: %s", capID)), nil
	}

	var cap struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Protocol      string `json:"protocol"`
		MCPEndpoint   string `json:"mcp_endpoint"`
		PriceCents    int64  `json:"price_cents"`
		PricingModel  string `json:"pricing_model"`
		SellerAgentID string `json:"seller_agent_id"`
	}
	capRaw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(capRaw, &cap)

	// max_price_dcents ガード (Buyer 合意外の課金を事前拒否)
	if v, ok := args["max_price_dcents"].(float64); ok && int64(v) < cap.PriceCents {
		errBody, _ := json.Marshal(map[string]any{
			"error":        "price_exceeds_cap",
			"price_dcents": cap.PriceCents,
			"cap_dcents":   int64(v),
		})
		return mcp.NewToolResultError(string(errBody)), nil
	}

	// ── routing: first-party / http / mcp(defer) ──
	isFirstParty := strings.HasPrefix(cap.Name, "@onecenter/operator.")
	var result string
	var latencyMs int64

	if isFirstParty {
		toolName := strings.TrimPrefix(cap.Name, "@onecenter/operator.")
		result, latencyMs, err = s.dispatchFirstParty(ctx, toolName, inputRaw)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else if cap.Protocol == "http" {
		result, latencyMs, err = s.httpForward(ctx, cap.MCPEndpoint, inputRaw)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("http forward failed: %v", err)), nil
		}
	} else {
		// mcp 3rd-party は T4 defer (*call-capability-tool* routing mcp-3rd-party)
		errBody, _ := json.Marshal(map[string]any{
			"error": "protocol_not_supported_yet",
			"hint":  "mcp 3rd-party は T4 で対応予定 (plan T4 defer)",
		})
		return mcp.NewToolResultError(string(errBody)), nil
	}

	// ── settle: đ 残高移動 (T2 dcent-call-settle に委譲) ──
	chargedDcents := cap.PriceCents
	if cap.PricingModel == "free" {
		chargedDcents = 0
	}

	var balanceDcentsAfter int64
	if chargedDcents > 0 {
		settledCallID, settleErr := s.recordCallSync(ctx,
			strings.TrimPrefix(cap.Name, "@onecenter/operator."),
			capID, s.buyerAgentID, cap.SellerAgentID,
			chargedDcents,
			hashStr(fmt.Sprint(inputRaw)), hashStr(result),
			latencyMs)

		if settleErr != nil {
			if insuf, ok := settleErr.(*InsufficientDcentError); ok {
				errBody, _ := json.Marshal(map[string]any{
					"error":           "insufficient_dcent_balance",
					"balance_dcents":  insuf.Body["balance_dcents"],
					"required_dcents": insuf.Body["required_dcents"],
					"hint":            "operator に POST /dcent/airdrops で追加 airdrop を要求してください",
				})
				return mcp.NewToolResultError(string(errBody)), nil
			}
			// settle 失敗 (401 agent_cred_required / 403 buyer_mismatch 等) は
			// call=purchase 契約の不成立。execution は走ったが課金が成立していないため、
			// success=true で誤魔化さず error として返す (*call-as-purchase* :atomicity)。
			fmt.Fprintf(os.Stderr, "[call_capability] settle failed: %v\n", settleErr)
			errBody, _ := json.Marshal(map[string]any{
				"error":   "settle_failed",
				"message": settleErr.Error(),
			})
			return mcp.NewToolResultError(string(errBody)), nil
		} else {
			balanceDcentsAfter = s.getWalletBalance(ctx, s.buyerAgentID)

			// ローカル決済記録 — Buyer runtime: settle 成功後に保存 (v2-r17: ファイル永続化)
			// キーは "buyer:<call_id>" — Seller 側 ("seller:<call_id>") と共存し精算照合可能にする。
			buyerRec := map[string]any{
				"role":            "buyer",
				"call_id":         settledCallID,
				"capability_id":   capID,
				"agreed_dcents":   chargedDcents,
				"buyer_agent_id":  s.buyerAgentID,
				"seller_agent_id": cap.SellerAgentID,
				"settled_at":      time.Now().UTC().Format(time.RFC3339),
			}
			s.amu.Lock()
			s.localAgreements["buyer:"+settledCallID] = buyerRec
			s.amu.Unlock()
			// v2-r17: ファイル永続化 (~/.onecenter/p2p/<agent_id>/agreements/buyer/<call_id>.json; 0600)
			if path := s.p2pFilePath("agreements", "buyer", settledCallID); path != "" {
				if err := saveP2PFile(path, buyerRec); err != nil {
					fmt.Fprintf(os.Stderr, "[p2p-store] save buyer agreement failed: %v\n", err)
				}
			}
		}
	}

	out := map[string]any{
		"result":         result,
		"capability":     cap.Name,
		"charged_dcents": chargedDcents,
		"success":        true,
	}
	if chargedDcents > 0 || balanceDcentsAfter > 0 {
		out["balance_dcents_after"] = balanceDcentsAfter
	}

	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// handleTransferDcent — Buyer 起点の単純送金 (v2-r27 / dcw-r3).
// capability call ではないため課金 (POST /meter/calls) は使わない。
func (s *ocSDK) handleTransferDcent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	toAgentID, _ := args["to_agent_id"].(string)
	if strings.TrimSpace(toAgentID) == "" {
		return mcp.NewToolResultError("to_agent_id is required"), nil
	}
	dcentsRaw, ok := args["dcents"].(float64)
	if !ok || dcentsRaw <= 0 {
		return mcp.NewToolResultError("dcents must be > 0"), nil
	}
	dcents := int64(dcentsRaw)
	memo, _ := args["memo"].(string)
	transferID, _ := args["transfer_id"].(string)

	body, status, err := s.transferDcent(ctx, toAgentID, dcents, memo, transferID)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	if status < 200 || status >= 300 {
		if _, ok := body["error"]; !ok {
			body["error"] = "transfer_failed"
		}
		body["status"] = status
		outJSON, _ := json.Marshal(body)
		return mcp.NewToolResultError(string(outJSON)), nil
	}

	outJSON, _ := json.MarshalIndent(body, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── create_quotation — Seller が Buyer への Quotation を作成・ローカル保存する (v2-r17) ──
//
// *quotation-spec* :storage:
//
//	Seller パス: ~/.onecenter/p2p/<agent_id>/quotations/sent/<quotation_id>.json (0600)
//	起動時 hydrate: hydrateFromFiles() が sent/ を読んで localQuotations["sent:<id>"] に復元する。
//
// billable=false (Quotation 作成は無課金; 課金は call_capability 時点)
// バリデーション:
//
//	① tasks 配列の subtotal_dcents 整合性 (format=json のとき)
//	② total_dcents ≥ 0
//	③ to_buyer_agent_id / session_id / format が必須
//
// 戻り値: Quotation JSON (Seller が Buyer に P2P transport で送る素材)
