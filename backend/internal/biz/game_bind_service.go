package biz

import (
	"context"
	"errors"
	"strings"
	"time"

	"revolcast-backend/internal/repository"
)

// 游戏角色绑定码（BIZ-009）的 tunable 默认值。
const (
	// gameBindCodeTTL 绑定码有效期。《数据库表设计文档》§3.6「建议创建后 10 分钟」。
	// 与 QQ 绑定码（qq_bind_codes，5 分钟）不同属有意差异：游戏角色绑定需要用户切到游戏内操作。
	gameBindCodeTTL = 10 * time.Minute
	// gameBindRateLimitWindow / Max 发码限流窗口与上限，维度为 (qq_uin + server_code)。
	gameBindRateLimitWindow = 5 * time.Minute
	gameBindRateLimitMax    = 5
)

// GameBindOutcomeKind 标识 BIZ-009 的三种「正常结束」分支。
// 这些分支文案需要携带角色名 / 服务器名等上下文，因此用结果对象表达，而不是 error。
type GameBindOutcomeKind string

const (
	// GameBindOutcomeCode 已（新生成或幂等复用）一个待使用绑定码。
	GameBindOutcomeCode GameBindOutcomeKind = "code"
	// GameBindOutcomeAlreadyBound 该账号在该服务器已有有效角色绑定，不生成新码。
	GameBindOutcomeAlreadyBound GameBindOutcomeKind = "already_bound"
	// GameBindOutcomeQQNotBound 该 QQ 尚未绑定网站账号，无法确定 user_id。
	GameBindOutcomeQQNotBound GameBindOutcomeKind = "qq_not_bound"
)

// GameBindOutcome 是 BIZ-009 的业务结论，由 transport 层渲染为用户文案。
type GameBindOutcome struct {
	Kind      GameBindOutcomeKind
	Server    AvailableServer // 目标服务器（用于回显名称 / 代号）
	Code      string          // Kind=GameBindOutcomeCode 时有效
	ExpiresAt time.Time       // Kind=GameBindOutcomeCode 时有效
	GameName  string          // Kind=GameBindOutcomeAlreadyBound 时有效
}

// availableServerSource 是 GameBindService 对服务器查询的依赖（便于测试注入 fake）。
// 列表（可滞后）与单条校验（权威）刻意分开，避免用缓存结果做发码判定。
type availableServerSource interface {
	ListAvailable(ctx context.Context) ([]AvailableServer, error)
	ResolveSubscribableServer(ctx context.Context, code string) (*repository.GameServerRecord, error)
}

// qqActiveBindingReader 是 GameBindService 对「按 QQ 号取有效网站账号」的依赖。
type qqActiveBindingReader interface {
	GetActiveQQBinding(ctx context.Context, qqUIN int64) (*repository.QQBindingInfo, error)
}

// serverRoleBindingReader 是 GameBindService 对「账号在某服务器是否已有角色」的依赖。
type serverRoleBindingReader interface {
	FindActive(ctx context.Context, serverID, userID int64) (*repository.ServerGameBindingRecord, error)
}

// gameBindCodeStore 是 GameBindService 对 game_bind_codes 读写的依赖。
type gameBindCodeStore interface {
	GetPendingGameBindCode(ctx context.Context, serverID, userID int64) (code string, expiresAt time.Time, found bool, err error)
	CreateGameBindCode(ctx context.Context, serverID, userID, qqUIN int64, traceID string, expiresAt time.Time) (string, error)
}

// GameBindConfig 游戏角色绑定业务 tunable 参数。
type GameBindConfig struct {
	CodeTTL         time.Duration
	RateLimitWindow time.Duration
	RateLimitMax    int64
}

// GameBindService 实现 BIZ-008（列出可用服务器）与 BIZ-009（生成游戏角色绑定码）。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type GameBindService struct {
	servers availableServerSource
	users   bindUserStatusStore
	qqBind  qqActiveBindingReader
	roles   serverRoleBindingReader
	codes   gameBindCodeStore
	rl      bindRateLimiter
	cfg     GameBindConfig
}

func NewGameBindService(
	servers availableServerSource,
	users bindUserStatusStore,
	qqBind qqActiveBindingReader,
	roles serverRoleBindingReader,
	codes gameBindCodeStore,
	rl bindRateLimiter,
	cfg GameBindConfig,
) *GameBindService {
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = gameBindCodeTTL
	}
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = gameBindRateLimitWindow
	}
	if cfg.RateLimitMax <= 0 {
		cfg.RateLimitMax = gameBindRateLimitMax
	}
	return &GameBindService{
		servers: servers,
		users:   users,
		qqBind:  qqBind,
		roles:   roles,
		codes:   codes,
		rl:      rl,
		cfg:     cfg,
	}
}

// ListAvailableServers 返回「可用服务器」列表（status=1），走 GameServerService 的 5 分钟缓存。
// 供 `revolcast server list` 使用；展示层的条数截断由 transport 负责。
func (s *GameBindService) ListAvailableServers(ctx context.Context) ([]AvailableServer, error) {
	return s.servers.ListAvailable(ctx)
}

