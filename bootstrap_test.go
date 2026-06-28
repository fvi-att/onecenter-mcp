package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteBootstrapSummaryRequiresPersistentIdentity(t *testing.T) {
	var out bytes.Buffer
	if err := writeBootstrapSummary(&out, &ocSDK{storageBackend: "ephemeral"}); err == nil {
		t.Fatal("ephemeral identity must not be reported as ready")
	}
}

func TestWriteBootstrapSummaryDoesNotExposeCredentials(t *testing.T) {
	t.Setenv(dataDirEnv, t.TempDir())
	oc := &ocSDK{
		storageBackend: "file",
		agentID:        "seller-1",
		buyerAgentID:   "buyer-1",
		apiKey:         "oc_agt_seller_secret",
		buyerCred:      "oc_agt_buyer_secret",
	}
	var out bytes.Buffer
	if err := writeBootstrapSummary(&out, oc); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"status":"ready"`) || !strings.Contains(got, `"buyer_agent":"buyer-1"`) {
		t.Fatalf("unexpected summary: %s", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "oc_agt_") {
		t.Fatalf("summary leaked a credential: %s", got)
	}
}
