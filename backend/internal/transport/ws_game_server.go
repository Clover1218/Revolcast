package transport

import (
	"encoding/json"
	"strconv"
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
}

// newGameServerClient 构造游戏服务器连接。
func newGameServerClient(hub *Hub, conn *websocket.Conn, serverID int64, serverCode string, logger *zap.Logger) *GameServerClient {
	return &GameServerClient{
		connection: newConnection(serverCode, ClientTypeGameServer, conn, logger),
		hub:        hub,
		serverID:   serverID,
		serverCode: serverCode,
	}
}

// start 启动读写协程。读协程退出时向 Hub 注销并关闭底层连接。
func (c *GameServerClient) start() {
	go c.writePump()
	go c.readPump()
}

func (c *GameServerClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.closeConn()
	}()
	c.readLoop(c.handleInbound)
}

// handleInbound 解析鹊桥原始事件并投递业务队列。
func (c *GameServerClient) handleInbound(message []byte) {
	task, ok := c.parseGameServerEvent(message)
	if !ok {
		return
	}
	enqueueTask(c.hub.pool, c.logger, task)
}

// parseGameServerEvent 解析鹊桥原始 JSON 游戏事件。
// 鹊桥不提供 trace_id，这里生成一次性 trace 以便链路追踪；type 由 sub_type 派生为 event.game.<sub_type>。
func (c *GameServerClient) parseGameServerEvent(message []byte) (queue.Task, bool) {
	var probe struct {
		SubType string `json:"sub_type"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		c.logger.Error("解析游戏事件 JSON 失败", zap.Error(err))
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
