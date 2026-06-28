package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestMCPServer_FrictionInstructions_R29(t *testing.T) {
	s := server.NewMCPServer("onecenter-mcp", "0.3.0",
		server.WithInstructions(frictionInstructions),
	)

	msg := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      mcp.NewRequestId(int64(1)),
		Request: mcp.Request{Method: "initialize"},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	resp := s.HandleMessage(context.Background(), raw)
	rpcResp, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("expected JSONRPCResponse, got %T", resp)
	}
	initResult, ok := rpcResp.Result.(mcp.InitializeResult)
	if !ok {
		t.Fatalf("expected InitializeResult, got %T", rpcResp.Result)
	}

	instr := initResult.Instructions
	if instr == "" {
		t.Fatal("instructions must not be empty")
	}
	for _, needle := range []string{"friction", "摩擦", "CLAUDE.md"} {
		if !strings.Contains(instr, needle) {
			t.Errorf("instructions missing %q", needle)
		}
	}
}
