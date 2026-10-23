package biz

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"revolcast-backend/internal/protocol"
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

// 游戏内兑换绑定码（BIZ-010）的 tunable 默认值。
const (
	// gameRedeemRateLimitWindow / Max 游戏内绑定的限流窗口与上限，维度为 (server_id + player_uuid)。
	// 位置在「码格式校验之后、任何 DB 查询之前」：这里是猜码场景，每次尝试都必须计数。
	gameRedeemRateLimitWindow = 5 * time.Minute
	gameRedeemRateLimitMax    = 5

	// gameBindCodeLen 绑定码长度（与 game_bind_codes.code 的 CHECK 一致）。
	gameBindCodeLen = 8

	// gameNameMaxLen server_game_bindings.game_name 的列长度上限（VARCHAR(64)）。
	gameNameMaxLen = 64
)

// gameBindCodeCharset 绑定码字符集，与 repository 的生成器保持一致（排除易混淆字符 0/O/1/I/L）。
const gameBindCodeCharset = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// gameUUIDRegexp 校验 Minecraft 角色 UUID 的标准 8-4-4-4-12 形式。
// 不引入 uuid 依赖：只需格式校验，值最终由 PostgreSQL uuid 类型再做一次强校验。
var gameUUIDRegexp = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeGameBindCode 归一化用户输入的绑定码：去空白 + 转大写，并校验长度与字符集。
// 不合法时 ok=false，调用方应直接按「绑定码无效」处理（不查库、不泄露更多信息）。
//
// 这是游戏内绑定码的唯一归一化入口，避免 transport 与 biz 各写一套导致行为不一致。
func NormalizeGameBindCode(raw string) (code string, ok bool) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	if len(v) != gameBindCodeLen {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		if !strings.ContainsRune(gameBindCodeCharset, rune(v[i])) {
			return "", false
		}
	}
	return v, true
}

// NormalizeGameUUID 校验并归一化角色 UUID（统一小写）。
// 空串或格式非法时返回空串，由调用方按「无法识别角色」处理。
func NormalizeGameUUID(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if !gameUUIDRegexp.MatchString(v) {
		return ""
	}
	return v
}

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
	// ResolveServer 只校验「格式合法 + 存在」，不判 status：解绑对已存在关系生效，
	// 不应因为服务器被停用而无法执行。
	ResolveServer(ctx context.Context, code string) (*repository.GameServerRecord, error)
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

// gameRoleBindingWriter 是 GameBindService 对 server_game_bindings 写入的依赖：
//   - RedeemGameBindCode 在单事务内消费绑定码并创建角色绑定（BIZ-010）；
//   - UnbindActive 软解绑当前有效角色（BIZ-011）。
type gameRoleBindingWriter interface {
	RedeemGameBindCode(ctx context.Context, in repository.GameRoleBindInput) (*repository.GameRoleBindResult, error)
	UnbindActive(ctx context.Context, serverID, userID int64) (*repository.ServerGameBindingRecord, error)
}

// GameBindConfig 游戏角色绑定业务 tunable 参数。
type GameBindConfig struct {
	CodeTTL         time.Duration
	RateLimitWindow time.Duration
	RateLimitMax    int64

	// RedeemRateLimitWindow / Max 游戏内兑换绑定码的限流窗口与上限（默认 5 分钟 / 5 次）。
	RedeemRateLimitWindow time.Duration
	RedeemRateLimitMax    int64
}

// GameBindService 实现 BIZ-008（列出可用服务器）与 BIZ-009（生成游戏角色绑定码）。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type GameBindService struct {
	servers availableServerSource
	users   bindUserStatusStore
	qqBind  qqActiveBindingReader
	roles   serverRoleBindingReader
	codes   gameBindCodeStore
	writer  gameRoleBindingWriter
	rl      bindRateLimiter
	cfg     GameBindConfig
}

