package transport

import (
	"context"
	"time"

	"revolcast-backend/internal/protocol"

	"go.uber.org/zap"
)

// 状态采集（BIZ-012）的默认参数与超时。
//
// 采集链路：每台已连接的 MC 服务器一条协程，连接建立后延迟 Interval 首次下发 `send_rcon_command: list uuids`，
// 此后按 Interval 周期下发，回执经 echo 关联后交给 ServerStatusSink 落地。详见 docs/服务器状态查询闭环方案.md。
const (
	// defaultStatusPollInterval 采集周期：1 分钟（同时是连接建立后的首次采集延迟）。
	defaultStatusPollInterval = 60 * time.Second
	// defaultStatusPollTimeout 单次等待回执的上限；超时只放弃本次，等下一周期。
	defaultStatusPollTimeout = 5 * time.Second
	// statusWriteTimeout 把解析结果写入 Redis 的上限（与 RCON 超时解耦）。
	statusWriteTimeout = 3 * time.Second
	// statusClearTimeout 断连清理快照的上限。
	statusClearTimeout = 2 * time.Second
)

// ServerStatusSink 是状态采集的落地接口（由 biz.GameStatusService 实现）。
//
// transport 只负责「定时下发 + 收齐回执 + 交给业务」，不解析回显、不碰 Redis，
// 保证协议层与存储层职责分离（见 AGENTS.md §3.1 / §5）。
type ServerStatusSink interface {
	// RecordListUUIDs 解析 `list uuids` 回显并写入快照。
	// 任何失败（解析失败 / 写入失败）都应返回 error，由本层记日志；上层保证失败不覆盖旧值。
	RecordListUUIDs(ctx context.Context, serverID int64, serverCode, rawEcho string) error
	// ClearStatus 删除该服务器的状态快照（连接注销时调用，失败由 TTL 兜底）。
	ClearStatus(ctx context.Context, serverCode string) error
}

// enableStatusPolling 注入状态采集依赖。sink 为 nil 时该连接不采集。
// interval / timeout 传 0 时使用默认值（60s / 5s）。
func (c *GameServerClient) enableStatusPolling(sink ServerStatusSink, interval, timeout time.Duration) {
	if interval <= 0 {
		interval = defaultStatusPollInterval
	}
	if timeout <= 0 {
		timeout = defaultStatusPollTimeout
	}
	c.statusSink = sink
	c.pollInterval = interval
	c.pollTimeout = timeout
}

// startStatusPoller 启动采集协程（未注入 sink 时为空操作）。
func (c *GameServerClient) startStatusPoller() {
	if c.statusSink == nil {
		return
	}
	go c.runStatusPoller()
}

// runStatusPoller 是采集协程：连接建立后先等一个完整周期，此后每 interval 一次，直到连接关闭。
//
// 首次延迟 = 一个 interval（默认 60s），而不是立即下发：服务器刚连上时往往仍在初始化
// （鹊桥 / RCON 尚未就绪），立即拉取大概率拿到空回显，只会白打一次失败日志。
// 代价是新连接的服务器在首分钟内查询返回「未找到」，这是有意的取舍。
//
// 退出一条路径统一收尾：删除状态快照（决策 D3），使「查不到」与「未连接」语义一致；
// 删除失败只记日志，TTL（3 × interval）会兜底。
func (c *GameServerClient) runStatusPoller() {
	defer c.clearStatusSnapshot()

	// 首个周期只等待、不采集：timer 初值取 pollInterval。
	timer := time.NewTimer(c.pollInterval)
	defer timer.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-timer.C:
		}

		c.collectStatusOnce(c.pollTimeout)
		timer.Reset(c.pollInterval)
	}
}

// collectStatusOnce 执行一次「下发 → 等回执 → 落地」。
// 任何失败都只记日志并返回，等待下一周期，不做重试（避免超时放大）。
func (c *GameServerClient) collectStatusOnce(timeout time.Duration) {
	started := time.Now()
	raw, err := c.requestRCON(protocol.GameRCONListUUIDs, timeout)
	if err != nil {
		c.logger.Warn("采集服务器状态失败",
			append(c.statusLogFields(), zap.String("command", protocol.GameRCONListUUIDs), zap.Error(err))...)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
	defer cancel()
	if err := c.statusSink.RecordListUUIDs(ctx, c.serverID, c.serverCode, raw); err != nil {
		c.logger.Warn("落地服务器状态失败", append(c.statusLogFields(), zap.Error(err))...)
		return
	}

	c.logger.Debug("服务器状态已更新",
		append(c.statusLogFields(), zap.Duration("elapsed", time.Since(started)))...)
}

// clearStatusSnapshot 删除该服务器的状态快照（尽力而为）。
func (c *GameServerClient) clearStatusSnapshot() {
	if c.statusSink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusClearTimeout)
	defer cancel()
	if err := c.statusSink.ClearStatus(ctx, c.serverCode); err != nil {
		c.logger.Warn("清理服务器状态快照失败", append(c.statusLogFields(), zap.Error(err))...)
		return
	}
	c.logger.Debug("服务器状态快照已清理", c.statusLogFields()...)
}

// statusLogFields 返回状态采集的统一日志字段。
func (c *GameServerClient) statusLogFields() []zap.Field {
	return []zap.Field{
		zap.Int64("server_id", c.serverID),
		zap.String("server_code", c.serverCode),
	}
}
