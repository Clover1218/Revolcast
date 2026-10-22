package biz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"revolcast-backend/internal/repository"
)

const (
	authRateLimitWindow = 5 * time.Minute     // 限流窗口（HTTP 接口文档 §6）
	authCheckNameMax    = 10                  // check-name 每 IP 窗口内上限
	authRegisterMax     = 5                   // register 每 IP 窗口内上限
	loginMax            = 5                   // login 每 IP / 每 username 窗口内上限（HTTP 接口文档 §6）
	loginSessionTTL     = 30 * 24 * time.Hour // 登录会话有效期（HTTP 接口文档 §5）
	bcryptCost          = 12                  // 密码哈希成本（数据库文档 §3.1）
	usernameMinLen      = 3
	usernameMaxLen      = 64 // 对齐 VARCHAR(64)
	passwordMinLen      = 8
	usernamePattern     = `^[a-zA-Z0-9_]+$`
)

var usernameRegexp = regexp.MustCompile(usernamePattern)

// UserStore 是 AuthService 对用户的存储依赖（便于测试注入 fake）。
type UserStore interface {
	ExistsByUsername(ctx context.Context, username string) (bool, error)
	GetByUsername(ctx context.Context, username string) (*repository.UserRecord, error)
	CreateUser(ctx context.Context, username, passwordHash string) error
}

// RateLimiter 是 AuthService 对限流的依赖（便于测试注入 fake）。
type RateLimiter interface {
	AllowAttempt(ctx context.Context, key string, window time.Duration, max int64) (bool, error)
}

// SessionStore 是 AuthService 对会话写入/撤销的依赖（便于测试注入 fake）。
type SessionStore interface {
	CreateSession(ctx context.Context, userID int64, tokenHash, deviceName string, expiresAt time.Time) error
	RevokeSession(ctx context.Context, tokenHash string) (affected bool, err error)
}

// AuthConfig 鉴权业务 tunable 参数。
type AuthConfig struct {
	RateLimitWindow time.Duration // 限流窗口
	CheckNameMax    int64         // check-name 窗口内最大次数
	RegisterMax     int64         // register 窗口内最大次数
	LoginMax        int64         // login 窗口内最大次数（IP 与 username 各计一份）
	SessionTTL      time.Duration // 登录会话有效期
}

// AuthService 实现 BIZ-002（校验用户名可用性）/ BIZ-003（注册网站账号）/ BIZ-004（账号登录）。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type AuthService struct {
	users    UserStore
	rl       RateLimiter
	sessions SessionStore
	cfg      AuthConfig
}

func NewAuthService(users UserStore, rl RateLimiter, sessions SessionStore, cfg AuthConfig) *AuthService {
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = authRateLimitWindow
	}
	if cfg.CheckNameMax <= 0 {
		cfg.CheckNameMax = authCheckNameMax
	}
	if cfg.RegisterMax <= 0 {
		cfg.RegisterMax = authRegisterMax
	}
	if cfg.LoginMax <= 0 {
		cfg.LoginMax = loginMax
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = loginSessionTTL
	}
	return &AuthService{users: users, rl: rl, sessions: sessions, cfg: cfg}
}

// normalizeUsername 仅裁剪首尾空白，保留用户原始大小写。
// 入库时保留原始大小写；重复检测由 repository 在 SQL 中用 lower() 将两侧统一小写，详见 ExistsByUsername。
func normalizeUsername(s string) string {
	return strings.TrimSpace(s)
}

// validateUsername 校验用户名：长度与字符集（大小写均允许，重复检测时再统一小写归一化）。
func validateUsername(s string) bool {
	n := len([]rune(s))
	if n < usernameMinLen || n > usernameMaxLen {
		return false
	}
	return usernameRegexp.MatchString(s)
}

// sessionTokenHash 计算会话 Token 的存储哈希：对「客户端持有的原始 Token 字符串」做 SHA-256，
// 返回 64 字符小写十六进制，对齐 web_sessions.token_hash VARCHAR(64)。
//
// 约定：签发（generateToken）与校验（Logout / QQBindService.resolveUserID）**必须**共用本函数。
// 历史缺陷：签发时对 32 字节随机原文求哈希，校验时却对十六进制字符串求哈希，
// 两者输入不同（sha256(b) vs sha256(hex(b))），导致所有已登录会话鉴权永远失败。
func sessionTokenHash(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// generateToken 生成会话原始 Token（crypto/rand 32 字节十六进制）及其 SHA-256 哈希（64 字符十六进制）。
// 原始 Token 仅一次性返回给客户端（经 HttpOnly Cookie），哈希存入 web_sessions.token_hash。
func generateToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("生成会话 Token 失败: %w", err)
	}
	raw = hex.EncodeToString(b)
	// 关键：哈希对象必须是「下发给客户端的 raw 字符串」，而不是原始随机字节 b，
	// 否则与校验侧（对 Cookie 值求哈希）不是同一个输入，永远匹配不上。
	hash = sessionTokenHash(raw)
	return raw, hash, nil
}

