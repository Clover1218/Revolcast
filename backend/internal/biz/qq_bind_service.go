package biz

import (
	"context"
	"errors"
	"strconv"
	"time"

	"revolcast-backend/internal/repository"
)

const (
	// bindQQRateLimitWindow 绑定 QQ 的限流窗口（HTTP 接口文档 §7）。
	bindQQRateLimitWindow = 5 * time.Minute
	// bindQQRateLimitMax 绑定 QQ 每用户窗口内最大尝试次数。
	bindQQRateLimitMax = 5
	// unbindQQRateLimitWindow 解绑 QQ 的限流窗口（HTTP 接口文档 §8）。
	unbindQQRateLimitWindow = 5 * time.Minute
	// unbindQQRateLimitMax 解绑 QQ 每用户窗口内最大尝试次数。
	unbindQQRateLimitMax = 5
	// bindCodeLen 绑定码长度（数据库表设计文档 §3.3：char_length(code)=8）。
	bindCodeLen = 8
)

// bindSessionResolver 是 QQBindService 对会话解析的依赖（便于测试注入 fake）。
type bindSessionResolver interface {
	ResolveSession(ctx context.Context, tokenHash string) (userID int64, ok bool, err error)
}

// bindUserStatusStore 是 QQBindService 对账号状态查询的依赖（便于测试注入 fake）。
type bindUserStatusStore interface {
	GetByID(ctx context.Context, id int64) (*repository.UserRecord, error)
}

// qqBindStore 是 QQBindService 对「消费绑定码 + 写入绑定」事务的依赖（便于测试注入 fake）。
type qqBindStore interface {
	ConsumeBindCodeAndBind(ctx context.Context, code string, userID int64) (qqUIN int64, botID string, err error)
}

// qqUnbindStore 是 QQBindService 对「软解绑当前有效绑定」的依赖（便于测试注入 fake）。
type qqUnbindStore interface {
	UnbindByUser(ctx context.Context, userID int64) (qqUIN int64, botID string, err error)
}

// qqBindingStore 组合绑定的读写能力；由 repository.QQBindRepo 同时实现。
type qqBindingStore interface {
	qqBindStore
	qqUnbindStore
}

// bindRateLimiter 是 QQBindService 对限流的依赖（便于测试注入 fake）。
type bindRateLimiter interface {
	AllowAttempt(ctx context.Context, key string, window time.Duration, max int64) (bool, error)
}

// QQBindService 实现 BIZ-006（网站赎回 QQ 绑定码）与 BIZ-007（网站解绑 QQ）。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type QQBindService struct {
	sessions bindSessionResolver
	users    bindUserStatusStore
	qqBind   qqBindingStore
	rl       bindRateLimiter
}

func NewQQBindService(sessions bindSessionResolver, users bindUserStatusStore, qqBind qqBindingStore, rl bindRateLimiter) *QQBindService {
	return &QQBindService{sessions: sessions, users: users, qqBind: qqBind, rl: rl}
}

// resolveUserID 将会话原始 Token 解析为 user_id（并顺带刷新 web_sessions.last_seen_at）。
// 原始 Token 为 HttpOnly Cookie 中的值；哈希统一走 sessionTokenHash（见 biz/auth.go），
// 与登录签发时的计算方法完全同源，避免「签发/校验输入不同」导致永不匹配。
// 供 BindQQ / UnbindQQ 共用，避免两处重复哈希与会话解析代码。
func (s *QQBindService) resolveUserID(ctx context.Context, rawToken string) (int64, error) {
	tokenHash := sessionTokenHash(rawToken)
	userID, ok, err := s.sessions.ResolveSession(ctx, tokenHash)
	if err != nil {
		return 0, Wrap(ErrInternal, err, "解析会话失败")
	}
	if !ok {
		return 0, ErrNotAuthenticated
	}
	return userID, nil
}

