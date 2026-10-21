package biz

import (
	"context"
	"time"

	"revolcast-backend/internal/repository"
)

// Config 绑定码业务的 tunable 参数。
// 默认值取自业务流程设计文档 §2.1：绑定码有效期≈5 分钟，限流每 qq_uin 5 分钟最多 3 次。
// 当前以常量驱动，如需运行时可调再提升到 config.yaml（避免过度设计）。
type Config struct {
	BindCodeTTL     time.Duration // 绑定码有效期
	RateLimitWindow time.Duration // 限流窗口
	RateLimitMax    int64         // 限流窗口内最大尝试次数
}

// BindCodeService 实现 BIZ-001：生成 QQ 绑定码。
// 平台无关，不依赖 ZeroBot 运行时上下文（见 AGENTS.md §3.1）。
type BindCodeService struct {
	pg  *repository.QQBindRepo
	rdb *repository.RedisRepo
	cfg Config
}

func NewBindCodeService(pg *repository.QQBindRepo, rdb *repository.RedisRepo, cfg Config) *BindCodeService {
	if cfg.BindCodeTTL <= 0 {
		cfg.BindCodeTTL = 5 * time.Minute
	}
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = 5 * time.Minute
	}
	if cfg.RateLimitMax <= 0 {
		cfg.RateLimitMax = 3
	}
	return &BindCodeService{pg: pg, rdb: rdb, cfg: cfg}
}

// GenerateBindCode 实现 BIZ-001 步骤 03-06。
//
//	03 限流检查：命中则 ErrRateLimited
//	04 已绑定检查：存在 active qq_bindings 则 ErrQQAlreadyBound
//	05 幂等取/建：存在未过期待使用码直接返回同一码，否则生成新码
//	06 持久化：写入 qq_bind_codes（bot_id, qq_uin, expires_at≈5min, status=0, trace_id）
//
// 调用方负责把返回的绑定码通过 command.qq.send_private_message 私聊下发。
// 私聊发送失败不影响业务成功（码已持久化，用户可重新请求获取同一码）。
func (s *BindCodeService) GenerateBindCode(ctx context.Context, qqUIN int64, botID, traceID string) (string, error) {
	// 步骤 03：限流。repository 在 Redis 故障时 fail-open 并返回放行，这里只取 bool。
	allowed, rlErr := s.rdb.AllowBindCodeAttempt(ctx, qqUIN, s.cfg.RateLimitWindow, s.cfg.RateLimitMax)
	if rlErr != nil {
		// 仅记录，不阻断（fail-open）。
		_ = rlErr
	}
	if !allowed {
		return "", ErrRateLimited
	}

	// 步骤 04：已绑定检查。
	bound, err := s.pg.IsQQActiveBound(ctx, qqUIN)
	if err != nil {
		return "", ErrInternal
	}
	if bound {
		return "", ErrQQAlreadyBound
	}

	// 步骤 05：幂等取/建——已有未过期待使用码则直接返回。
	code, _, found, err := s.pg.GetPendingBindCode(ctx, qqUIN)
	if err != nil {
		return "", ErrInternal
	}
	if found {
		return code, nil
	}

	// 步骤 06：持久化新码。
	expiresAt := time.Now().UTC().Add(s.cfg.BindCodeTTL)
	created, err := s.pg.CreateBindCode(ctx, qqUIN, botID, traceID, expiresAt)
	if err != nil {
		return "", ErrInternal
	}
	return created, nil
}
