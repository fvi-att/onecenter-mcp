// p2p_store.go — shared P2P local-file persistence (split from main.go, B17).
// Used by U1 settlement (agreements/), U2 quotation (quotations/), and bootstrap hydrate.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *ocSDK) p2pFilePath(docType, role, id string) string {
	base := s.p2pBaseDir
	if base == "" {
		base = filepath.Join(mustOneCenterDataDir(), "p2p")
	}
	return filepath.Join(base, s.agentID, docType, role, id+".json")
}

// saveP2PFile — JSON データを 0600 ファイルとして保存する (ディレクトリは自動作成)
func saveP2PFile(path string, data map[string]any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, b)
}

// loadP2PDir — ディレクトリ内の全 JSON ファイルを読み込んで map[id]→data を返す
func loadP2PDir(dir string) map[string]map[string]any {
	result := make(map[string]map[string]any)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result // ディレクトリ未作成は正常 (初回起動)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[p2p-store] read error: %v\n", err)
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		result[strings.TrimSuffix(e.Name(), ".json")] = m
	}
	return result
}

// hydrateFromFiles — 起動時にファイルから runtime memory へ復元する (v2-r17)
func (s *ocSDK) hydrateFromFiles() {
	// Quotation: received (Buyer)
	for id, q := range loadP2PDir(filepath.Dir(s.p2pFilePath("quotations", "received", "_"))) {
		s.qmu.Lock()
		if _, seen := s.seenQIDs[id]; !seen {
			s.seenQIDs[id] = struct{}{}
			s.localQuotations[id] = q
		}
		s.qmu.Unlock()
	}
	// Quotation: sent (Seller)
	for id, q := range loadP2PDir(filepath.Dir(s.p2pFilePath("quotations", "sent", "_"))) {
		s.qmu.Lock()
		if _, seen := s.seenQIDs[id]; !seen {
			s.seenQIDs[id] = struct{}{}
			s.localQuotations["sent:"+id] = q
		}
		s.qmu.Unlock()
	}
	// Agreement: seller
	for id, a := range loadP2PDir(filepath.Dir(s.p2pFilePath("agreements", "seller", "_"))) {
		s.amu.Lock()
		key := "seller:" + id
		if _, seen := s.seenCallIDs[id]; !seen {
			s.seenCallIDs[id] = struct{}{}
			s.localAgreements[key] = a
		}
		s.amu.Unlock()
	}
	// Agreement: buyer
	for id, a := range loadP2PDir(filepath.Dir(s.p2pFilePath("agreements", "buyer", "_"))) {
		s.amu.Lock()
		key := "buyer:" + id
		if _, seen := s.seenCallIDs[id]; !seen {
			s.seenCallIDs[id] = struct{}{}
			s.localAgreements[key] = a
		}
		s.amu.Unlock()
	}
	total := len(s.localQuotations) + len(s.localAgreements)
	if total > 0 {
		fmt.Fprintf(os.Stderr, "[p2p-store] hydrated: quotations=%d agreements=%d\n",
			len(s.localQuotations), len(s.localAgreements))
	}
}

// ── Pure computation helpers (billing 抜きの純粋関数; call_capability から再利用) ──
