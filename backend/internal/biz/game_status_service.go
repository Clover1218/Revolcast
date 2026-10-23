package biz

import (
	"context"
	"fmt"
	"time"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

const (
	// defaultStatusPollInterval 是状态采集默认周期；TTL 由其乘以 statusTTLFactor 推导。
	defaultStatusPollInterval = 60 * time.Second
	// statusTTLFactor 是 TTL 相对采集周期的倍数：容忍 1~2 次采集失败，同时保证
	// 采集停摆（进程崩溃、服务器掉线）后 3 个周期内状态键必然消失。
	statusTTLFactor = 3
	// statusSourceRCON 标识采集方式为 RCON 命令。
	statusSourceRCON = "rcon"
)

// ServerStatusCache 是 GameStatusService 对状态快照存储的依赖（便于测试注入 fake）。
//
// 复用「缓存读写」这组既有能力而非新增专用仓储方法：状态快照本身就是可随时重建的
// 实时态数据，语义与缓存一致；生产实现为 *repository.RedisRepo。
type ServerStatusCache interface {
	GetCache(ctx context.Context, key string, dest any) (hit bool, err error)
	SetCache(ctx context.Context, key string, val any, ttl time.Duration) error
	DelCache(ctx context.Context, keys ...string) error
}

// GameStatusConfig 是状态业务的 tunable 参数。
type GameStatusConfig struct {
	// PollInterval 是采集周期（与 transport 的定时器保持一致），用于推导 TTL。默认 60s。
	PollInterval time.Duration
	// TTL 显式指定快照有效期；为 0 时取 3 × PollInterval。
	TTL time.Duration
}

// StatusPlayer 是状态视图中的单个在线玩家。
type StatusPlayer struct {
	Nickname string
	UUID     string
}

// ServerStatusView 是「查询某服务器在线状态」的业务结论（已与 Redis JSON 解耦）。
//
// 键不存在时 GetStatus 返回 (nil, nil)：这是可预期的正常分支（服务器未连接 / 刚掉线），
// 因此不用 error 表达，也不新增对外错误码。
type ServerStatusView struct {
	ServerCode string
	Online     int
	MaxPlayers int
	Players    []StatusPlayer
	UpdatedAt  time.Time
	Source     string
}

// GameStatusService 负责服务器在线状态快照的采集落库与查询。
//
// 生命周期：transport 的采集协程周期性调用 RecordListUUIDs；QQ 指令调用 GetStatus；
// 连接断开始调用 ClearStatus。本服务不依赖 WS 运行时上下文（见 AGENTS.md §3.1）。
type GameStatusService struct {
	cache ServerStatusCache
	ttl   time.Duration
}

func NewGameStatusService(cache ServerStatusCache, cfg GameStatusConfig) *GameStatusService {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultStatusPollInterval
	}
	if cfg.TTL <= 0 {
		cfg.TTL = statusTTLFactor * cfg.PollInterval
	}
	return &GameStatusService{cache: cache, ttl: cfg.TTL}
}

// TTL 返回快照有效期（供测试与排查使用）。
func (s *GameStatusService) TTL() time.Duration { return s.ttl }

// RecordListUUIDs 解析一次 `list uuids` 回显并写入快照。
//
// serverID 必须来自 WS 连接的**已鉴权身份**（不接受外部传入，也不由 server_name 反查）。
//
// 失败策略（核心约束）：**任何失败都不写快照**，宁可让上一个有效值自然过期，
// 也不让超时、RCON 未启用、本地化文本等坏样本把玩家列表清空。
// 返回的 error 只用于上层记日志，不对外暴露。
func (s *GameStatusService) RecordListUUIDs(ctx context.Context, serverID int64, serverCode, rawEcho string) error {
	snap, err := protocol.ParseListUUIDsEcho(rawEcho)
	if err != nil {
		return fmt.Errorf("解析服务器 %s 的 list uuids 回显失败: %w", serverCode, err)
	}

	players := make([]repository.StatusPlayer, 0, len(snap.Players))
	for _, p := range snap.Players {
		players = append(players, repository.StatusPlayer{Nickname: p.Nickname, UUID: p.UUID})
	}

	rec := repository.ServerStatusRecord{
		ServerID:   serverID,
		ServerCode: serverCode,
		Online:     snap.Online,
		MaxPlayers: snap.MaxPlayers,
		Players:    players,
		UpdatedAt:  protocol.FormatEventTime(time.Now()),
		Source:     statusSourceRCON,
	}
	if err := s.cache.SetCache(ctx, repository.ServerStatusKey(serverCode), rec, s.ttl); err != nil {
		return fmt.Errorf("写入服务器 %s 的状态快照失败: %w", serverCode, err)
	}
	return nil
}

// GetStatus 读取某服务器的状态快照。
//
// 返回值语义：
//   - (view, nil)：命中；
//   - (nil, nil)：键不存在（服务器未连接 / 刚掉线 / 从未采集到），属正常业务分支；
//   - (nil, err)：Redis 故障，调用方应渲染「暂时不可用」而不是「未找到」。
func (s *GameStatusService) GetStatus(ctx context.Context, serverCode string) (*ServerStatusView, error) {
	var rec repository.ServerStatusRecord
	hit, err := s.cache.GetCache(ctx, repository.ServerStatusKey(serverCode), &rec)
	if err != nil {
		return nil, Wrap(ErrInternal, err, "读取服务器状态快照失败")
	}
	if !hit {
		return nil, nil
	}

	players := make([]StatusPlayer, 0, len(rec.Players))
	for _, p := range rec.Players {
		players = append(players, StatusPlayer{Nickname: p.Nickname, UUID: p.UUID})
	}
	view := &ServerStatusView{
		ServerCode: rec.ServerCode,
		Online:     rec.Online,
		MaxPlayers: rec.MaxPlayers,
		Players:    players,
		Source:     rec.Source,
	}
	if view.ServerCode == "" {
		view.ServerCode = serverCode
	}
	// 时间只用于展示/排查，解析失败不应影响查询结果。
	if t, pErr := time.Parse(protocol.EventTimeLayout, rec.UpdatedAt); pErr == nil {
		view.UpdatedAt = t
	}
	return view, nil
}

// ClearStatus 在服务器 WS 连接注销时删除快照。
//
// 尽力而为：失败只返回 error 供上层记日志，TTL 会兜底过期（最坏多展示 3 个周期）。
// 当前只有进程崩溃会走到 TTL 兜底路径。
func (s *GameStatusService) ClearStatus(ctx context.Context, serverCode string) error {
	if err := s.cache.DelCache(ctx, repository.ServerStatusKey(serverCode)); err != nil {
		return fmt.Errorf("删除服务器 %s 的状态快照失败: %w", serverCode, err)
	}
	return nil
}
