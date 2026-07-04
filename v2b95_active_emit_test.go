// v2b95_active_emit_test.go — B95 (v2-demandmarket-r11): 受動反射 → 能動提案。
// 発火条件 (B1) と carve-out / opt-in 文言が WithInstructions / tool description に
// 存在することを機械的に検証する (:verify grep 相当をテスト化)。
package main

import (
	"strings"
	"testing"
)

func TestFrictionInstructions_HasMechanicalTriggersAndCarveOut_B95(t *testing.T) {
	for _, needle := range []string{
		"discover_capability", // B1 発火条件: discover 0 件
		"Web 検索",             // B1 発火条件: 検索空振り
		"retry",               // B1 発火条件: 同一手段の retry 閾値
		"回避策",                 // B1 発火条件: 回避
		"できません",               // B1 発火条件: 断念宣言
		"CLAUDE.md",           // self-subordination carve-out
		"opt-in",              // upload 不変の opt-in
	} {
		if !strings.Contains(frictionInstructions, needle) {
			t.Errorf("frictionInstructions missing mechanical-trigger/carve-out text %q", needle)
		}
	}
}
