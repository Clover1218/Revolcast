package biz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"revolcast-backend/internal/repository"
)

const (
	serverCodeMaxLen = 64
	serverNameMaxLen = 128

	// serverManageRateLimitWindow / Max 管理类接口（创建/改/删/轮换）限流窗口与上限。
	// 当前管理接口暂不鉴权（角色系统待引入），actorKey 取客户端 IP，作为基础防护。
	serverManageRateLimitWindow = 5 * time.Minute
	serverManageRateLimitMax    = 20

	// serverReadRateLimitWindow / Max 读接口（列表/详情）限流窗口与上限，actorKey 取客户端 IP。
	serverReadRateLimitWindow = 5 * time.Minute
	serverReadRateLimitMax    = 60

	// serverSecretBytes 原始 API Secret 的随机字节数（hex 后为 64 字符）。
	serverSecretBytes = 32

	// serverListCacheTTL 「可用服务器列表」缓存有效期（5 分钟）。
	serverListCacheTTL = 5 * time.Minute
	// serverListLimit 单次从库中拉取的可用服务器上限（整份列表进缓存）。
	serverListLimit = 100
)

// serverCodeRegexp 服务器标识（code）字符集：小写字母、数字、连字符，与数据库 CHECK 约束一致。
var serverCodeRegexp = regexp.MustCompile(`^[a-z0-9-]+$`)

// GameServerStore 是 GameServerService 对 game_servers 存储的依赖（便于测试注入 fake）。
type GameServerStore interface {
	CreateGameServer(ctx context.Context, code, name, description, apiSecretHash string) (*repository.GameServerRecord, error)
	ListGameServers(ctx context.Context, statusFilter *int16, limit, offset int) ([]*repository.GameServerRecord, error)
	GetGameServerByCode(ctx context.Context, code string) (*repository.GameServerRecord, error)
	GetGameServerBySecretHash(ctx context.Context, secretHash string) (*repository.GameServerRecord, error)
	UpdateGameServer(ctx context.Context, code string, u repository.GameServerUpdate) (*repository.GameServerRecord, error)
	DisableGameServer(ctx context.Context, code string) (*repository.GameServerRecord, error)
	RotateGameServerSecret(ctx context.Context, code, newAPISecretHash string) (*repository.GameServerRecord, error)
}

// ServerRateLimiter 是 GameServerService 对限流的依赖（便于测试注入 fake）。
type ServerRateLimiter interface {
	AllowAttempt(ctx context.Context, key string, window time.Duration, max int64) (bool, error)
}

// ServerCache 是 GameServerService 对缓存读写的依赖（便于测试注入 fake）。
// 缓存只承载「可用服务器列表」这一可重建的展示数据，**不作为权威配置**：
// 任何一致性要求高的判定（如发绑定码前校验服务器是否启用）都必须回源数据库。
type ServerCache interface {
	GetCache(ctx context.Context, key string, dest any) (hit bool, err error)
	SetCache(ctx context.Context, key string, val any, ttl time.Duration) error
	DelCache(ctx context.Context, keys ...string) error
}

// AvailableServer 是「可用服务器」（status=1）的对外摘要，供 QQ 指令展示。
// Description 已把 NULL 拍平为空字符串，避免调用方反复判空。
type AvailableServer struct {
	Code        string
	Name        string
	Description string
}

// ServerSecretGenerator 生成 API Secret 原始串及其 SHA-256 哈希（原始串仅一次性下发）。
type ServerSecretGenerator interface {
	GenerateSecret() (raw, hash string, err error)
}

// GameServerConfig 游戏服务器业务 tunable 参数。
type GameServerConfig struct {
	ManageRateLimitWindow time.Duration
	ManageRateLimitMax    int64
	ReadRateLimitWindow   time.Duration
	ReadRateLimitMax      int64
	SecretBytes           int
	// AvailableCacheTTL 「可用服务器列表」缓存有效期，默认 5 分钟。
	AvailableCacheTTL time.Duration
	// AvailableListLimit 单次拉取的可用服务器上限，默认 100（缓存整份列表，展示层再截断）。
	AvailableListLimit int
}

