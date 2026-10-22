package biz

import (
	"errors"
	"strings"
	"testing"
)

// TestWrap_PreservesChainAndSentinel 锁定 Wrap 的核心契约：
//  1. errors.Is(结果, sentinel) 仍为 true —— transport 据此映射错误码不受破坏；
//  2. 根因文本出现在 Error() 链中 —— 日志可追溯到底层成因（排障就靠这个）。
func TestWrap_PreservesChainAndSentinel(t *testing.T) {
	root := errors.New("pq: deadlock detected")
	wrapped := Wrap(ErrInternal, root, "写入用户失败")

	if !errors.Is(wrapped, ErrInternal) {
		t.Fatalf("包装后 errors.Is(ErrInternal) 应为 true，实际 %v", wrapped)
	}
	got := wrapped.Error()
	for _, want := range []string{"写入用户失败", "internal error", "pq: deadlock detected"} {
		if !strings.Contains(got, want) {
			t.Fatalf("错误链缺失 %q，实际: %s", want, got)
		}
	}
}

// TestWrap_NilCauseReturnsSentinel cause 为 nil 时直接返回哨兵本身（无根因可携带）。
func TestWrap_NilCauseReturnsSentinel(t *testing.T) {
	if got := Wrap(ErrInternal, nil, "无根因"); !errors.Is(got, ErrInternal) {
		t.Fatalf("cause 为 nil 时应直接返回哨兵，实际 %v", got)
	}
}

// TestResolveErrorMeta_UnknownFallsBackToInternal 未知错误回退为 500 / INTERNAL_ERROR，不泄露内部细节。
func TestResolveErrorMeta_UnknownFallsBackToInternal(t *testing.T) {
	meta := ResolveErrorMeta(errors.New("some unknown"))
	if meta.HTTPCode != 500 || meta.WSCode != "INTERNAL_ERROR" {
		t.Fatalf("未知错误应回退为 500/INTERNAL_ERROR，实际 %+v", meta)
	}
}

// TestResolveErrorMeta_MapsKnownSentinels 校验注册表对每个哨兵的映射（HTTP code + WS error_code）。
func TestResolveErrorMeta_MapsKnownSentinels(t *testing.T) {
	cases := []struct {
		err      error
		httpCode int
		wsCode   string
	}{
		{ErrRateLimited, 429, "RATE_LIMITED"},
		{ErrQQAlreadyBound, 409, "QQ_ALREADY_BOUND"},
		{ErrNotBound, 409, "NOT_BOUND"},
		{ErrInvalidParam, 400, "INVALID_PARAM"},
		{ErrUsernameTaken, 409, "USERNAME_TAKEN"},
		{ErrUserAlreadyBound, 409, "USER_ALREADY_BOUND"},
		{ErrBindCodeInvalid, 400, "BIND_CODE_INVALID"},
		{ErrInvalidCredentials, 401, "INVALID_CREDENTIALS"},
		{ErrAccountDisabled, 403, "ACCOUNT_DISABLED"},
		{ErrNotAuthenticated, 401, "NOT_AUTHENTICATED"},
		{ErrInternal, 500, "INTERNAL_ERROR"},
	}
	for _, c := range cases {
		meta := ResolveErrorMeta(c.err)
		if meta.HTTPCode != c.httpCode || meta.WSCode != c.wsCode {
			t.Fatalf("哨兵 %v 映射错误，实际 %+v", c.err, meta)
		}
	}

	// 回归：包装后的错误（带根因）也应被正确解析，错误链不能破坏映射。
	if meta := ResolveErrorMeta(Wrap(ErrRateLimited, errors.New("redis down"), "限流计数失败")); meta.HTTPCode != 429 || meta.WSCode != "RATE_LIMITED" {
		t.Fatalf("包装后的 ErrRateLimited 应仍映射到 429/RATE_LIMITED，实际 %+v", meta)
	}
}
