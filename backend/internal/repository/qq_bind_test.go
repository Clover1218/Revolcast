package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

func TestUniqueViolationOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"码唯一约束（实际索引名）", &pgconn.PgError{Code: "23505", ConstraintName: "qq_bind_codes_code_key"}, true},
		{"码唯一约束（历史名）", &pgconn.PgError{Code: "23505", ConstraintName: "ux_qq_bind_codes_code"}, true},
		{"待使用码唯一约束", &pgconn.PgError{Code: "23505", ConstraintName: "ux_qq_bind_codes_pending_qq"}, true},
		{"其他唯一约束", &pgconn.PgError{Code: "23505", ConstraintName: "some_other_key"}, false},
		{"非唯一冲突（23503 外键）", &pgconn.PgError{Code: "23503", ConstraintName: "qq_bind_codes_code_key"}, false},
		{"普通错误", errors.New("boom"), false},
		{"被包裹的唯一冲突", fmt.Errorf("insert 失败: %w", &pgconn.PgError{Code: "23505", ConstraintName: "ux_qq_bind_codes_pending_qq"}), true},
	}
	for _, c := range cases {
		if got := uniqueViolationOn(c.err, "qq_bind_codes_code_key", "ux_qq_bind_codes_code", "ux_qq_bind_codes_pending_qq"); got != c.want {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}
