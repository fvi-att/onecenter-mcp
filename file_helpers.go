// file_helpers.go — shared JSON file persistence helpers (used by demand_market_tools.go).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
		return result
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[file-store] read error: %v\n", err)
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