// GameServerService 实现 game_servers 的增删改查与密钥生命周期。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
//
// 鉴权说明：管理接口（创建/更新/禁用/轮换）当前**暂不鉴权**（角色系统待引入）；
// 上线前必须由角色系统接入管理员判定，否则这些接口将完全开放。代码中已用 TODO(role-system) 标注。
type GameServerService struct {
	store  GameServerStore
	rl     ServerRateLimiter
	cache  ServerCache
	secret ServerSecretGenerator
	cfg    GameServerConfig
}

func NewGameServerService(store GameServerStore, rl ServerRateLimiter, cache ServerCache, cfg GameServerConfig) *GameServerService {
	if cfg.ManageRateLimitWindow <= 0 {
		cfg.ManageRateLimitWindow = serverManageRateLimitWindow
	}
	if cfg.ManageRateLimitMax <= 0 {
		cfg.ManageRateLimitMax = serverManageRateLimitMax
	}
	if cfg.ReadRateLimitWindow <= 0 {
		cfg.ReadRateLimitWindow = serverReadRateLimitWindow
	}
	if cfg.ReadRateLimitMax <= 0 {
		cfg.ReadRateLimitMax = serverReadRateLimitMax
	}
	if cfg.SecretBytes <= 0 {
		cfg.SecretBytes = serverSecretBytes
	}
	if cfg.AvailableCacheTTL <= 0 {
		cfg.AvailableCacheTTL = serverListCacheTTL
	}
	if cfg.AvailableListLimit <= 0 {
		cfg.AvailableListLimit = serverListLimit
	}
	return &GameServerService{
		store:  store,
		rl:     rl,
		cache:  cache,
		secret: defaultServerSecretGenerator{bytes: cfg.SecretBytes},
		cfg:    cfg,
	}
}

// validateServerCode 校验服务器标识：非空、长度 1..64、仅小写字母/数字/连字符。
func validateServerCode(code string) bool {
	if len(code) < 1 || len(code) > serverCodeMaxLen {
		return false
	}
	return serverCodeRegexp.MatchString(code)
}

// validateServerName 校验展示名称：非空、长度 1..128。
func validateServerName(name string) bool {
	n := len([]rune(name))
	return n >= 1 && n <= serverNameMaxLen
}

// Create 创建游戏服务器并生成一次性 API Secret。
// actorKey 当前取客户端 IP（管理接口暂未鉴权）；返回记录（不含 secret 哈希明文）与原始 secret（仅此一次）。
// 流程：校验 code/name → 限流（fail-open）→ 生成 secret → 写入（UNIQUE(code) 兜底 code 冲突）。
func (s *GameServerService) Create(ctx context.Context, actorKey, code, name, description string) (*repository.GameServerRecord, string, error) {
	// TODO(role-system): 接入角色系统后，此处须校验调用者为管理员。
	if !validateServerCode(code) {
		return nil, "", ErrServerCodeInvalid
	}
	if !validateServerName(name) {
		return nil, "", ErrServerNameInvalid
	}

	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("manage", actorKey), s.cfg.ManageRateLimitWindow, s.cfg.ManageRateLimitMax)
	if rlErr != nil {
		// fail-open：仅记录，不阻断（具体记录由 transport 层做）。
		_ = rlErr
	}
	if !allowed {
		return nil, "", ErrRateLimited
	}

	raw, hash, gErr := s.secret.GenerateSecret()
	if gErr != nil {
		return nil, "", Wrap(ErrInternal, gErr, "生成服务器密钥失败")
	}

	rec, cErr := s.store.CreateGameServer(ctx, code, name, description, hash)
	if cErr != nil {
		if errors.Is(cErr, repository.ErrServerCodeTaken) {
			return nil, "", Wrap(ErrServerCodeTaken, cErr, "创建游戏服务器失败")
		}
		return nil, "", Wrap(ErrInternal, cErr, "创建游戏服务器失败")
	}
	s.invalidateAvailableCache(ctx)
	return rec, raw, nil
}