// BindQQ 完成一次 QQ 绑定赎回：校验会话 → 校验账号状态 → 校验参数 → 限流 → 原子消费并绑定。
// 入参 rawToken 为 HttpOnly Cookie 中的原始会话 Token（由 transport 层从 Cookie 取出后传入）；
// 内部做 SHA-256 与 ResolveSession 同源（见 biz/auth.go 的 Login/Logout）。
// 绑定成功返回 nil；各类失败返回对应业务错误，供 transport 层映射为协议状态码。
func (s *QQBindService) BindQQ(ctx context.Context, rawToken, bindCode string) error {
	// 1. 参数校验（廉价，先于任何 DB 访问）。
	if len(bindCode) != bindCodeLen {
		return ErrInvalidParam
	}

	// 2. 解析会话：原始 Token → SHA-256 → user_id，并刷新 last_seen_at。
	userID, err := s.resolveUserID(ctx, rawToken)
	if err != nil {
		return err
	}

	// 3. 账号状态校验：禁用账号（status != 1）禁止发起绑定（数据库文档 §1.1 规则 6）。
	rec, uErr := s.users.GetByID(ctx, userID)
	if uErr != nil {
		if errors.Is(uErr, repository.ErrUserNotFound) {
			// 会话指向的账号不存在：视为未认证（保留根因便于排障）。
			return Wrap(ErrNotAuthenticated, uErr, "会话指向账号不存在")
		}
		return Wrap(ErrInternal, uErr, "查询用户失败")
	}
	if rec.Status != 1 {
		return ErrAccountDisabled
	}

	// 4. 限流：按登录用户维度，5 分钟 5 次（fail-open）。
	rlKey := repository.UserRateLimitKey("bind_qq", strconv.FormatInt(userID, 10))
	allowed, rlErr := s.rl.AllowAttempt(ctx, rlKey, bindQQRateLimitWindow, bindQQRateLimitMax)
	if rlErr != nil {
		_ = rlErr // fail-open：仅记录，不阻断（具体记录由 transport 层做）。
	}
	if !allowed {
		return ErrRateLimited
	}

	// 5. 原子消费绑定码并写入绑定关系；仓库层哨兵错误映射为业务错误（携带根因）。
	if _, _, cErr := s.qqBind.ConsumeBindCodeAndBind(ctx, bindCode, userID); cErr != nil {
		switch {
		case errors.Is(cErr, repository.ErrBindCodeInvalid):
			return Wrap(ErrBindCodeInvalid, cErr, "消费绑定码失败")
		case errors.Is(cErr, repository.ErrUserAlreadyBound):
			return Wrap(ErrUserAlreadyBound, cErr, "消费绑定码失败")
		case errors.Is(cErr, repository.ErrQQAlreadyBound):
			return Wrap(ErrQQAlreadyBound, cErr, "消费绑定码失败")
		default:
			return Wrap(ErrInternal, cErr, "消费绑定码失败")
		}
	}
	return nil
}

// UnbindQQ 解绑当前登录账号的 QQ 绑定（BIZ-007）。
//
// 接口只校验会话（HTTP 接口文档 §8）：无请求参数、无绑定码；
// 不校验 users.status —— 解绑是释放资源、降低风险的操作，禁用账号也应允许解除绑定；
// 也不做绑定明细预查 —— 由单条条件 UPDATE 的受影响行数给出权威结论（0 行 → ErrNotBound）。
// 解绑成功后不发 QQ 私聊通知（不依赖 Adapter 在线），前端自行刷新绑定状态。
func (s *QQBindService) UnbindQQ(ctx context.Context, rawToken string) error {
	// 1. 解析会话：原始 Token → SHA-256 → user_id，并刷新 last_seen_at。
	userID, err := s.resolveUserID(ctx, rawToken)
	if err != nil {
		return err
	}

	// 2. 限流：按登录用户维度，5 分钟 5 次（fail-open：Redis 故障不阻断解绑）。
	rlKey := repository.UserRateLimitKey("unbind_qq", strconv.FormatInt(userID, 10))
	allowed, rlErr := s.rl.AllowAttempt(ctx, rlKey, unbindQQRateLimitWindow, unbindQQRateLimitMax)
	if rlErr != nil {
		_ = rlErr // fail-open：仅记录，不阻断（具体记录由 transport 层做）。
	}
	if !allowed {
		return ErrRateLimited
	}

	// 3. 软解绑：status 1→0 并记录 unbound_at（保留历史行用于审计）。
	if _, _, uErr := s.qqBind.UnbindByUser(ctx, userID); uErr != nil {
		if errors.Is(uErr, repository.ErrNotBound) {
			return Wrap(ErrNotBound, uErr, "解绑查询无有效绑定")
		}
		return Wrap(ErrInternal, uErr, "解绑失败")
	}
	return nil
}
