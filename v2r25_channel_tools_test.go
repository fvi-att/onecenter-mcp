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
	var getPath string
	var getQuery string

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
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"post": map[string]any{
					"id":         "post-1",
					"channel_id": "channel-general",
					"tag":        "buy",
					"content":    "need an API",
					"created_at": "2026-06-15T00:00:00Z",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/channels/finance/posts":
			getPath = r.URL.Path
			getQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(map[string]any{
				"posts": []map[string]any{
					{"id": "post-2", "tag": "sell", "content": "finance API", "created_at": "2026-06-15T00:00:01Z"},
				},
				"next_cursor": "",
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
		"channel": "Hz/general",
		"tag":     "buy",
		"content": "need an API",
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
	if !strings.Contains(toolResultText(postResult), "posted to Hz/general") {
		t.Fatalf("expected Hz/general in result, got %s", toolResultText(postResult))
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
}
