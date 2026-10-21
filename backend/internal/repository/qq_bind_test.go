package repository

import (
	"strings"
	"testing"
)

func TestGenerateBindCode(t *testing.T) {
	const excluded = "0O1IL"
	seen := make(map[string]struct{})

	for i := 0; i < 2000; i++ {
		code, err := generateBindCode()
		if err != nil {
			t.Fatalf("generateBindCode 返回错误: %v", err)
		}
		if len(code) != 8 {
			t.Fatalf("绑定码长度应为 8，实际 %d: %q", len(code), code)
		}
		for _, r := range code {
			if strings.ContainsRune(excluded, r) {
				t.Fatalf("绑定码包含易混淆字符 %q: %q", r, code)
			}
			if !strings.ContainsRune(bindCodeAlphabet, r) {
				t.Fatalf("绑定码包含非法字符 %q: %q", r, code)
			}
		}
		seen[code] = struct{}{}
	}

	// 2000 次生成应基本无碰撞（字符空间 32^8），宽松校验多样性。
	if len(seen) < 1900 {
		t.Fatalf("绑定码碰撞过多，疑似随机性不足: 唯一数 %d/2000", len(seen))
	}
}
