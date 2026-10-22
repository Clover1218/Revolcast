package transport

import (
	"encoding/json"
	"time"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// AdapterClient 是 QQ Adapter 等「共享密钥」接入端的连接。
// 上行消息为统一 Envelope（version/type/trace_id/payload），
// 下行由 Hub 按 client_id 回传 command.* 指令。
type AdapterClient struct {
	*connection
	hub *Hub
}

// newAdapterClient 构造 Adapter 连接（公共生命周期由 connection 提供）。
func newAdapterClient(hub *Hub, conn *websocket.Conn, clientID string, logger *zap.Logger) *AdapterClient {
	return &AdapterClient{
		connection: newConnection(clientID, ClientTypeAdapter, conn, logger),
		hub:        hub,
	}
}

// start 启动读写协程。读协程退出时向 Hub 注销并关闭底层连接。
func (c *AdapterClient) start() {
	go c.writePump()
	go c.readPump()
}

func (c *AdapterClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.closeConn()
	}()
	c.readLoop(c.handleInbound)
}

// handleInbound 解析统一 Envelope 并投递业务队列。
func (c *AdapterClient) handleInbound(message []byte) {
	task, ok := c.parseEnvelopeEvent(message)
	if !ok {
		return
	}
	enqueueTask(c.hub.pool, c.logger, task)
}

// parseEnvelopeEvent 解析统一 Envelope 包装的上行消息（Adapter 等共享密钥接入端）。
// 返回 (task, false) 表示消息非法/应丢弃。
func (c *AdapterClient) parseEnvelopeEvent(message []byte) (queue.Task, bool) {
	var env protocol.Envelope
	if err := json.Unmarshal(message, &env); err != nil {
		c.logger.Error("解析 JSON 失败", zap.Error(err))
		return queue.Task{}, false
	}

	// 派生带 TraceID 的 Logger，实现链路追踪
	traceLogger := c.logger.With(zap.String("trace_id", env.TraceID))
	traceLogger.Info("收到上行事件", zap.String("type", env.Type), zap.ByteString("payload", env.Payload))

	return queue.Task{
		TraceID:    env.TraceID,
		ClientID:   c.ConnID(),
		Type:       env.Type,
		Payload:    env.Payload,
		ReceivedAt: time.Now(),
	}, true
}
