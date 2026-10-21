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
}

func (f *fakeUserStore) ExistsByUsername(_ context.Context, _ string) (bool, error) {
	return f.exists, f.existsErr
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

// ---- CheckUsername ----

func TestCheckUsername_InvalidParam(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, AuthConfig{})
	// 长度不足 + 含非法字符
	if _, err := svc.CheckUsername(context.Background(), "1.2.3.4", "ab"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
	}
}

func TestCheckUsername_RateLimited(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: false}, AuthConfig{})
	if _, err := svc.CheckUsername(context.Background(), "1.2.3.4", "clover"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestCheckUsername_Exists(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: true}, &fakeRateLimiter{allowed: true}, AuthConfig{})
	exists, err := svc.CheckUsername(context.Background(), "1.2.3.4", "Clover")
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if !exists {
		t.Fatalf("期望 exists=true（Clover 与已存在 user 在 SQL 层统一小写后命中）")
	}
}

func TestCheckUsername_NotExists(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: false}, &fakeRateLimiter{allowed: true}, AuthConfig{})
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
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: true}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "short"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
	}
}

func TestRegister_RateLimited(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{}, &fakeRateLimiter{allowed: false}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestRegister_UsernameTaken_Precheck(t *testing.T) {
	svc := NewAuthService(&fakeUserStore{exists: true}, &fakeRateLimiter{allowed: true}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken，实际 %v", err)
	}
}

func TestRegister_Success(t *testing.T) {
	store := &fakeUserStore{exists: false}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, AuthConfig{})
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
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, AuthConfig{})
	if err := svc.Register(context.Background(), "1.2.3.4", "clover", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken（唯一约束兜底），实际 %v", err)
	}
}

func TestRegister_DBError(t *testing.T) {
	store := &fakeUserStore{exists: false, createErr: errors.New("pg down")}
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, AuthConfig{})
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
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, AuthConfig{})
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
	svc := NewAuthService(store, &fakeRateLimiter{allowed: true}, AuthConfig{})
	// 首次以不同大小写注册（newuser 已占）→ 应直接判重复。
	if err := svc.Register(context.Background(), "1.2.3.4", "NewUser", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("期望 ErrUsernameTaken（大小写不敏感重复），实际 %v", err)
	}
}
