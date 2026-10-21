package biz

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"revolcast-backend/internal/repository"
)

const (
	authRateLimitWindow = 5 * time.Minute // 限流窗口（HTTP 接口文档 §5）
	authCheckNameMax    = 10              // check-name 每 IP 窗口内上限
	authRegisterMax     = 5               // register 每 IP 窗口内上限
	bcryptCost          = 12              // 密码哈希成本（数据库文档 §3.1）
	usernameMinLen      = 3
	usernameMaxLen      = 64 // 对齐 VARCHAR(64)
	passwordMinLen      = 8
	usernamePattern     = `^[a-zA-Z0-9_]+$`
)

var usernameRegexp = regexp.MustCompile(usernamePattern)

// UserStore 是 AuthService 对用户的存储依赖（便于测试注入 fake）。
type UserStore interface {
	ExistsByUsername(ctx context.Context, username string) (bool, error)
	CreateUser(ctx context.Context, username, passwordHash string) error
}

// RateLimiter 是 AuthService 对限流的依赖（便于测试注入 fake）。
type RateLimiter interface {
	AllowAttempt(ctx context.Context, key string, window time.Duration, max int64) (bool, error)
}

// AuthConfig 鉴权业务 tunable 参数。
type AuthConfig struct {
	RateLimitWindow time.Duration // 限流窗口
	CheckNameMax    int64         // check-name 窗口内最大次数
	RegisterMax     int64         // register 窗口内最大次数
}

// AuthService 实现 BIZ-002（校验用户名可用性）/ BIZ-003（注册网站账号）。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type AuthService struct {
	users UserStore
	rl    RateLimiter
	cfg   AuthConfig
}

func NewAuthService(users UserStore, rl RateLimiter, cfg AuthConfig) *AuthService {
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = authRateLimitWindow
	}
	if cfg.CheckNameMax <= 0 {
		cfg.CheckNameMax = authCheckNameMax
	}
	if cfg.RegisterMax <= 0 {
		cfg.RegisterMax = authRegisterMax
	}
	return &AuthService{users: users, rl: rl, cfg: cfg}
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
		return false, ErrInternal
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
		return ErrInternal
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
		return ErrInternal
	}
	return nil
}
