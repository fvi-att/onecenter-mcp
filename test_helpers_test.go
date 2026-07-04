package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

type mockAPI struct {
	capabilities   []map[string]any
	demandSignals  []map[string]any
	lastWalletPath string
}

func newMockAPI(caps []map[string]any) *mockAPI { return &mockAPI{capabilities: caps} }

func (m *mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/capabilities":
		query := r.URL.Query().Get("semantic")
		result := make([]map[string]any, 0)
		for _, c := range m.capabilities {
			if query == "" || mockSemanticMatch(c, query) {
				result = append(result, c)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": result, "total": len(result)})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/capabilities/"):
		id := strings.TrimPrefix(r.URL.Path, "/capabilities/")
		for _, c := range m.capabilities {
			if c["id"] == id {
				_ = json.NewEncoder(w).Encode(c)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && r.URL.Path == "/demand/signals":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.demandSignals = append(m.demandSignals, body)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/dcent/wallets/"):
		m.lastWalletPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"balance_dcents": int64(995)})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"not found","path":%q}`, r.URL.Path)
	}
}

func mockSemanticMatch(cap map[string]any, query string) bool {
	name, _ := cap["name"].(string)
	desc, _ := cap["description"].(string)
	for _, token := range strings.Fields(strings.ToLower(query)) {
		if strings.Contains(strings.ToLower(name), token) || strings.Contains(strings.ToLower(desc), token) {
			return true
		}
	}
	return false
}

var testWordCountCap = map[string]any{
	"id":                  "cap-word-count-001",
	"name":                "@onecenter/operator.word_count",
	"description":         "Count words sentences characters in text",
	"protocol":            "mcp",
	"pricing_model":       "free",
	"price_dcents":         int64(0),
	"sig_status":          "no-pubkey",
	"seller_principal_id": "principal-2",
	"mcp_endpoint":        "mcp://onecenter-operator/word_count",
	"reputation":          map[string]any{"success_rate": 1.0, "p50_latency_ms": 1, "p95_latency_ms": 2, "volume_30d": 0},
}

func newTestSDK(ts *httptest.Server) *ocSDK {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &ocSDK{
		principalID: "principal-1",
		cred:        "oc_prn_test",
		oncenterURL: ts.URL,
		sessionID:   "test-session",
		privKey:     priv,
		pubKeyB64:   base64.RawURLEncoding.EncodeToString(pub),
		client:      ts.Client(),
	}
}

func toolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return ""
	}
	return content.Text
}

func toolResultIsError(result *mcp.CallToolResult) bool { return result != nil && result.IsError }

func makeCallReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}
