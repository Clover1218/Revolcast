package transport

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"revolcast-qq-adapter/internal/protocol"

	"github.com/gorilla/websocket"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"
	"go.uber.org/zap"
)

const serverURL = "ws://127.0.0.1:8080/api/v1/adapter/ws"

// WSClient 封装与 Backend 的 WebSocket 连接（含重连）。
type WSClient struct {
	url    string
	send   chan []byte // 业务层写到这里，writePump 来消费
	ctx    context.Context
	logger *zap.Logger

	// botSelfID 记录本 Adapter 所连机器人的 QQ 号（从入站事件获取），
	// 用于调用 OneBot 私聊发送 API（GetBot 需要 self_id）。
	botSelfID atomic.Int64
}

// SetSelfID 在收到入站事件时记录机器人自身 QQ 号。
func (c *WSClient) SetSelfID(selfID int64) {
	c.botSelfID.Store(selfID)
}

func NewWSClient(ctx context.Context, url string, logger *zap.Logger) *WSClient {
	instanceLogger := logger.With(zap.String("module", "WSClient"), zap.String("target", url))
	return &WSClient{
		url:    url,
		send:   make(chan []byte, 100), // 发送缓冲队列
		ctx:    ctx,
		logger: instanceLogger,
	}
}

// Start 启动连接循环（包含重连逻辑）。
func (c *WSClient) Start() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
			if err != nil {
				c.logger.Error("[连接失败] %v, 5秒后重试...", zap.Error(err))
				time.Sleep(5 * time.Second)
				continue
			}
			c.logger.Info("[连接成功] 连上 Backend")

			// 启动读写泵
			go c.readPump(conn)
			go c.writePump(conn)

			// 阻塞等待连接断开或上下文取消
			<-c.waitDisconnect(conn)
		}
	}
}

// writePump 独占写操作，保证并发安全。
func (c *WSClient) writePump(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			conn.WriteMessage(websocket.TextMessage, msg)
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump 独占读操作。
func (c *WSClient) readPump(conn *websocket.Conn) {
	defer conn.Close()
	conn.SetReadLimit(512 * 1024)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			c.logger.Error("[读取断开]", zap.Error(err))
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			c.logger.Error("[解析失败]", zap.Error(err))
			continue
		}
		go c.handleBusiness(env)
	}
}

// SendEnvelope 发送一条标准化信封到 Backend（非阻塞，队列满则丢弃并告警）。
func (c *WSClient) SendEnvelope(envType, traceID string, payload interface{}) {
	env := protocol.Envelope{
		Version: "v1",
		Type:    envType,
		TraceID: traceID,
	}
	env.Payload, _ = json.Marshal(payload)
	envData, err := json.Marshal(env)
	if err != nil {
		c.logger.Error("[信封序列化失败]", zap.Error(err))
		return
	}

	select {
	case c.send <- envData:
		// ok
	default:
		c.logger.Warn("[警告] 发送队列已满，丢弃消息")
	}
}

// waitDisconnect 辅助监听连接断开。
func (c *WSClient) waitDisconnect(conn *websocket.Conn) chan struct{} {
	done := make(chan struct{})
	conn.SetCloseHandler(func(code int, text string) error {
		close(done)
		return nil
	})
	return done
}

// handleBusiness 处理 Backend 下发的指令。
func (c *WSClient) handleBusiness(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeCommandQQSendPrivateMsg:
		c.handleSendPrivateMessage(env)
	case protocol.TypeCommandQQSendGroupMsg:
		// 群内 @ 机器人触发业务时，Backend 在原群回执（临时能力，见 main.go 注册的群聊 handler）。
		c.handleSendGroupMessage(env)
	default:
		c.logger.Info("[收到未处理指令]", zap.String("type", env.Type))
	}
}