// ResolveServerBySecret 使用 MC 服务器持有的原始 API Secret 解析出其 game_servers.id 与 code，
// 供 /api/v1/game-servers/ws 接入鉴权（id 用于业务归属，code 用于 Hub 连接索引）。
// 计算 SHA256(raw) 后按 api_secret_hash 查启用中的服务器；
// 未命中（secret 错误 / 不存在 / 已停用）返回 ErrServerNotFound，DB 错误包裹为 ErrInternal。
// 与登录接口一致：调用方应统一将错误映射为 401，不向客户端区分具体成因。
func (s *GameServerService) ResolveServerBySecret(ctx context.Context, rawSecret string) (serverID int64, serverCode string, err error) {
	sum := sha256.Sum256([]byte(rawSecret))
	hash := hex.EncodeToString(sum[:])
	rec, err := s.store.GetGameServerBySecretHash(ctx, hash)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return 0, "", Wrap(ErrServerNotFound, err, "解析游戏服务器凭证失败")
		}
		return 0, "", Wrap(ErrInternal, err, "解析游戏服务器凭证失败")
	}
	return rec.ID, rec.Code, nil
}

// ResolveSubscribableServer 以「可订阅」语义解析 Web 端 WS 订阅目标：
// code 格式非法 → ErrServerCodeInvalid；不存在 → ErrServerNotFound；已停用 → ErrServerDisabled。
// 不做读接口限流：WebSocket 握手限流由 transport 层按 user_id / IP 维度单独计数（见 ws_web.go），
// 若复用 GetByCode 会把握手流量计入公开读接口的配额，影响普通 HTTP 查询。
func (s *GameServerService) ResolveSubscribableServer(ctx context.Context, code string) (*repository.GameServerRecord, error) {
	if !validateServerCode(code) {
		return nil, ErrServerCodeInvalid
	}
	rec, err := s.store.GetGameServerByCode(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return nil, Wrap(ErrServerNotFound, err, "查询游戏服务器失败")
		}
		return nil, Wrap(ErrInternal, err, "查询游戏服务器失败")
	}
	if rec.Status != 1 {
		return nil, Wrap(ErrServerDisabled, nil, "服务器已停用，拒绝订阅")
	}
	return rec, nil
}

// ResolveServer 以「存在即可」语义解析游戏服务器：格式非法 / 不存在 → ErrServerNotFound。
//
// 与 ResolveSubscribableServer 的唯一区别是**不校验 status**：解绑这类「对已存在关系生效」的
// 操作不应因为服务器被停用而无法执行，否则用户的历史绑定会被锁死。订阅事件、发绑定码等
// 「此刻能否使用该服务器」的判定仍必须走 ResolveSubscribableServer。
func (s *GameServerService) ResolveServer(ctx context.Context, code string) (*repository.GameServerRecord, error) {
	if !validateServerCode(code) {
		return nil, Wrap(ErrServerNotFound, ErrServerCodeInvalid, "服务器标识格式非法")
	}
	rec, err := s.store.GetGameServerByCode(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return nil, Wrap(ErrServerNotFound, err, "查询游戏服务器失败")
		}
		return nil, Wrap(ErrInternal, err, "查询游戏服务器失败")
	}
	return rec, nil
}

// List 分页列出游戏服务器。statusFilter 为 nil 时不过滤。actorKey 取客户端 IP。
func (s *GameServerService) List(ctx context.Context, actorKey string, statusFilter *int16, limit, offset int) ([]*repository.GameServerRecord, error) {
	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("read", actorKey), s.cfg.ReadRateLimitWindow, s.cfg.ReadRateLimitMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return nil, ErrRateLimited
	}
	recs, err := s.store.ListGameServers(ctx, statusFilter, limit, offset)
	if err != nil {
		return nil, Wrap(ErrInternal, err, "查询游戏服务器列表失败")
	}
	return recs, nil
}