func NewGameBindService(
	servers availableServerSource,
	users bindUserStatusStore,
	qqBind qqActiveBindingReader,
	roles serverRoleBindingReader,
	codes gameBindCodeStore,
	writer gameRoleBindingWriter,
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
	if cfg.RedeemRateLimitWindow <= 0 {
		cfg.RedeemRateLimitWindow = gameRedeemRateLimitWindow
	}
	if cfg.RedeemRateLimitMax <= 0 {
		cfg.RedeemRateLimitMax = gameRedeemRateLimitMax
	}
	return &GameBindService{
		servers: servers,
		users:   users,
		qqBind:  qqBind,
		roles:   roles,
		codes:   codes,
		writer:  writer,
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

// ---- BIZ-010：游戏内绑定角色（消费绑定码） ----

// GameRoleBindOutcomeKind 标识 BIZ-010 的业务结论。
type GameRoleBindOutcomeKind string

const (
	// GameRoleBindOutcomeBound 绑定成功。
	GameRoleBindOutcomeBound GameRoleBindOutcomeKind = "bound"
	// GameRoleBindOutcomeIdempotent 同一码 + 同一角色重复提交，视为已成功（不重复写库）。
	GameRoleBindOutcomeIdempotent GameRoleBindOutcomeKind = "idempotent"
	// GameRoleBindOutcomeCodeInvalid 码不存在 / 已过期 / 已被他人使用 / 不属于本服务器 / 格式非法。
	GameRoleBindOutcomeCodeInvalid GameRoleBindOutcomeKind = "code_invalid"
	// GameRoleBindOutcomeAccountBound 该账号在该服务器已有有效角色绑定。
	GameRoleBindOutcomeAccountBound GameRoleBindOutcomeKind = "account_bound"
	// GameRoleBindOutcomeRoleTaken 该角色已被其他账号占用。
	GameRoleBindOutcomeRoleTaken GameRoleBindOutcomeKind = "role_taken"
	// GameRoleBindOutcomeServerUnavailable 目标服务器已停用。
	GameRoleBindOutcomeServerUnavailable GameRoleBindOutcomeKind = "server_unavailable"
	// GameRoleBindOutcomePlayerInvalid 玩家 UUID / 昵称缺失或格式非法。
	GameRoleBindOutcomePlayerInvalid GameRoleBindOutcomeKind = "player_invalid"
)

// GameRoleBindOutcome 是 BIZ-010 的业务结论，由 transport 渲染为游戏内私聊文案。
type GameRoleBindOutcome struct {
	Kind       GameRoleBindOutcomeKind
	GameName   string // 绑定成功 / 幂等 / 已绑定 时的角色名
	ServerCode string // 目标服务器代号（用于提示解绑指令）
	ServerName string // 目标服务器名称
}

// BindGameRole 实现 BIZ-010：用游戏内输入的绑定码，把玩家角色绑定到码所属的网站账号。
//
// 执行顺序（顺序本身是设计的一部分）：
//  1. 码格式校验 —— 长度 / 字符集不合法直接判无效，**不查库**，避免用畸形输入打库；
//  2. 玩家身份校验 —— 角色 UUID 与昵称必须来自事件体（由已鉴权服务器上报）；
//  3. 限流（前置） —— 维度 (server_id + player_uuid)。与 BIZ-009 的「限流后置」相反：
//     发码是产生资源，只读幂等答案不该被限流挡住；兑换是**猜码**，每次尝试都必须计数；
//  4. 赎回事务 —— 全部一致性判定与写入在同一个事务内完成（见 repository.RedeemGameBindCode）；
//  5. 结论映射 —— 正常分支走 outcome，只有真失败才返回 error。
//
// 返回 error 仅用于真失败：ErrRateLimited / ErrAccountDisabled / ErrInternal。
func (s *GameBindService) BindGameRole(ctx context.Context, serverID int64, player protocol.QueqiaoPlayer, rawCode, traceID string) (*GameRoleBindOutcome, error) {
	// 1. 码格式：不合法时既不查库也不计数（无法构成有效猜码尝试，且避免畸形输入消耗配额）。
	code, ok := NormalizeGameBindCode(rawCode)
	if !ok {
		return &GameRoleBindOutcome{Kind: GameRoleBindOutcomeCodeInvalid}, nil
	}

	// 2. 玩家身份：UUID 是绑定的身份主键，昵称是展示名，两者都必须可用。
	gameUUID := NormalizeGameUUID(player.UUID)
	gameName := strings.TrimSpace(player.Nickname)
	if gameUUID == "" || gameName == "" || len([]rune(gameName)) > gameNameMaxLen {
		return &GameRoleBindOutcome{Kind: GameRoleBindOutcomePlayerInvalid}, nil
	}

	// 3. 限流（前置，见上方注释）。
	allowed, rlErr := s.rl.AllowAttempt(ctx,
		repository.GameRedeemRateLimitKey(serverID, gameUUID),
		s.cfg.RedeemRateLimitWindow, s.cfg.RedeemRateLimitMax)
	if rlErr != nil {
		_ = rlErr // fail-open：仅记录，不阻断（具体记录由 transport 层做）。
	}
	if !allowed {
		return nil, Wrap(ErrRateLimited, nil, "游戏内绑定角色被限流")
	}

	// 4. 赎回事务。
	res, rErr := s.writer.RedeemGameBindCode(ctx, repository.GameRoleBindInput{
		Code:     code,
		ServerID: serverID,
		GameName: gameName,
		GameUUID: gameUUID,
	})
	if rErr != nil {
		return nil, Wrap(ErrInternal, rErr, "兑换游戏角色绑定码失败")
	}

	// 5. 结论映射。
	out := &GameRoleBindOutcome{
		GameName:   res.GameName,
		ServerCode: res.ServerCode,
		ServerName: res.ServerName,
	}
	switch res.Outcome {
	case repository.GameRoleBindOK:
		out.Kind = GameRoleBindOutcomeBound
		if out.GameName == "" {
			out.GameName = gameName
		}
	case repository.GameRoleBindAlreadyBound:
		out.Kind = GameRoleBindOutcomeIdempotent
		if out.GameName == "" {
			out.GameName = gameName
		}
	case repository.GameRoleBindCodeInvalid:
		out.Kind = GameRoleBindOutcomeCodeInvalid
	case repository.GameRoleBindAccountBound:
		out.Kind = GameRoleBindOutcomeAccountBound
	case repository.GameRoleBindRoleTaken:
		out.Kind = GameRoleBindOutcomeRoleTaken
	case repository.GameRoleBindServerUnavailable:
		out.Kind = GameRoleBindOutcomeServerUnavailable
	case repository.GameRoleBindAccountDisabled:
		// 账号不可用属于失败而非业务结论：复用既有错误码与文案。
		return nil, Wrap(ErrAccountDisabled, nil, "绑定码所属账号不可用")
	default:
		return nil, Wrap(ErrInternal, nil, fmt.Sprintf("未知的绑定码兑换结论: %s", res.Outcome))
	}
	return out, nil
}

// ---- BIZ-011：解除服务器角色绑定 ----

// GameRoleUnbindOutcomeKind 标识 BIZ-011 的业务结论。
type GameRoleUnbindOutcomeKind string

const (
	// GameRoleUnbindOutcomeDone 已解除绑定。
	GameRoleUnbindOutcomeDone GameRoleUnbindOutcomeKind = "done"
	// GameRoleUnbindOutcomeNotBound 当前没有可解绑的有效绑定。
	GameRoleUnbindOutcomeNotBound GameRoleUnbindOutcomeKind = "not_bound"
	// GameRoleUnbindOutcomeQQNotBound 该 QQ 尚未绑定网站账号。
	GameRoleUnbindOutcomeQQNotBound GameRoleUnbindOutcomeKind = "qq_not_bound"
)

// GameRoleUnbindOutcome 是 BIZ-011 的业务结论。
// 带有角色/服务器快照，供 transport 渲染 QQ 回执与（尽力而为的）游戏内通知。
type GameRoleUnbindOutcome struct {
	Kind       GameRoleUnbindOutcomeKind
	ServerID   int64
	ServerCode string
	ServerName string
	GameName   string
	GameUUID   string
}

// UnbindGameRole 实现 BIZ-011：解除「该 QQ 对应账号」在指定服务器的有效角色绑定。
//
// 设计要点：
//   - 服务器判定用 ResolveServer（不校验 status）：服务器停用后用户仍应能解绑自己的历史绑定；
//   - 解绑是软解绑（status 1→0 + unbound_at），历史行保留（数据库文档 §3.7）；
//   - 不联动修改 game_bind_codes（已使用的码保持 status=1 作为审计）；
//   - 单条 UPDATE 原子，无需事务；并发重复解绑只有一个命中。
//
// 返回 error 仅用于真失败：ErrServerNotFound / ErrInternal。
func (s *GameBindService) UnbindGameRole(ctx context.Context, qqUIN int64, serverCode, traceID string) (*GameRoleUnbindOutcome, error) {
	code := strings.ToLower(strings.TrimSpace(serverCode))
	rec, err := s.servers.ResolveServer(ctx, code)
	if err != nil {
		switch {
		case errors.Is(err, ErrServerNotFound), errors.Is(err, ErrServerCodeInvalid):
			// 格式非法 / 不存在统一为「找不到该服务器」，避免枚举。
			return nil, Wrap(ErrServerNotFound, err, "解析解绑目标服务器失败")
		default:
			return nil, err
		}
	}

	// QQ → 网站账号：解绑必须由绑定了该账号的 QQ 发起，不接受其它身份来源。
	binding, bErr := s.qqBind.GetActiveQQBinding(ctx, qqUIN)
	if bErr != nil {
		return nil, Wrap(ErrInternal, bErr, "查询 QQ 绑定失败")
	}
	if binding == nil {
		return &GameRoleUnbindOutcome{
			Kind:       GameRoleUnbindOutcomeQQNotBound,
			ServerID:   rec.ID,
			ServerCode: rec.Code,
			ServerName: rec.Name,
		}, nil
	}

	removed, uErr := s.writer.UnbindActive(ctx, rec.ID, binding.UserID)
	if uErr != nil {
		return nil, Wrap(ErrInternal, uErr, "解除角色绑定失败")
	}
	if removed == nil {
		return &GameRoleUnbindOutcome{
			Kind:       GameRoleUnbindOutcomeNotBound,
			ServerID:   rec.ID,
			ServerCode: rec.Code,
			ServerName: rec.Name,
		}, nil
	}

	return &GameRoleUnbindOutcome{
		Kind:       GameRoleUnbindOutcomeDone,
		ServerID:   rec.ID,
		ServerCode: rec.Code,
		ServerName: rec.Name,
		GameName:   removed.GameName,
		GameUUID:   removed.GameUUID,
	}, nil
}
