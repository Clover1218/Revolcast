package biz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"revolcast-backend/internal/repository"
)

// ---- fakes ----

type fakeUserStore struct {
	exists       bool
	existsErr    error
	createErr    error
	createCalled bool
	createdUser  string
	createdHash  string
	user         *repository.UserRecord
	getUserErr   error
}

func (f *fakeUserStore) ExistsByUsername(_ context.Context, _ string) (bool, error) {
	return f.exists, f.existsErr
}

func (f *fakeUserStore) GetByUsername(_ context.Context, _ string) (*repository.UserRecord, error) {
	if f.getUserErr != nil {
		return nil, f.getUserErr
	}
	return f.user, nil
}

func (f *fakeUserStore) CreateUser(_ context.Context, username, passwordHash string) error {
	f.createCalled = true
	f.createdUser = username
	f.createdHash = passwordHash
	return f.createErr
}

type fakeRateLimiter struct {
	allowed bool
	err     error
}

func (f *fakeRateLimiter) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, f.err
}

// fakeSessionStore 实现 biz.SessionStore，记录写入参数以便断言。
type fakeSessionStore struct {
	createCalled     bool
	createdUserID    int64
	createdTokenHash string
	createErr        error
	revokeAffected   bool
	revokeErr        error
	revokedHash      string
	resolveUserID    int64
	resolveOK        bool
	resolveErr       error
	resolvedHash     string
}

func (f *fakeSessionStore) CreateSession(_ context.Context, userID int64, tokenHash, _ string, _ time.Time) error {
	f.createCalled = true
	f.createdUserID = userID
	f.createdTokenHash = tokenHash
	return f.createErr
}

func (f *fakeSessionStore) RevokeSession(_ context.Context, tokenHash string) (bool, error) {
	f.revokedHash = tokenHash
	return f.revokeAffected, f.revokeErr
}

func (f *fakeSessionStore) ResolveSession(_ context.Context, tokenHash string) (int64, bool, error) {
	f.resolvedHash = tokenHash
	return f.resolveUserID, f.resolveOK, f.resolveErr
}

// ---- CheckUsername ----

func TestCheckUsername_InvalidParam(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	// 长度不足 + 含非法字符
	if _, err := svc.CheckUsername(context.Background(), "1.2.3.4", "ab"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
	}
}

func TestCheckUsername_RateLimited(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: false}, &fakeSessionStore{}, AuthConfig{})
	if _, err := svc.CheckUsername(context.Background(), "1.2.3.4", "clover"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestCheckUsername_Exists(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: true}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	exists, err := svc.CheckUsername(context.Background(), "1.2.3.4", "Clover")
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if !exists {
		t.Fatalf("期望 exists=true（Clover 与已存在 user 在 SQL 层统一小写后命中）")
	}
}

func TestCheckUsername_NotExists(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: false}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	exists, err := svc.CheckUsername(context.Background(), "1.2.3.4", "clover")
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if exists {
		t.Fatalf("期望 exists=false")
	}
}

// ---- Register ----

func TestRegister_InvalidParam_ShortPassword(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "short"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
	}
}

func TestRegister_RateLimited(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: false}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestRegister_UsernameTaken_Precheck(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: true}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken，实际 %v", err)
	}
}

func TestRegister_Success(t *testing.T) {
	store := &fakeUserStore{exists: false}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "Clover", "password123"); err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if !store.createCalled {
		t.Fatalf("应调用 CreateUser")
	}
	if store.createdUser != "Clover" {
		t.Fatalf("username 应保留原始大小写 Clover（仅裁剪，不转小写），实际 %q", store.createdUser)
	}
	// 落库的是 bcrypt 哈希，且 cost=12，可用原密码校验。
	if err := bcrypt.CompareHashAndPassword([]byte(store.createdHash), []byte("password123")); err != nil {
		t.Fatalf("password_hash 不是该密码的有效 bcrypt 哈希: %v", err)
	}
	// 明文绝不应出现在存储中。
	if store.createdHash == "password123" {
		t.Fatalf("明文密码被直接存储，严重违反安全约定")
	}
}

func TestRegister_UniqueConflictOnInsert(t *testing.T) {
	// 预检不存在，但并发插入触发唯一约束（数据库兜底）。
	store := &fakeUserStore{exists: false, createErr: repository.ErrUsernameTaken}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken（唯一约束兜底），实际 %v", err)
	}
}

func TestRegister_DBError(t *testing.T) {
	store := &fakeUserStore{exists: false, createErr: errors.New("pg down")}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

// ---- 大小写区分相关（入库保留原大小写、查重大小写不敏感）----

// caseInsensitiveFakeStore 在内存中模拟 users 表：以 lower(username) 为唯一键，
// 但透传调用方传入的原始大小写（与 repository.UserRepo 行为一致）。
type caseInsensitiveFakeStore struct {
	data      map[string]string // lower(username) -> 任意标记
	existErr  error
	createErr error
}

func (f *caseInsensitiveFakeStore) ExistsByUsername(_ context.Context, username string) (bool, error) {
	if f.existErr != nil {
		return false, f.existErr
	}
	_, ok := f.data[strings.ToLower(username)]
	return ok, nil
}

func (f *caseInsensitiveFakeStore) GetByUsername(_ context.Context, _ string) (*repository.UserRecord, error) {
	// 该 fake 仅用于注册的大小写相关用例，不测登录；直接返回未找到。
	return nil, repository.ErrUserNotFound
}

func (f *caseInsensitiveFakeStore) CreateUser(_ context.Context, username, _ string) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.data[strings.ToLower(username)]; ok {
		return repository.ErrUsernameTaken
	}
	f.data[strings.ToLower(username)] = username // 记录原始大小写
	return nil
}

