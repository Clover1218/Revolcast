package transport

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"revolcast-backend/internal/config"
	"revolcast-backend/internal/protocol"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// 允许所有跨域请求，如果是公网服务，建议在这里配置域名白名单
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client 代表一个连接上的 Adapter 实例
type Client struct {
	BotID  string // 标识是哪个机器人连上来了
	conn   *websocket.Conn
	send   chan []byte
	logger *zap.Logger
}

// Hub 管理所有连接
type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	logger     *zap.Logger
}

func NewHub(logger *zap.Logger) *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		logger:     logger.With(zap.String("module", "WSHub")),
	}
}

// Run 启动 Hub 的事件循环
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.logger.Info("Adapter 已连接", zap.String("bot_id", client.BotID))
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			h.logger.Info("Adapter 已断开", zap.String("bot_id", client.BotID))
		}
	}
}

// ServeWS 处理 HTTP 升级为 WS 的入口
func ServeWS(hub *Hub, cfg *config.WSConfig, baseLogger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 鉴权：获取 Query 参数
		token := r.URL.Query().Get("token")
		botID := r.URL.Query().Get("bot_id")

		if token != cfg.Token {
			baseLogger.Warn("WS 连接鉴权失败，Token 错误", zap.String("remote_ip", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if botID == "" {
			http.Error(w, "Missing bot_id", http.StatusBadRequest)
			return
		}

		// 2. 升级协议
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			baseLogger.Error("WS 协议升级失败", zap.Error(err))
			return
		}

		// 3. 创建 Client 实例
		clientLogger := baseLogger.With(zap.String("bot_id", botID), zap.String("remote_ip", r.RemoteAddr))
		client := &Client{
			BotID:  botID,
			conn:   conn,
			send:   make(chan []byte, 256),
			logger: clientLogger,
		}

		// 4. 注册并启动读写协程
		hub.register <- client
		go client.writePump()
		go client.readPump(hub)
	}
}

// readPump 负责读取 Adapter 上行的事件
func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512 * 1024) // 限制最大读取 512KB
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Error("WS 读取异常断开", zap.Error(err))
			}
			break
		}

		// 刷新读取超时
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			c.logger.Error("解析 JSON 失败", zap.Error(err))
			continue
		}

		// 派生带 TraceID 的 Logger，完美实现链路追踪
		traceLogger := c.logger.With(zap.String("trace_id", env.TraceID))
		traceLogger.Info("收到上行事件", zap.String("type", env.Type), zap.ByteString("payload", env.Payload))

		// TODO: 在这里把事件丢给后端的业务逻辑层处理
		// 模拟异步处理完业务后，下发指令给 Adapter
		go func(log *zap.Logger) {
			time.Sleep(100 * time.Millisecond) // 模拟处理耗时
			respEnv := protocol.Envelope{
				Type:    "action",
				TraceID: env.TraceID,
				Payload: json.RawMessage(`{"action":"send_msg","message":"后端已收到"}`),
			}
			respData, _ := json.Marshal(respEnv)

			// 将回复放入发送队列
			select {
			case c.send <- respData:
				log.Info("已下发动作指令")
			default:
				log.Warn("发送队列已满，指令下发失败")
			}
		}(traceLogger)
	}
}

// writePump 负责向 Adapter 写数据
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second) // 发送心跳
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// Hub 关闭了 channel
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			// 发送 Ping
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
