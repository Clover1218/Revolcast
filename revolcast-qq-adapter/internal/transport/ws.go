package transport

import (
	"context"
	"encoding/json"
	"time"

	"revolcast-qq-adapter/internal/protocol"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const serverURL = "ws://127.0.0.1:8080/ws"

// WSClient 封装
type WSClient struct {
	url    string
	send   chan []byte // 业务层写到这里，writePump 来消费
	ctx    context.Context
	logger *zap.Logger
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

// Start 启动循环（包含重连逻辑）
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

// writePump 独占写操作，保证并发安全
func (c *WSClient) writePump(conn *websocket.Conn) {
	// 心跳定时器
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send: // 从业务层拿数据
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			conn.WriteMessage(websocket.TextMessage, msg)
		case <-ticker.C:
			// 定时发送 Ping 保活
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump 独占读操作
func (c *WSClient) readPump(conn *websocket.Conn) {
	defer conn.Close()
	conn.SetReadLimit(512 * 1024) // 限制最大读取 512KB，防 OOM
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	// 收到 Pong 刷新超时时间
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
		// 刷新超时
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// 解析信封
		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			c.logger.Error("[解析失败]", zap.Error(err))
			continue
		}
		// 分发给业务层处理
		go c.handleBusiness(env)
	}
}

// 发送业务消息给后端
func (c *WSClient) SendEvent(payload interface{}) {
	traceID := uuid.NewString()
	data, _ := json.Marshal(payload)
	env := protocol.Envelope{
		Type:    "event",
		TraceID: traceID,
		Payload: data,
	}
	envData, _ := json.Marshal(env)

	select {
	case c.send <- envData:
		c.logger.Info("[发送事件]", zap.String("trace_id", traceID))
	default:
		c.logger.Warn("[警告] 发送队列已满，丢弃消息")
	}
}

// 辅助：监听连接是否断开
func (c *WSClient) waitDisconnect(conn *websocket.Conn) chan struct{} {
	done := make(chan struct{})
	conn.SetCloseHandler(func(code int, text string) error {
		close(done)
		return nil
	})
	return done
}

// 模拟业务层处理下行指令
func (c *WSClient) handleBusiness(env protocol.Envelope) {
	if env.Type == "action" {
		c.logger.Info("[收到指令]", zap.String("trace_id", env.TraceID))
	}
}

// func main() {
// 	ctx, cancel := context.WithCancel(context.Background())
// 	defer cancel()

// 	client := NewWSClient(ctx, serverURL)
// 	go client.Start()

// 	// 模拟 ZeroBot 发送消息
// 	time.Sleep(2 * time.Second)
// 	for i := 0; i < 3; i++ {
// 		client.SendEvent(map[string]interface{}{
// 			"user_id": 1001,
// 			"msg":     "测试消息",
// 		})
// 		time.Sleep(1 * time.Second)
// 	}
// 	time.Sleep(2 * time.Second)
// }