// TestRegister_PreservesOriginalCase 验证：入库用户名保留用户输入大小写，不强制小写。
func TestRegister_PreservesOriginalCase(t *testing.T) {
	store := &caseInsensitiveFakeStore{data: map[string]string{}}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "Clover_99", "password123"); err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	// 查重大小写不敏感：注册后用小写查询应判定为已存在。
	exists, err := svc.CheckUsername(context.Background(), "1.2.3.4", "clover_99")
	if err != nil {
		t.Fatalf("查重不应报错: %v", err)
	}
	if !exists {
		t.Fatalf("期望大小写不敏感查重命中 clover_99")
	}
	// 存储保留原始大小写 Clover_99（而非全小写）。
	if got := store.data["clover_99"]; got != "Clover_99" {
		t.Fatalf("期望库内保留原始大小写 Clover_99，实际 %q", got)
	}
}

// TestRegister_CaseInsensitiveDedup 验证：不同大小写视为重复，返回 409。
func TestRegister_CaseInsensitiveDedup(t *testing.T) {
	store := &caseInsensitiveFakeStore{data: map[string]string{"newuser": "NewUser"}}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	// 首次以不同大小写注册（newuser 已占）→ 应直接判重复。
	if err := svc.Register(context.Background(), "1.2.3.4", "NewUser", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken（大小写不敏感重复），实际 %v", err)
	}
}

// ---- Login (BIZ-004) ----

func newLoginService(user *repository.UserRecord, getUserErr error, allowed bool) (*AuthService, *fakeSessionStore) {
	store := &fakeUserStore{user: user, getUserErr: getUserErr}
	rl := &fakeRateLimiter{allowed: allowed}
	sess := &fakeSessionStore{}
	svc := NewAuthService(store, rl, sess, AuthConfig{})
	return svc, sess
}

func TestLogin_Success(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcryptCost)
	svc, sess := newLoginService(&repository.UserRecord{ID: 7, PasswordHash: string(hash), Status: 1}, nil, true)
	raw, expiresAt, err := svc.Login(context.Background(), "1.2.3.4", "Clover", "password123", "unittest")
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if raw == "" {
		t.Fatalf("应返回非空原始 Token")
	}
	if !sess.createCalled {
		t.Fatalf("应写入会话")
	}
	if sess.createdUserID != 7 {
		t.Fatalf("会话应归属 user_id=7，实际 %d", sess.createdUserID)
	}
	// token_hash 应为 64 字符 SHA-256，且不等于原始 Token（库不存明文）。
	if len(sess.createdTokenHash) != 64 {
		t.Fatalf("token_hash 应为 64 字符 SHA-256，实际 %q", sess.createdTokenHash)
	}
	if sess.createdTokenHash == raw {
		t.Fatalf("库内不应存储原始 Token 明文")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("expiresAt 应晚于当前")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("rightpass"), bcryptCost)
	svc, _ := newLoginService(&repository.UserRecord{ID: 1, PasswordHash: string(hash), Status: 1}, nil, true)
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "wrongpass", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("期望 ErrInvalidCredentials，实际 %v", err)
	}
}

func TestLogin_UserNotFound(t *testing.T) {
	svc, _ := newLoginService(nil, repository.ErrUserNotFound, true)
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "ghost", "whatever", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("期望 ErrInvalidCredentials（不暴露账号是否存在），实际 %v", err)
	}
}

func TestLogin_Disabled(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcryptCost)
	svc, _ := newLoginService(&repository.UserRecord{ID: 1, PasswordHash: string(hash), Status: 0}, nil, true)
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "password123", ""); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("期望 ErrAccountDisabled，实际 %v", err)
	}
}

func TestLogin_InvalidParam(t *testing.T) {
	svc, _ := newLoginService(nil, nil, true)
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "ab", "password123", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam（用户名过短），实际 %v", err)
	}
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "short", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam（密码过短），实际 %v", err)
	}
}

func TestLogin_RateLimited(t *testing.T) {
	svc, _ := newLoginService(nil, nil, false)
	if _, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "password123", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

// ---- Logout (BIZ-005) ----

func TestLogout_Success(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{revokeAffected: true}, AuthConfig{})
	if err := svc.Logout(context.Background(), "some-raw-token"); err != nil {
		t.Fatalf("期望登出成功，实际 %v", err)
	}
}

