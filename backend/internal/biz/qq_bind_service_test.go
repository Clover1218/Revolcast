package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"revolcast-backend/internal/repository"
)

// ---- fakes ----

type fakeBindSessionResolver struct {
	userID int64
	ok     bool
	err    error
}

func (f *fakeBindSessionResolver) ResolveSession(_ context.Context, _ string) (int64, bool, error) {
	return f.userID, f.ok, f.err
}

type fakeUserStatusStore struct {
	user *repository.UserRecord
	err  error
}

func (f *fakeUserStatusStore) GetByID(_ context.Context, _ int64) (*repository.UserRecord, error) {
	return f.user, f.err
}

type fakeQQBindStore struct {
	qqUIN       int64
	botID       string
	err         error
	unbindErr   error
	unbindCalls int
}

func (f *fakeQQBindStore) ConsumeBindCodeAndBind(_ context.Context, _ string, _ int64) (int64, string, error) {
	return f.qqUIN, f.botID, f.err
}

func (f *fakeQQBindStore) UnbindByUser(_ context.Context, _ int64) (int64, string, error) {
	f.unbindCalls++
	return f.qqUIN, f.botID, f.unbindErr
}

type fakeBindRateLimiter struct {
	allowed bool
	err     error

	// calls / lastKey 用于断言「限流是否被调用」「限流键维度是否正确」。
	calls   int
	lastKey string
}

func (f *fakeBindRateLimiter) AllowAttempt(_ context.Context, key string, _ time.Duration, _ int64) (bool, error) {
	f.calls++
	f.lastKey = key
	return f.allowed, f.err
}

// newBindQQService 组装一个 QQBindService 测试实例。
func newBindQQService(sess *fakeBindSessionResolver, users *fakeUserStatusStore, qq *fakeQQBindStore, rl *fakeBindRateLimiter) *QQBindService {
	return NewQQBindService(sess, users, qq, rl)
}

// ---- BindQQ ----

func TestBindQQ_Success(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{qqUIN: 10001, botID: "botA"},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); err != nil {
		t.Fatalf("期望绑定成功，实际 %v", err)
	}
}

func TestBindQQ_InvalidParam_CodeLength(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	// 长度不为 8 直接 400（先于任何 DB 访问）。
	if err := svc.BindQQ(context.Background(), "raw-token", "ABC"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
	}
}

func TestBindQQ_NotAuthenticated(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{ok: false}, // 会话无效
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 1, Status: 1}},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("期望 ErrNotAuthenticated，实际 %v", err)
	}
}

func TestBindQQ_SessionResolveError_Internal(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{err: errors.New("pg down")},
		&fakeUserStatusStore{},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

func TestBindQQ_AccountDisabled(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 0}}, // 禁用
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("期望 ErrAccountDisabled，实际 %v", err)
	}
}

func TestBindQQ_UserNotFound_NotAuthenticated(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{err: repository.ErrUserNotFound},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("期望 ErrNotAuthenticated（会话指向的账号不存在），实际 %v", err)
	}
}

func TestBindQQ_RateLimited(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: false}, // 命中限流
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestBindQQ_BindCodeInvalid(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{err: repository.ErrBindCodeInvalid},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrBindCodeInvalid) {
		t.Fatalf("期望 ErrBindCodeInvalid，实际 %v", err)
	}
}

func TestBindQQ_UserAlreadyBound(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{err: repository.ErrUserAlreadyBound},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrUserAlreadyBound) {
		t.Fatalf("期望 ErrUserAlreadyBound，实际 %v", err)
	}
}

func TestBindQQ_QQAlreadyBound(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{err: repository.ErrQQAlreadyBound},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrQQAlreadyBound) {
		t.Fatalf("期望 ErrQQAlreadyBound，实际 %v", err)
	}
}

func TestBindQQ_ConsumeInternalError(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{err: errors.New("tx deadlock")},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal（仓库层未知错误映射），实际 %v", err)
	}
}

// ---- UnbindQQ（BIZ-007） ----

func TestUnbindQQ_Success(t *testing.T) {
	store := &fakeQQBindStore{qqUIN: 10001, botID: "botA"}
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		store,
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); err != nil {
		t.Fatalf("期望解绑成功，实际 %v", err)
	}
	if store.unbindCalls != 1 {
		t.Fatalf("期望调用仓库解绑 1 次，实际 %d", store.unbindCalls)
	}
}

// 禁用账号仍允许解绑：解绑是释放资源操作，不校验 users.status（HTTP 接口文档 §8 关键设计决策）。
func TestUnbindQQ_AccountDisabledStillAllowed(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 0}}, // 已禁用
		&fakeQQBindStore{qqUIN: 10001, botID: "botA"},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); err != nil {
		t.Fatalf("期望禁用账号也能解绑成功，实际 %v", err)
	}
}

func TestUnbindQQ_NotAuthenticated(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{ok: false}, // 会话无效
		&fakeUserStatusStore{},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("期望 ErrNotAuthenticated，实际 %v", err)
	}
}

func TestUnbindQQ_SessionResolveError_Internal(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{err: errors.New("pg down")},
		&fakeUserStatusStore{},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

func TestUnbindQQ_NotBound(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{},
		&fakeQQBindStore{unbindErr: repository.ErrNotBound}, // 无 active 绑定
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("期望 ErrNotBound（当前无有效绑定），实际 %v", err)
	}
}

func TestUnbindQQ_RateLimited(t *testing.T) {
	store := &fakeQQBindStore{}
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{},
		store,
		&fakeBindRateLimiter{allowed: false}, // 命中限流
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
	if store.unbindCalls != 0 {
		t.Fatalf("限流时不应触达仓库，实际调用 %d 次", store.unbindCalls)
	}
}

func TestUnbindQQ_StoreInternalError(t *testing.T) {
	svc := newBindQQService(
		&fakeBindSessionResolver{userID: 42, ok: true},
		&fakeUserStatusStore{},
		&fakeQQBindStore{unbindErr: errors.New("db timeout")},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal（仓库层未知错误映射），实际 %v", err)
	}
}

// hashCheckingSession 只有「请求侧哈希 == 预期哈希」时才认为会话有效，
// 用于锁定 resolveUserID 与登录签发侧使用同一哈希输入（回归：曾对随机字节而非 Token 字符串求哈希）。
type hashCheckingSession struct{ expectedHash string }

func (f *hashCheckingSession) ResolveSession(_ context.Context, tokenHash string) (int64, bool, error) {
	return 42, tokenHash == f.expectedHash, nil
}

func TestUnbindQQ_ResolvesSessionWithCanonicalHash(t *testing.T) {
	svc := NewQQBindService(
		&hashCheckingSession{expectedHash: sessionTokenHash("raw-token")},
		&fakeUserStatusStore{},
		&fakeQQBindStore{},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.UnbindQQ(context.Background(), "raw-token"); err != nil {
		t.Fatalf("期望用原始 Token 的 SHA-256 命中会话，实际 %v", err)
	}
}

func TestBindQQ_ResolvesSessionWithCanonicalHash(t *testing.T) {
	svc := NewQQBindService(
		&hashCheckingSession{expectedHash: sessionTokenHash("raw-token")},
		&fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		&fakeQQBindStore{qqUIN: 10001, botID: "botA"},
		&fakeBindRateLimiter{allowed: true},
	)
	if err := svc.BindQQ(context.Background(), "raw-token", "ABCDEFGH"); err != nil {
		t.Fatalf("期望用原始 Token 的 SHA-256 命中会话，实际 %v", err)
	}
}
