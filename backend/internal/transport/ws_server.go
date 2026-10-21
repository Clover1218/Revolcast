package transport

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"revolcast-backend/internal/config"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

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

	closeOnce sync.Once // 保护 send channel 不被重复 close（unregister 与 CloseAll 都可能触发）
}

// closeSend 安全关闭发送通道，幂等。
func (c *Client) closeSend() {
	c.closeOnce.Do(func() { close(c.send) })
}

// Hub 管理所有连接，并持有全局业务队列。
type Hub struct {
	clients      map[*Client]bool
	clientsByBot map[string]*Client // bot_id -> client，便于按 bot 回传 Action
	register     chan *Client
	unregister   chan *Client
	mu           sync.RWMutex
	logger       *zap.Logger

	pool *queue.Pool // 业务队列，所有连接共享（生产者-消费者模型的消费者池）
}

func NewHub(logger *zap.Logger) *Hub {
	return &Hub{
		clients:      make(map[*Client]bool),
		clientsByBot: make(map[string]*Client),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
		logger:       logger.With(zap.String("module", "WSHub")),
	}
}

// SetPool 注入业务队列（在 main 中先建 Hub 再建 Pool，由 Pool 的 Handler 反向引用 Hub）。
func (h *Hub) SetPool(p *queue.Pool) { h.pool = p }

// Run 启动 Hub 的事件循环
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.clientsByBot[client.BotID] = client
			h.mu.Unlock()
			h.logger.Info("Adapter 已连接", zap.String("bot_id", client.BotID))
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if h.clientsByBot[client.BotID] == client {
					delete(h.clientsByBot, client.BotID)
				}
				client.closeSend()
			}
			h.mu.Unlock()
			h.logger.Info("Adapter 已断开", zap.String("bot_id", client.BotID))
		}
	}
}

// SendToBot 向指定 bot_id 的 Adapter 回传 Action 指令（非阻塞）。
// 返回 false 表示该 bot 不在线或发送队列已满；调用方（Worker）不应因此阻塞。
func (h *Hub) SendToBot(botID string, data []byte) bool {
	h.mu.RLock()
	client, ok := h.clientsByBot[botID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case client.send <- data:
		return true
	default:
		return false
	}
}

// CloseAll 关闭所有 Adapter 连接，使 readPump 退出、停止产生新任务。
// 用于优雅关闭阶段，先于 pool.Close 执行。
func (h *Hub) CloseAll() {
	h.mu.Lock()
	for client := range h.clients {
		_ = client.conn.Close()
		client.closeSend()
	}
	h.clients = make(map[*Client]bool)
	h.clientsByBot = make(map[string]*Client)
	h.mu.Unlock()
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

// readPump 负责读取 Adapter 上行的事件，并投递到业务队列。
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

		// 投递到业务队列：绝不在此等待业务完成，避免阻塞读循环与心跳（背压由 EnqueueTimeout 控制）。
		if hub.pool == nil {
			traceLogger.Warn("业务队列未初始化，丢弃事件")
			continue
		}
		task := queue.Task{
			TraceID:    env.TraceID,
			BotID:      c.BotID,
			Type:       env.Type,
			Payload:    env.Payload,
			ReceivedAt: time.Now(),
		}
		if err := hub.pool.Enqueue(task); err != nil {
			// 背压溢出或队列已关闭：记录 + 计数，后续可回一个 busy Ack 给 Adapter 做降级。
			traceLogger.Warn("事件入队失败",
				zap.String("type", env.Type), zap.Error(err))
		}
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