// ListAvailable 返回全部「可用」服务器（status=1，启用），带 Redis 缓存（默认 5 分钟）。
//
// 缓存策略（Cache-Aside）：
//   - 命中：直接返回缓存值，不查库；
//   - 未命中 / Redis 故障 / 缓存值损坏：回源查库，成功后再覆盖写入缓存（失败仅忽略，由 TTL 兜底）。
//
// 失败策略：Redis 是加速层，任何缓存异常都 **fail-open**（退化为直接查库），绝不因缓存故障阻断业务。
//
// 不做限流：本方法只服务「列服务器」这类读多写极少的场景，命中缓存时成本≈一次 GET；
// 公开 HTTP 列表接口仍走 List（按 IP 限流），二者互不影响。
//
// 注意：返回的是展示用快照，存在最长 AvailableCacheTTL 的滞后。
// 任何「该服务器此刻是否可绑定」的判定必须走 ResolveSubscribableServer（直查库），不要复用本方法。
func (s *GameServerService) ListAvailable(ctx context.Context) ([]AvailableServer, error) {
	if s.cache != nil {
		var cached []AvailableServer
		if hit, err := s.cache.GetCache(ctx, repository.GameServerListCacheKey, &cached); err == nil && hit {
			return cached, nil
		}
	}

	enabled := int16(1)
	recs, err := s.store.ListGameServers(ctx, &enabled, s.cfg.AvailableListLimit, 0)
	if err != nil {
		return nil, Wrap(ErrInternal, err, "查询可用服务器列表失败")
	}

	out := make([]AvailableServer, 0, len(recs))
	for _, rec := range recs {
		item := AvailableServer{Code: rec.Code, Name: rec.Name}
		if rec.Description != nil {
			item.Description = *rec.Description
		}
		out = append(out, item)
	}

	if s.cache != nil {
		// 写入失败仅忽略：本次已有正确结果，缓存缺失由 TTL/下次请求自然修复。
		_ = s.cache.SetCache(ctx, repository.GameServerListCacheKey, out, s.cfg.AvailableCacheTTL)
	}
	return out, nil
}

// invalidateAvailableCache 在服务器配置变更后失效「可用服务器列表」缓存。
//
// 失败仅忽略：TTL（默认 5 分钟）会自动兜底，最坏表现是短暂展示旧列表；
// 因此不在业务成功路径上因缓存失效失败而回滚或报错。
func (s *GameServerService) invalidateAvailableCache(ctx context.Context) {
	if s.cache == nil {
		return
	}
	_ = s.cache.DelCache(ctx, repository.GameServerListCacheKey)
}

// GetByCode 按 code 查询单个服务器（公开读）。actorKey 取客户端 IP。
func (s *GameServerService) GetByCode(ctx context.Context, actorKey, code string) (*repository.GameServerRecord, error) {
	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("read", actorKey), s.cfg.ReadRateLimitWindow, s.cfg.ReadRateLimitMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return nil, ErrRateLimited
	}
	if !validateServerCode(code) {
		// 路径 code 格式非法直接 400，避免无意义查库。
		return nil, ErrServerCodeInvalid
	}
	rec, err := s.store.GetGameServerByCode(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return nil, Wrap(ErrServerNotFound, err, "查询游戏服务器失败")
		}
		return nil, Wrap(ErrInternal, err, "查询游戏服务器失败")
	}
	return rec, nil
}