// GenerateBindCode 实现 BIZ-009：为「该 QQ 已绑定的网站账号」生成指定服务器的游戏角色绑定码。
//
// 执行顺序（顺序本身是设计的一部分）：
//  1. 服务器校验 —— 走 ResolveSubscribableServer **直查数据库**，不吃列表缓存，
//     避免管理员在缓存窗口内停用服务器后仍能为其发码；
//  2. QQ → 网站账号 —— 无有效 qq_bindings 时返回 GameBindOutcomeQQNotBound；
//  3. 账号状态 —— users.status != 1 拒绝（与 BIZ-006 口径一致）；
//  4. 角色绑定检查 —— 已有 server_game_bindings(status=1) 时返回 GameBindOutcomeAlreadyBound；
//  5. 限流 —— 放在所有只读幂等校验之后：已绑定/未绑 QQ 等确定性答案不会被限流挡住，
//     限流只拦截真正会发码的请求；
//  6. 幂等取/建 —— 已有未过期待使用码直接复用，否则回收过期槽位后生成新码。
//
// 返回 error 仅用于真失败：ErrServerNotFound / ErrAccountDisabled / ErrRateLimited / ErrInternal。
func (s *GameBindService) GenerateBindCode(ctx context.Context, qqUIN int64, serverCode, traceID string) (*GameBindOutcome, error) {
	// 1. 服务器校验：code 统一小写（库内 code 约束为小写字母/数字/连字符）。
	code := strings.ToLower(strings.TrimSpace(serverCode))
	rec, err := s.servers.ResolveSubscribableServer(ctx, code)
	if err != nil {
		switch {
		case errors.Is(err, ErrServerCodeInvalid),
			errors.Is(err, ErrServerNotFound),
			errors.Is(err, ErrServerDisabled):
			// 格式非法 / 不存在 / 已停用对外统一为「找不到可用服务器」，避免枚举已停用服务器。
			return nil, Wrap(ErrServerNotFound, err, "解析目标游戏服务器失败")
		default:
			// 已是 ErrInternal（含根因链），直接上抛。
			return nil, err
		}
	}
	server := AvailableServer{Code: rec.Code, Name: rec.Name}
	if rec.Description != nil {
		server.Description = *rec.Description
	}

	// 2. QQ → 网站账号。
	binding, bErr := s.qqBind.GetActiveQQBinding(ctx, qqUIN)
	if bErr != nil {
		return nil, Wrap(ErrInternal, bErr, "查询 QQ 绑定失败")
	}
	if binding == nil {
		return &GameBindOutcome{Kind: GameBindOutcomeQQNotBound, Server: server}, nil
	}

	// 3. 账号状态：禁用账号不允许发码。
	user, uErr := s.users.GetByID(ctx, binding.UserID)
	if uErr != nil {
		if errors.Is(uErr, repository.ErrUserNotFound) {
			// 绑定指向已消失的账号：数据不一致，按「未绑定」回复（并保留根因供日志）。
			return &GameBindOutcome{Kind: GameBindOutcomeQQNotBound, Server: server}, nil
		}
		return nil, Wrap(ErrInternal, uErr, "查询用户失败")
	}
	if user.Status != 1 {
		return nil, ErrAccountDisabled
	}

	// 4. 已有有效角色绑定 → 不生成新码。
	role, rErr := s.roles.FindActive(ctx, rec.ID, binding.UserID)
	if rErr != nil {
		return nil, Wrap(ErrInternal, rErr, "查询服务器角色绑定失败")
	}
	if role != nil {
		return &GameBindOutcome{
			Kind:     GameBindOutcomeAlreadyBound,
			Server:   server,
			GameName: role.GameName,
		}, nil
	}

	// 5. 限流（维度：qq_uin + server_code）。
	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.GameBindRateLimitKey(qqUIN, rec.Code), s.cfg.RateLimitWindow, s.cfg.RateLimitMax)
	if rlErr != nil {
		_ = rlErr // fail-open：仅记录，不阻断（具体记录由 transport 层做）。
	}
	if !allowed {
		return nil, Wrap(ErrRateLimited, nil, "生成游戏角色绑定码被限流")
	}

	// 6. 幂等取/建：未过期待使用码优先复用，避免用户重复请求产生多个码。
	if existing, exp, found, pErr := s.codes.GetPendingGameBindCode(ctx, rec.ID, binding.UserID); pErr != nil {
		return nil, Wrap(ErrInternal, pErr, "查询待使用游戏角色绑定码失败")
	} else if found {
		return &GameBindOutcome{Kind: GameBindOutcomeCode, Server: server, Code: existing, ExpiresAt: exp}, nil
	}

	expiresAt := time.Now().UTC().Add(s.cfg.CodeTTL)
	created, cErr := s.codes.CreateGameBindCode(ctx, rec.ID, binding.UserID, qqUIN, traceID, expiresAt)
	if cErr != nil {
		return nil, Wrap(ErrInternal, cErr, "写入游戏角色绑定码失败")
	}
	return &GameBindOutcome{Kind: GameBindOutcomeCode, Server: server, Code: created, ExpiresAt: expiresAt}, nil
}
