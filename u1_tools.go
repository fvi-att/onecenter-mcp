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
	"strings"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerU1Tools registers the U1 core toolset on the MCP server.
func registerU1Tools(s *server.MCPServer, oc *ocSDK) {
	// show_identity — current Principal identity.
	s.AddTool(
		mcp.NewTool("show_identity",
			mcp.WithDescription("Show the current Principal ID, public-key fingerprint, storage backend, identity file, and đ balance."),
		),
		oc.handleShowIdentity,
	)

	// regenerate_identity creates a new Principal and keypair; confirm=true is required.
	s.AddTool(
		mcp.NewTool("regenerate_identity",
			mcp.WithDescription("Create a new Principal and Ed25519 keypair. Dry-run by default; pass confirm=true to execute. The old đ balance is not carried over."),
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

	result := echoResult(text, prefix)
	return mcp.NewToolResultText(result), nil
}

func (s *ocSDK) handleWordCount(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	text, _ := args["text"].(string)

	result := wordCountResult(text)
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
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)

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
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)

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
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)

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
	text := s.getBillingSummaryText(ctx)
	return mcp.NewToolResultText(text), nil
}

// ── show_identity ──
func (s *ocSDK) handleShowIdentity(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s.idmu.RLock()
	principalID := s.principalID
	cred := s.cred
	pubkeyFP := s.pubKeyB64
	if len(pubkeyFP) > 8 {
		pubkeyFP = pubkeyFP[:8]
	}
	backend := s.storageBackend
	if backend == "" {
		backend = "ephemeral"
	}
	s.idmu.RUnlock()

	balance := s.getWalletBalance(ctx, principalID, cred)

	out := map[string]any{
		"principal_id":       principalID,
		"pubkey_fp":          pubkeyFP,
		"balance_dcents":     balance,
		"storage_backend":    backend,
		"identity_file_path": s.identityFilePath(),
	}
	outJSON, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(outJSON)), nil
}

// ── regenerate_identity ──
func (s *ocSDK) handleRegenerateIdentity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	confirm, _ := req.GetArguments()["confirm"].(bool)
	if !confirm {
		balance := s.getWalletBalance(ctx, s.principalID, s.cred)
		out, _ := json.MarshalIndent(map[string]any{
			"dry_run":                true,
			"current_balance_dcents": balance,
			"warning":                "A new Principal will not inherit the current đ balance. Pass confirm=true to continue.",
		}, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
	info, err := s.regenerateIdentity("")
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": "regenerate_failed", "message": err.Error()})
		return mcp.NewToolResultError(string(body)), nil
	}
	info["regenerated"] = true
	info["storage_backend"] = s.storageBackend
	out, _ := json.MarshalIndent(info, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
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
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)

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
			PriceDcents  int64  `json:"price_dcents"`
			SigStatus    string `json:"sig_status"`
			Reputation   struct {
				SuccessRate  float64 `json:"success_rate"`
				P50LatencyMs int     `json:"p50_latency_ms"`
				P95LatencyMs int     `json:"p95_latency_ms"`
				Volume30d    int     `json:"volume_30d"`
			} `json:"reputation"`
		} `json:"capabilities"`
		Total int `json:"total"`
	}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &apiResp)

	// Build output: retain the API's d-cent-only price field (W4).
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
			PriceDcents:  c.PriceDcents,
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
		// L-no-silent-stop / R-no-silent-stop (B83): 空結果を黙って返さない。
		// 需要記録フローを user/AI 双方が拾えるよう構造化 next_actions として明示提示する。
		zeroOut := map[string]any{
			"capabilities": caps,
			"total":        0,
			"query":        query,
			"next_actions": []map[string]any{
				{
					"tool":   "save_demand_locally",
					"reason": "この検索ミスを需要としてローカルに記録する (remote 非送信)",
					"args":   map[string]any{"query": query},
				},
				{
					"tool":   "upload_demand",
					"reason": "需要を OneCenter に共有しマッチング対象にする (明示 opt-in)",
					"args":   map[string]any{"query": query},
				},
			},
		}
		zeroJSON, _ := json.MarshalIndent(zeroOut, "", "  ")
		return mcp.NewToolResultText(fmt.Sprintf(
			"No capabilities found for query: %q\nThis is not a dead end — record it as demand:\n  • save_demand_locally — keep it locally (no remote send)\n  • upload_demand — share it with OneCenter (opt-in)\n\n%s",
			query, string(zeroJSON))), nil
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
	httpReq.Header.Set("Authorization", "Bearer "+s.cred)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultText("OneCenter API unreachable: " + err.Error()), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return mcp.NewToolResultError(fmt.Sprintf("capability not found: %s", capID)), nil
	}

	var cap struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Protocol          string `json:"protocol"`
		MCPEndpoint       string `json:"mcp_endpoint"`
		PriceDcents       int64  `json:"price_dcents"`
		PricingModel      string `json:"pricing_model"`
		SellerPrincipalID string `json:"seller_principal_id"`
	}
	capRaw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(capRaw, &cap)

	// max_price_dcents ガード (Buyer 合意外の課金を事前拒否)
	if v, ok := args["max_price_dcents"].(float64); ok && int64(v) < cap.PriceDcents {
		errBody, _ := json.Marshal(map[string]any{
			"error":        "price_exceeds_cap",
			"price_dcents": cap.PriceDcents,
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
	_ = latencyMs

	// Paid execution settles through the Principal-to-Principal transfer rail.
	chargedDcents := cap.PriceDcents
	if cap.PricingModel == "free" {
		chargedDcents = 0
	}

	var balanceDcentsAfter int64
	if chargedDcents > 0 {
		transfer, status, settleErr := s.transferDcent(ctx, cap.SellerPrincipalID, chargedDcents,
			"capability purchase: "+capID, uuid.NewString())
		if settleErr != nil || status < 200 || status >= 300 {
			errBody, _ := json.Marshal(map[string]any{
				"error":    "settle_failed",
				"message":  fmt.Sprint(settleErr),
				"response": transfer,
			})
			return mcp.NewToolResultError(string(errBody)), nil
		}
		balanceDcentsAfter = s.getWalletBalance(ctx, s.principalID, s.cred)
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
// capability call ではないため単純送金 rail を使う。
func (s *ocSDK) handleTransferDcent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	toPrincipalID, _ := args["to_principal_id"].(string)
	if strings.TrimSpace(toPrincipalID) == "" {
		return mcp.NewToolResultError("to_principal_id is required"), nil
	}
	dcentsRaw, ok := args["dcents"].(float64)
	if !ok || dcentsRaw <= 0 {
		return mcp.NewToolResultError("dcents must be > 0"), nil
	}
	dcents := int64(dcentsRaw)
	memo, _ := args["memo"].(string)
	transferID, _ := args["transfer_id"].(string)

	body, status, err := s.transferDcent(ctx, toPrincipalID, dcents, memo, transferID)
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