// segmentsText 拼接消息段中的纯文本（MVP 仅支持文本，富媒体段后续扩展）。
func segmentsText(segments []protocol.QQSegment) string {
	var sb []rune
	for _, seg := range segments {
		if seg.Type != "text" {
			continue
		}
		var t string
		if raw, ok := seg.Data["text"]; ok {
			_ = json.Unmarshal(raw, &t)
		}
		sb = append(sb, []rune(t)...)
	}
	return string(sb)
}

// botCtx 返回用于主动发送消息的 ZeroBot 上下文。
// self_id 来自入站事件（SetSelfID），未收到过任何消息前无法发送。
// 失败时返回面向内部日志的原因文案。
func (c *WSClient) botCtx() (*zero.Ctx, string) {
	selfID := c.botSelfID.Load()
	if selfID == 0 {
		c.logger.Error("[未获取到 bot self_id，无法发送消息]")
		return nil, "unknown bot self_id"
	}
	botCtx := zero.GetBot(selfID)
	if botCtx == nil {
		c.logger.Error("[GetBot 返回空，bot 未连接]", zap.Int64("self_id", selfID))
		return nil, "bot not connected"
	}
	return botCtx, ""
}

// handleSendPrivateMessage 处理 command.qq.send_private_message：
// 调用 OneBot 私聊发送 API，并回 result.qq.send_private_message（原样返回 command_id）。
func (c *WSClient) handleSendPrivateMessage(env protocol.Envelope) {
	var p protocol.QQSendPrivateMsgPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.logger.Error("[指令解析失败]", zap.Error(err))
		return
	}

	qq, err := strconv.ParseInt(p.ReceiverID, 10, 64)
	if err != nil {
		c.logger.Error("[receiver_id 非法]", zap.String("receiver_id", p.ReceiverID), zap.Error(err))
		return
	}

	botCtx, reason := c.botCtx()
	if botCtx == nil {
		res := protocol.QQSendPrivateMessageResultPayload{
			CommandID:    p.CommandID,
			Status:       protocol.CommandStatusFailed,
			ErrorCode:    "QQ_SEND_API_ERROR",
			ErrorMessage: reason,
		}
		c.SendEnvelope(protocol.TypeResultQQSendPrivateMsg, env.TraceID, res)
		return
	}
	platformMsgID := botCtx.SendPrivateMessage(qq, message.Text(segmentsText(p.MessageSegments)))

	res := protocol.QQSendPrivateMessageResultPayload{
		CommandID:         p.CommandID,
		Status:            protocol.CommandStatusSuccess,
		PlatformMessageID: strconv.FormatInt(platformMsgID, 10),
	}
	c.SendEnvelope(protocol.TypeResultQQSendPrivateMsg, env.TraceID, res)
}

// handleSendGroupMessage 处理 command.qq.send_group_msg：在指定群里回一条文本消息。
//
// 根因与私聊不同：群聊回执目前不产生 result.*（Backend 侧尚未消费任何 result.qq.*），
// 因此这里只记日志，不虚构一个无人处理的回执类型。随群聊回执正式化时再补。
func (c *WSClient) handleSendGroupMessage(env protocol.Envelope) {
	var p protocol.QQSendGroupMsgPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.logger.Error("[群聊指令解析失败]", zap.Error(err))
		return
	}

	groupID, err := strconv.ParseInt(p.GroupID, 10, 64)
	if err != nil {
		c.logger.Error("[group_id 非法]", zap.String("group_id", p.GroupID), zap.Error(err))
		return
	}

	botCtx, reason := c.botCtx()
	if botCtx == nil {
		c.logger.Error("[群聊消息发送失败]", zap.String("reason", reason),
			zap.String("group_id", p.GroupID), zap.String("command_id", p.CommandID))
		return
	}
	msgID := botCtx.SendGroupMessage(groupID, message.Text(segmentsText(p.MessageSegments)))
	c.logger.Info("[群聊消息已发送]",
		zap.String("group_id", p.GroupID),
		zap.String("command_id", p.CommandID),
		zap.Int64("platform_message_id", msgID),
	)
}