// CheckUsername 实现 BIZ-002：限流 → 校验 → 查重（SQL 层统一小写，大小写不敏感）。
// 返回 exists=true 表示用户名已被占用。
func (s *AuthService) CheckUsername(ctx context.Context, ip, rawUsername string) (bool, error) {
	username := normalizeUsername(rawUsername)
	if !validateUsername(username) {
		return false, ErrInvalidParam
	}

	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.AuthRateLimitKey("check_name", ip), s.cfg.RateLimitWindow, s.cfg.CheckNameMax)
	if rlErr != nil {
		// fail-open：仅记录，不阻断（具体记录由 transport 层做）。
		_ = rlErr
	}
	if !allowed {
		return false, ErrRateLimited
	}

	exists, err := s.users.ExistsByUsername(ctx, username)
	if err != nil {
		return false, Wrap(ErrInternal, err, "查重失败")
	}
	return exists, nil
}

// Register 实现 BIZ-003：限流 → 校验 → 查重 → bcrypt 哈希 → 入库（保留原始大小写、唯一约束兜底）。
// 应用层预检用于返回友好 409；数据库 ux_users_username_lower 唯一约束作为并发兜底。
func (s *AuthService) Register(ctx context.Context, ip, rawUsername, password string) error {
	username := normalizeUsername(rawUsername)
	if !validateUsername(username) {
		return ErrInvalidParam
	}
	if len(password) < passwordMinLen {
		return ErrInvalidParam
	}

	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.AuthRateLimitKey("register", ip), s.cfg.RateLimitWindow, s.cfg.RegisterMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return ErrRateLimited
	}

	// 应用层预检：用户名已存在直接返回友好错误。
	exists, err := s.users.ExistsByUsername(ctx, username)
	if err != nil {
		return Wrap(ErrInternal, err, "查重失败")
	}
	if exists {
		return ErrUsernameTaken
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return ErrInternal
	}

	if err := s.users.CreateUser(ctx, username, string(hash)); err != nil {
		if errors.Is(err, repository.ErrUsernameTaken) {
			return ErrUsernameTaken
		}
		return Wrap(ErrInternal, err, "写入用户失败")
	}
	return nil
}

// Login 实现 BIZ-004：双重限流 → 校验 → 查用户 → bcrypt 比对 → 状态校验 → 建会话并签发 Token。
// 成功返回原始 Token（仅此一次，调用方负责下发）与会话过期时间；凭证类失败统一返回 ErrInvalidCredentials（防枚举）。
func (s *AuthService) Login(ctx context.Context, ip, rawUsername, password, deviceName string) (rawToken string, expiresAt time.Time, err error) {
	username := normalizeUsername(rawUsername)
	if !validateUsername(username) || len(password) < passwordMinLen {
		return "", time.Time{}, ErrInvalidParam
	}

	// 双重限流：每 IP 与每 username 各计一份，任一命中即拒绝（HTTP 接口文档 §6）。
	ipKey := repository.AuthRateLimitKey("login", ip)
	userKey := repository.AuthRateLimitKey("login", username)
	for _, key := range []string{ipKey, userKey} {
		allowed, rlErr := s.rl.AllowAttempt(ctx, key, s.cfg.RateLimitWindow, s.cfg.LoginMax)
		if rlErr != nil {
			_ = rlErr // fail-open
		}
		if !allowed {
			return "", time.Time{}, ErrRateLimited
		}
	}

	rec, gErr := s.users.GetByUsername(ctx, username)
	if gErr != nil {
		if errors.Is(gErr, repository.ErrUserNotFound) {
			// 用户名不存在：与密码错误统一处理，不暴露账号是否存在。
			return "", time.Time{}, ErrInvalidCredentials
		}
		return "", time.Time{}, Wrap(ErrInternal, gErr, "查询用户失败")
	}
	if rec == nil {
		// 库未返回用户且未报错（异常态），与未找到统一处理。
		return "", time.Time{}, ErrInvalidCredentials
	}

	if bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)) != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}

	if rec.Status != 1 {
		return "", time.Time{}, ErrAccountDisabled
	}

	raw, hash, gErr := generateToken()
	if gErr != nil {
		return "", time.Time{}, Wrap(ErrInternal, gErr, "生成会话 Token 失败")
	}

	expiresAt = time.Now().UTC().Add(s.cfg.SessionTTL)
	if cErr := s.sessions.CreateSession(ctx, rec.ID, hash, deviceName, expiresAt); cErr != nil {
		return "", time.Time{}, Wrap(ErrInternal, cErr, "写入会话失败")
	}
	return raw, expiresAt, nil
}

// Logout 实现 BIZ-005：按原始 Token 撤销当前会话（软删除 revoked_at）。
// 原始 Token 不进库，由调用方从 HttpOnly Cookie 取出后传入；本方法内部做 SHA-256，
// 与 Login 写入时的哈希逻辑同源（见 generateToken）。
// 无有效会话（Token 缺失 / 已撤销 / 已过期）返回 ErrNotAuthenticated；数据库错误返回 ErrInternal。
func (s *AuthService) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return ErrNotAuthenticated
	}
	// 与 generateToken 同源：对原始 Token 字符串求哈希（见 sessionTokenHash）。
	hash := sessionTokenHash(rawToken)
	affected, err := s.sessions.RevokeSession(ctx, hash)
	if err != nil {
		return Wrap(ErrInternal, err, "撤销会话失败")
	}
	if !affected {
		return ErrNotAuthenticated
	}
	return nil
}
