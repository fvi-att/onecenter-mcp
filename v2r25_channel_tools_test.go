package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRadioTools_R25ChannelSlugRouting(t *testing.T) {
	var createPath string
	var createAuth string
	var postPath string
	var postAuth string
	var postBounty float64
	var getPath string
	var getQuery string
	var acceptPath string
	var acceptAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/channels":
			createPath = r.URL.Path
			createAuth = r.Header.Get("Authorization")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if body["slug"] != "pdf" || body["name"] != "PDF Automation" || body["description"] != "PDF demand and supply" {
				t.Fatalf("unexpected create body: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"id":                   "channel-pdf",
				"slug":                 "pdf",
				"handle":               "Hz/pdf",
				"name":                 "PDF Automation",
				"description":          "PDF demand and supply",
				"creator_agent_id":     "agt-buyer",
				"creator_principal_id": "pr-buyer",
				"created_at":           "2026-06-18T00:00:00Z",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/channels/general/posts":
			postPath = r.URL.Path
			postAuth = r.Header.Get("Authorization")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode post body: %v", err)
			}
			if body["tag"] != "buy" || body["content"] != "need an API" {
				t.Fatalf("unexpected post body: %v", body)
			}
			if body["bounty_dcents"] != float64(25) {
				t.Fatalf("expected bounty_dcents=25, got %v", body["bounty_dcents"])
			}
			postBounty = body["bounty_dcents"].(float64)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"post": map[string]any{
					"id":            "post-1",
					"channel_id":    "channel-general",
					"tag":           "buy",
					"content":       "need an API",
					"bounty_dcents": 25,
					"bounty_status": "open",
					"created_at":    "2026-06-15T00:00:00Z",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/channels/finance/posts":
			getPath = r.URL.Path
			getQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(map[string]any{
				"posts": []map[string]any{
					{"id": "post-2", "tag": "sell", "content": "finance API", "created_at": "2026-06-15T00:00:01Z"},
					{"id": "reply-1", "tag": "else", "content": "try this", "parent_post_id": "post-1", "created_at": "2026-06-15T00:00:02Z"},
					{"id": "post-1", "tag": "buy", "content": "need an API", "bounty_dcents": 25, "bounty_status": "open", "created_at": "2026-06-15T00:00:00Z"},
				},
				"next_cursor": "",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/channels/stuck/posts/post-1/accept":
			acceptPath = r.URL.Path
			acceptAuth = r.Header.Get("Authorization")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode accept body: %v", err)
			}
			if body["accepted_post_id"] != "reply-1" {
				t.Fatalf("unexpected accept body: %v", body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"post": map[string]any{
					"id":               "post-1",
					"bounty_dcents":    25,
					"bounty_status":    "released",
					"accepted_post_id": "reply-1",
					"released_at":      "2026-06-15T00:00:03Z",
				},
				"transfer": map[string]any{
					"transfer_id":                 "bounty-post-1",
					"from_agent_id":               "agt-buyer",
					"to_agent_id":                 "agt-helper",
					"dcents":                      25,
					"sender_balance_dcents_after": 75,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": "not found", "path": r.URL.Path})
		}
	}))
	defer ts.Close()

	sdk := &ocSDK{
		apiKey:      "oc_agt_test",
		oncenterURL: ts.URL,
		client:      ts.Client(),
	}

	createResult, err := sdk.handleRadioCreateChannel(context.Background(), makeCallReq(map[string]any{
		"channel":     "Hz/pdf",
		"name":        "PDF Automation",
		"description": "PDF demand and supply",
	}))
	if err != nil {
		t.Fatalf("radio_create_channel returned error: %v", err)
	}
	if toolResultIsError(createResult) {
		t.Fatalf("radio_create_channel tool error: %s", toolResultText(createResult))
	}
	if createPath != "/channels" {
		t.Fatalf("expected /channels, got %q", createPath)
	}
	if createAuth != "Bearer oc_agt_test" {
		t.Fatalf("expected create bearer auth, got %q", createAuth)
	}
	if !strings.Contains(toolResultText(createResult), "created Hz/pdf") {
		t.Fatalf("expected Hz/pdf in result, got %s", toolResultText(createResult))
	}

	postResult, err := sdk.handleRadioPost(context.Background(), makeCallReq(map[string]any{
		"channel":       "Hz/general",
		"tag":           "buy",
		"content":       "need an API",
		"bounty_dcents": float64(25),
	}))
	if err != nil {
		t.Fatalf("radio_post returned error: %v", err)
	}
	if toolResultIsError(postResult) {
		t.Fatalf("radio_post tool error: %s", toolResultText(postResult))
	}
	if postPath != "/channels/general/posts" {
		t.Fatalf("expected /channels/general/posts, got %q", postPath)
	}
	if postAuth != "Bearer oc_agt_test" {
		t.Fatalf("expected bearer auth, got %q", postAuth)
	}
	if postBounty != 25 {
		t.Fatalf("expected post bounty 25, got %v", postBounty)
	}
	if !strings.Contains(toolResultText(postResult), "posted to Hz/general") {
		t.Fatalf("expected Hz/general in result, got %s", toolResultText(postResult))
	}
	if !strings.Contains(toolResultText(postResult), "bounty:     25đ (open)") {
		t.Fatalf("expected bounty in post result, got %s", toolResultText(postResult))
	}

	getResult, err := sdk.handleRadioGetPosts(context.Background(), makeCallReq(map[string]any{
		"slug": "finance",
		"tag":  "sell",
	}))
	if err != nil {
		t.Fatalf("radio_get_posts returned error: %v", err)
	}
	if toolResultIsError(getResult) {
		t.Fatalf("radio_get_posts tool error: %s", toolResultText(getResult))
	}
	if getPath != "/channels/finance/posts" || getQuery != "tag=sell" {
		t.Fatalf("unexpected GET route: path=%q query=%q", getPath, getQuery)
	}
	if !strings.Contains(toolResultText(getResult), "Hz/finance posts") {
		t.Fatalf("expected Hz/finance in result, got %s", toolResultText(getResult))
	}
	if !strings.Contains(toolResultText(getResult), "[bounty=25đ status=open]") {
		t.Fatalf("expected bounty metadata in get result, got %s", toolResultText(getResult))
	}

	acceptResult, err := sdk.handleRadioAcceptBounty(context.Background(), makeCallReq(map[string]any{
		"channel":          "Hz/stuck",
		"post_id":          "post-1",
		"accepted_post_id": "reply-1",
	}))
	if err != nil {
		t.Fatalf("radio_accept_bounty returned error: %v", err)
	}
	if toolResultIsError(acceptResult) {
		t.Fatalf("radio_accept_bounty tool error: %s", toolResultText(acceptResult))
	}
	if acceptPath != "/channels/stuck/posts/post-1/accept" {
		t.Fatalf("unexpected accept path: %q", acceptPath)
	}
	if acceptAuth != "Bearer oc_agt_test" {
		t.Fatalf("expected accept bearer auth, got %q", acceptAuth)
	}
	if !strings.Contains(toolResultText(acceptResult), "bounty:        25đ (released)") {
		t.Fatalf("expected released bounty in accept result, got %s", toolResultText(acceptResult))
	}
	if !strings.Contains(toolResultText(acceptResult), "from_agent:    agt-buyer") {
		t.Fatalf("expected transfer agent in accept result, got %s", toolResultText(acceptResult))
	}
}
