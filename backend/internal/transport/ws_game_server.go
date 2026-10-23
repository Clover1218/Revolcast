package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// GameServerClient 是 MC 游戏服务器（鹊桥）的连接。
//
// 标识约定：
//   - 连接标识（Hub 索引键）为 server_code：同一服务器只保留一个有效连接；
//   - 业务归属（业务队列里的 task.ClientID）为 game_servers.id 的十进制字符串：
//     事件落库必须以已鉴权身份为准，不得由 server_name 反查。
//
// 上行消息为鹊桥原始 JSON（不带统一 Envelope），按 sub_type 派生 task.Type。
// 前端 Web 订阅者的推送不经过本结构，避免两端协议互相污染。
type GameServerClient struct {
	*connection
	hub        *Hub
	serverID   int64  // game_servers.id，由 API Secret 鉴权派生
	serverCode string // game_servers.code，Hub 索引键

	// rconMu / rconWait 是 RCON 回执的 echo → 等待方映射（当前仅状态采集使用）。
	// 每连接同时只允许一个在途请求（由采集协程「发完就等」保证）；
	// 仍按 echo 精确匹配，避免将来新增其他 RCON 指令时回执串台。
	rconMu   sync.Mutex
	rconWait map[string]chan string

	// done 用于停止采集协程（连接注销时关闭），stopOnce 保证幂等。
	done     chan struct{}
	stopOnce sync.Once

	// 状态采集配置；statusSink 为 nil 时不采集（未装配或单元测试场景）。
	statusSink   ServerStatusSink
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// newGameServerClient 构造游戏服务器连接。
func newGameServerClient(hub *Hub, conn *websocket.Conn, serverID int64, serverCode string, logger *zap.Logger) *GameServerClient {
	return &GameServerClient{
		connection: newConnection(serverCode, ClientTypeGameServer, conn, logger),
		hub:        hub,
		serverID:   serverID,
		serverCode: serverCode,
		rconWait:   make(map[string]chan string),
		done:       make(chan struct{}),
	}
}

// start 启动读写协程与状态采集协程。读协程退出时停止采集、向 Hub 注销并关闭底层连接。
func (c *GameServerClient) start() {
	go c.writePump()
	go c.readPump()
	c.startStatusPoller()
}

func (c *GameServerClient) readPump() {
	defer func() {
		// 先停采集：断连后不应继续向服务器下发 RCON，并顺手清理状态快照（见 runStatusPoller 的 defer）。
		c.stop()
		c.hub.unregister <- c
		c.closeConn()
	}()
	c.readLoop(c.handleInbound)
}

// stop 幂等停止状态采集协程，并唤醒所有等待中的 RCON 请求。
func (c *GameServerClient) stop() {
	c.stopOnce.Do(func() { close(c.done) })
}

// handleInbound 解析鹊桥原始事件并投递业务队列。
func (c *GameServerClient) handleInbound(message []byte) {
	task, ok := c.parseGameServerEvent(message)
	if !ok {
		return
	}
	enqueueTask(c.hub.pool, c.logger, task)
}

// parseGameServerEvent 解析鹊桥原始 JSON 消息。
//
// 同一连接上可能收到两类消息：
//   - 事件（post_type=notice/message，带 sub_type）：投递业务队列；
//   - 下行指令的响应（post_type=response）：先按 echo 交给等待方（状态采集），
//     无等待方时仅记录日志，**都不投递业务队列**。
//
// 鹊桥不提供 trace_id，事件侧生成一次性 trace 以便链路追踪；type 由 sub_type 派生为 event.game.<sub_type>。
func (c *GameServerClient) parseGameServerEvent(message []byte) (queue.Task, bool) {
	var probe struct {
		PostType string `json:"post_type"`
		SubType  string `json:"sub_type"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		c.logger.Error("解析游戏事件 JSON 失败", zap.Error(err))
		return queue.Task{}, false
	}

	// 下行指令回执：单独处理，不当作事件（否则会因缺少 sub_type 产生误导性告警）。
	if probe.PostType == protocol.GameServerPostTypeResponse {
		var resp protocol.GameServerResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			c.logger.Warn("解析鹊桥下行回执失败", zap.Error(err))
			return queue.Task{}, false
		}
		// 状态采集等待中的回执：投递给等待方后即结束，不再走下面的日志分支。
		if c.deliverRCONEcho(resp) {
			c.logger.Debug("收到 RCON 回执",
				append(c.statusLogFields(), zap.String("echo", resp.Echo), zap.Int("code", resp.Code))...)
			return queue.Task{}, false
		}
		if resp.Code != 200 || (resp.Status != "" && resp.Status != protocol.GameServerStatusSuccess) {
			c.logger.Warn("鹊桥下行指令回执异常",
				zap.String("server_id", strconv.FormatInt(c.serverID, 10)),
				zap.String("api", resp.API),
				zap.Int("code", resp.Code),
				zap.String("status", resp.Status),
				zap.String("echo", resp.Echo))
		} else {
			c.logger.Debug("鹊桥下行指令回执成功",
				zap.String("server_id", strconv.FormatInt(c.serverID, 10)),
				zap.String("api", resp.API),
				zap.String("echo", resp.Echo))
		}
		return queue.Task{}, false
	}

	if probe.SubType == "" {
		c.logger.Warn("游戏事件缺少 sub_type，丢弃", zap.ByteString("raw", message))
		return queue.Task{}, false
	}

	traceID := protocol.NewCommandID() // 鹊桥未提供 trace_id，生成占位以串联日志
	c.logger.Info("收到游戏服务器事件",
		zap.String("server_id", strconv.FormatInt(c.serverID, 10)),
		zap.String("sub_type", probe.SubType),
		zap.String("trace_id", traceID))

	return queue.Task{
		TraceID: traceID,
		// 业务归属使用已鉴权的 server_id（而非 server_code），与事件落库规则保持一致。
		ClientID:   strconv.FormatInt(c.serverID, 10),
		Type:       "event.game." + probe.SubType,
		Payload:    message,
		ReceivedAt: time.Now(),
	}, true
}

// deliverRCONEcho 把一条 RCON 回执投递给按 echo 注册的等待方。
// 返回 false 表示该回执没有等待方（例如普通的 send_private_msg 回执）。
//
// 回执失败（code != 200 / status != SUCCESS / data 不是字符串）时投递空串，
// 由等待方判定采集失败——上层据此**不写快照**，避免坏样本覆盖有效数据。
func (c *GameServerClient) deliverRCONEcho(resp protocol.GameServerResponse) bool {
	if resp.API != protocol.GameAPISendRCONCommand || resp.Echo == "" {
		return false
	}

	c.rconMu.Lock()
	ch, ok := c.rconWait[resp.Echo]
	if ok {
		delete(c.rconWait, resp.Echo)
	}
	c.rconMu.Unlock()
	if !ok {
		return false
	}

	raw := ""
	if resp.Code == 200 && (resp.Status == "" || resp.Status == protocol.GameServerStatusSuccess) {
		if s, valid := resp.RCONEcho(); valid {
			raw = s
		}
	}
	ch <- raw // buffered(1)，且每个 echo 只有一个等待方，不会阻塞
	return true
}

// requestRCON 下发一条 RCON 命令并等待同 echo 的回执。
//
// 并发约束：每连接同时只允许一个在途请求（调用方为单 goroutine 的采集协程）。
// 超时、连接关闭、发送队列满、回执为空都返回 error；调用方只记日志、等下一轮。
func (c *GameServerClient) requestRCON(command string, timeout time.Duration) (string, error) {
	echo := "rcon:" + protocol.NewCommandID()
	ch := make(chan string, 1)

	c.rconMu.Lock()
	c.rconWait[echo] = ch
	c.rconMu.Unlock()
	defer func() {
		c.rconMu.Lock()
		delete(c.rconWait, echo)
		c.rconMu.Unlock()
	}()

	payload, err := json.Marshal(protocol.NewGameRCONRequest(command, echo))
	if err != nil {
		return "", fmt.Errorf("构造 RCON 请求失败: %w", err)
	}
	if !c.deliver(payload) {
		return "", errors.New("发送队列已满或连接已关闭")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case raw := <-ch:
		if raw == "" {
			return "", errors.New("RCON 回执为空（服务端未启用 RCON 或命令未成功执行）")
		}
		return raw, nil
	case <-timer.C:
		return "", fmt.Errorf("等待 RCON 回执超时(%s)", timeout)
	case <-c.done:
		return "", errors.New("连接已关闭")
	}
}