func TestLogout_EmptyToken_ReturnsNotAuthenticated(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{}, AuthConfig{})
	if err := svc.Logout(context.Background(), ""); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("期望 ErrNotAuthenticated（空 Token），实际 %v", err)
	}
}

func TestLogout_NoActiveSession_ReturnsNotAuthenticated(t *testing.T) {
	// revokeAffected=false 模拟令牌不存在 / 已撤销 / 已过期。
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{revokeAffected: false}, AuthConfig{})
	if err := svc.Logout(context.Background(), "stale-token"); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("期望 ErrNotAuthenticated（无有效会话），实际 %v", err)
	}
}

func TestLogout_DBError_ReturnsInternal(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, &fakeSessionStore{revokeErr: errors.New("pg down")}, AuthConfig{})
	if err := svc.Logout(context.Background(), "token"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

// ---- 会话 Token 哈希一致性（回归测试） ----
//
// 历史缺陷：generateToken 对 32 字节随机原文求哈希，而校验侧（Logout / resolveUserID）
// 对「十六进制 Token 字符串」求哈希——sha256(b) 与 sha256(hex(b)) 永不相等，
// 表现为登录成功但后续所有需要会话的请求一律 401。以下用例锁定两侧输入必须同源。

func TestGenerateToken_HashMatchesVerification(t *testing.T) {
	raw, hash, err := generateToken()
	if err != nil {
		t.Fatalf("生成 Token 不应报错: %v", err)
	}
	if len(raw) != 64 || len(hash) != 64 {
		t.Fatalf("原始 Token 与哈希均应为 64 字符十六进制，实际 raw=%d hash=%d", len(raw), len(hash))
	}
	if got := sessionTokenHash(raw); got != hash {
		t.Fatalf("签发侧 token_hash 与校验侧哈希不一致：sign=%s verify=%s", hash, got)
	}
}

// matchingSessionStore 模拟真实 web_sessions 的比对行为：
// 只有请求侧算出的哈希与登录写入的 token_hash 完全一致，才认为会话有效。
type matchingSessionStore struct {
	tokenHash string
}

func (f *matchingSessionStore) CreateSession(_ context.Context, _ int64, tokenHash, _ string, _ time.Time) error {
	f.tokenHash = tokenHash
	return nil
}

func (f *matchingSessionStore) RevokeSession(_ context.Context, tokenHash string) (bool, error) {
	return tokenHash != "" && tokenHash == f.tokenHash, nil
}

func (f *matchingSessionStore) ResolveSession(_ context.Context, tokenHash string) (int64, bool, error) {
	return 7, tokenHash != "" && tokenHash == f.tokenHash, nil
}

// TestLoginThenLogout_RoundTrip 登录下发的原始 Token 必须能通过校验侧哈希匹配（端到端锁定哈希链路）。
func TestLoginThenLogout_RoundTrip(t *testing.T) {
	pwHash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcryptCost)
	sess := &matchingSessionStore{}
	svc := NewAuthService(
		&fakeUserStore{user: &repository.UserRecord{ID: 7, PasswordHash: string(pwHash), Status: 1}},
		&fakeRateLimiter{allowed: true},
		sess,
		AuthConfig{},
	)

	raw, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "password123", "unittest")
	if err != nil {
		t.Fatalf("登录不应报错: %v", err)
	}
	if err := svc.Logout(context.Background(), raw); err != nil {
		t.Fatalf("用登录返回的原始 Token 登出应成功（哈希必须一致），实际 %v", err)
	}
}

// TestResolveUserID_RoundTrip 锁定 WebSocket 握手复用会话鉴权时的哈希链路：
// 登录下发的原始 Token 必须能被 ResolveUserID 解析为同一 user_id，错误 Token 一律 ErrNotAuthenticated。
func TestResolveUserID_RoundTrip(t *testing.T) {
	pwHash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcryptCost)
	svc := NewAuthService(
		&fakeUserStore{user: &repository.UserRecord{ID: 7, PasswordHash: string(pwHash), Status: 1}},
		&fakeRateLimiter{allowed: true},
		&matchingSessionStore{},
		AuthConfig{},
	)

	raw, _, err := svc.Login(context.Background(), "1.2.3.4", "clover", "password123", "unittest")
	if err != nil {
		t.Fatalf("登录不应报错: %v", err)
	}

	uid, err := svc.ResolveUserID(context.Background(), raw)
	if err != nil || uid != 7 {
		t.Fatalf("原始 Token 应解析出 userID=7，实际 uid=%d err=%v", uid, err)
	}
	if _, err := svc.ResolveUserID(context.Background(), "not-a-real-token"); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("无效 Token 应返回 ErrNotAuthenticated，实际 %v", err)
	}
	if _, err := svc.ResolveUserID(context.Background(), ""); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("空 Token 应返回 ErrNotAuthenticated，实际 %v", err)
	}
}

// TestResolveUserID_DBError 会话解析的 DB 错误必须包裹为 ErrInternal 并保留根因。
func TestResolveUserID_DBError(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true},
		&fakeSessionStore{resolveErr: errors.New("pg down")}, AuthConfig{})
	if _, err := svc.ResolveUserID(context.Background(), "token"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}