// Update 部分更新服务器（name/description/status）。code 不可改、secret 不可经此改。actorKey 取客户端 IP。
func (s *GameServerService) Update(ctx context.Context, actorKey, code string, u repository.GameServerUpdate) (*repository.GameServerRecord, error) {
	// TODO(role-system): 接入角色系统后，此处须校验调用者为管理员。
	if !validateServerCode(code) {
		return nil, ErrServerCodeInvalid
	}
	if u.Name != nil && !validateServerName(*u.Name) {
		return nil, ErrServerNameInvalid
	}
	if u.Status != nil && *u.Status != 0 && *u.Status != 1 {
		return nil, ErrInvalidParam
	}

	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("manage", actorKey), s.cfg.ManageRateLimitWindow, s.cfg.ManageRateLimitMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return nil, ErrRateLimited
	}

	rec, err := s.store.UpdateGameServer(ctx, code, u)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return nil, Wrap(ErrServerNotFound, err, "更新游戏服务器失败")
		}
		return nil, Wrap(ErrInternal, err, "更新游戏服务器失败")
	}
	// name / description / status 都可能影响列表展示，一律失效缓存。
	s.invalidateAvailableCache(ctx)
	return rec, nil
}

// Disable 软禁用服务器（status 1→0，保留审计与 FK 引用）。actorKey 取客户端 IP。
func (s *GameServerService) Disable(ctx context.Context, actorKey, code string) (*repository.GameServerRecord, error) {
	// TODO(role-system): 接入角色系统后，此处须校验调用者为管理员。
	if !validateServerCode(code) {
		return nil, ErrServerCodeInvalid
	}
	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("manage", actorKey), s.cfg.ManageRateLimitWindow, s.cfg.ManageRateLimitMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return nil, ErrRateLimited
	}
	rec, err := s.store.DisableGameServer(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return nil, Wrap(ErrServerNotFound, err, "禁用游戏服务器失败")
		}
		return nil, Wrap(ErrInternal, err, "禁用游戏服务器失败")
	}
	// 停用后必须立刻从「可用服务器」缓存中消失（否则最长 5 分钟内仍会被列出）。
	s.invalidateAvailableCache(ctx)
	return rec, nil
}

// RotateSecret 轮换服务器的 API Secret，返回原始 secret（仅此一次）。actorKey 取客户端 IP。
// 校验方案（按决策）：仅存 SHA256 哈希，对端在 TLS 通道出示 secret，服务端比 SHA256(presented)==api_secret_hash。
func (s *GameServerService) RotateSecret(ctx context.Context, actorKey, code string) (string, *repository.GameServerRecord, error) {
	// TODO(role-system): 接入角色系统后，此处须校验调用者为管理员。
	if !validateServerCode(code) {
		return "", nil, ErrServerCodeInvalid
	}
	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.ServerRateLimitKey("manage", actorKey), s.cfg.ManageRateLimitWindow, s.cfg.ManageRateLimitMax)
	if rlErr != nil {
		_ = rlErr
	}
	if !allowed {
		return "", nil, ErrRateLimited
	}

	raw, hash, gErr := s.secret.GenerateSecret()
	if gErr != nil {
		return "", nil, Wrap(ErrInternal, gErr, "生成服务器密钥失败")
	}
	rec, err := s.store.RotateGameServerSecret(ctx, code, hash)
	if err != nil {
		if errors.Is(err, repository.ErrServerNotFound) {
			return "", nil, Wrap(ErrServerNotFound, err, "轮换游戏服务器密钥失败")
		}
		return "", nil, Wrap(ErrInternal, err, "轮换游戏服务器密钥失败")
	}
	return raw, rec, nil
}

// defaultServerSecretGenerator 默认密钥生成器：crypto/rand 随机串 → hex 原始串 → SHA256 哈希。
// 原始串仅一次性返回；库只存哈希，满足「Secret 仅用于校验可保存安全哈希」。
type defaultServerSecretGenerator struct{ bytes int }

func (g defaultServerSecretGenerator) GenerateSecret() (string, string, error) {
	b := make([]byte, g.bytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("读取随机字节失败: %w", err)
	}
	raw := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}
