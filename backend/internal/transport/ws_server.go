package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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

// Client 代表一个已接入的客户端连接（Adapter / MC 服务器等）。
type Client struct {
	ClientID   string // 全局唯一身份 ID（如 qq-adapter-01 / mc-survival-01），用于回传路由与去重
	ClientType string // 接入端类型：adapter（QQ 适配器）/ mc_server（MC 服务器）/ 未来扩展
	conn       *websocket.Conn
	send       chan []byte
	logger     *zap.Logger

	// 发送与关闭的生命周期控制：
	// sendMu 保证 deliver（发送）与 closeSend（关闭）互斥，杜绝「向已关闭 channel 发送」的 panic；
	// closed 标记配合 sendMu 实现幂等关闭（取代原 sync.Once）；
	// connCloseOnce 保护底层连接不被 readPump/writePump/CloseAll/踢旧 并发或重复关闭。
	sendMu        sync.Mutex
	closed        bool
	connCloseOnce sync.Once
}

// deliver 非阻塞地向客户端发送数据；连接已关闭或发送队列已满时返回 false。
// 必须在 sendMu 保护下执行，与 closeSend 互斥，避免向已关闭的 channel 发送引发 panic。
func (c *Client) deliver(data []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

// closeSend 安全关闭发送通道，幂等。与 deliver 共用 sendMu，保证关闭期间不会有并发发送。
func (c *Client) closeSend() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}

// closeConn 安全关闭底层连接，幂等；避免多处并发调用 conn.Close()。
func (c *Client) closeConn() {
	c.connCloseOnce.Do(func() { _ = c.conn.Close() })
}

// Hub 管理所有连接，并持有全局业务队列。
type Hub struct {
	clients     map[*Client]bool
	clientsByID map[string]*Client // client_id -> client，便于按身份回传 Action
	register    chan *Client
	unregister  chan *Client
	mu          sync.RWMutex
	logger      *zap.Logger

	pool *queue.Pool // 业务队列，所有连接共享（生产者-消费者模型的消费者池）
}

func NewHub(logger *zap.Logger) *Hub {
	return &Hub{
		clients:     make(map[*Client]bool),
		clientsByID: make(map[string]*Client),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		logger:      logger.With(zap.String("module", "WSHub")),
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
			// 同一 ClientID 同时只允许一个有效连接：若已存在旧连接，踢掉旧连接让新连接接管。
			if old, exists := h.clientsByID[client.ClientID]; exists && old != client {
				h.logger.Warn("检测到重复 ClientID 连接，踢掉旧连接",
					zap.String("client_id", client.ClientID),
					zap.String("client_type", client.ClientType))
				old.closeConn()
				old.closeSend()
				delete(h.clients, old)
				// 旧连接稍后由其 readPump 触发 unregister；届时映射已指向新连接，
				// 注销保护（clientsByID[clientID] == client 判断）会跳过删除，避免误删新映射。
			}
			h.clients[client] = true
			h.clientsByID[client.ClientID] = client
			h.mu.Unlock()
			h.logger.Info("客户端已连接",
				zap.String("client_id", client.ClientID),
				zap.String("client_type", client.ClientType))
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if h.clientsByID[client.ClientID] == client {
					delete(h.clientsByID, client.ClientID)
				}
				client.closeSend()
			}
			h.mu.Unlock()
			h.logger.Info("客户端已断开",
				zap.String("client_id", client.ClientID),
				zap.String("client_type", client.ClientType))
		}
	}
}

// SendToClient 向指定 client_id 的客户端回传 Action / 指令（非阻塞）。
// 返回 false 表示该客户端不在线或发送队列已满；调用方（Worker）不应因此阻塞。
// 发送经由 client.deliver，由 sendMu 与 closeSend 互斥，杜绝向已关闭 channel 发送的 panic。
func (h *Hub) SendToClient(clientID string, data []byte) bool {
	h.mu.RLock()
	client, ok := h.clientsByID[clientID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	return client.deliver(data)
}

// CloseAll 关闭所有客户端连接，使 readPump 退出、停止产生新任务。
// 用于优雅关闭阶段，先于 pool.Close 执行。
func (h *Hub) CloseAll() {
	h.mu.Lock()
	for client := range h.clients {
		client.closeConn()
		client.closeSend()
	}
	h.clients = make(map[*Client]bool)
	h.clientsByID = make(map[string]*Client)
	h.mu.Unlock()
}

// serveWSConnection 完成「协议升级 + 创建 Client + 注册 Hub + 启动读写协程」这一段，
// 被 ServeWS（adapter 共享密钥）与 ServeGameServerWS（游戏服务器 per-server 密钥）共用，避免重复。
// 调用前应已完成鉴权（clientID 与 clientType 已确定）。升级失败时仅记日志并返回 false。
func serveWSConnection(hub *Hub, w http.ResponseWriter, r *http.Request, clientID, clientType string, baseLogger *zap.Logger) bool {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		baseLogger.Error("WS 协议升级失败", zap.Error(err))
		return false
	}

	clientLogger := baseLogger.With(
		zap.String("client_id", clientID),
		zap.String("client_type", clientType),
		zap.String("remote_ip", r.RemoteAddr))
	client := &Client{
		ClientID:   clientID,
		ClientType: clientType,
		conn:       conn,
		send:       make(chan []byte, 256),
		logger:     clientLogger,
	}

	hub.register <- client
	go client.writePump()
	go client.readPump(hub)
	return true
}

// GameServerAuth 是 /api/v1/game-servers/ws 接入鉴权所需的最小接口（依赖倒置，便于测试注入 fake）。
type GameServerAuth interface {
	// ResolveServerBySecret 以原始 API Secret 解析出 game_servers.id；鉴权失败返回 error。
	ResolveServerBySecret(ctx context.Context, rawSecret string) (int64, error)
}

// ServeWS 处理 HTTP 升级为 WS 的入口（供 QQ Adapter 等共享密钥接入端）。
// clientType 由路由决定（/api/v1/adapter/ws => adapter），客户端不可自行声明类型，从而避免冒用其他接入端类型。
// 鉴权：query.token 比对共享密钥 cfg.WS.Token；query.client_id 为自声明全局身份（用于回传路由与去重）。
// 后续接入端独立凭证与 wss 由部署层/后续迭代处理。
func ServeWS(hub *Hub, cfg *config.WSConfig, baseLogger *zap.Logger, clientType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		clientID := r.URL.Query().Get("client_id")

		if token != cfg.Token {
			baseLogger.Warn("WS 连接鉴权失败，Token 错误",
				zap.String("client_type", clientType), zap.String("remote_ip", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if clientID == "" {
			http.Error(w, "Missing client_id", http.StatusBadRequest)
			return
		}

		serveWSConnection(hub, w, r, clientID, clientType, baseLogger)
	}
}

// ServeGameServerWS 处理游戏服务器经 WebSocket 接入的鉴权与连接（/api/v1/game-servers/ws）。
//
// 鉴权：query.token 为创建/轮换服务器时一次性下发的「原始 API Secret」；
// 服务端对其计算 SHA256 后与 game_servers.api_secret_hash 比对，并要求 status=1（停用服务器不允许接入）。
// 命中则将 clientID 设为 game_servers.id，供后续按服务器维度路由指令与查询；身份由 secret 派生，不可冒用。
//
// 鉴权失败（secret 错误 / 服务器不存在 / 已停用 / DB 异常）一律返回 401，不向客户端区分具体成因，避免枚举与信息泄露。
// 该路由不再接受自声明的 client_id。
func ServeGameServerWS(hub *Hub, auth GameServerAuth, baseLogger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			baseLogger.Warn("WS 游戏服务器连接鉴权失败：缺少 token",
				zap.String("remote_ip", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		serverID, err := auth.ResolveServerBySecret(r.Context(), token)
		if err != nil {
			// 不区分 secret 错误 / 服务器不存在 / 已停用 / DB 异常，统一 401，避免枚举与信息泄露。
			baseLogger.Warn("WS 游戏服务器连接鉴权失败",
				zap.String("remote_ip", r.RemoteAddr),
				zap.Error(err))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		serveWSConnection(hub, w, r, strconv.FormatInt(serverID, 10), "mc_server", baseLogger)
	}
}

// readPump 负责读取客户端上行的事件，并投递到业务队列。
func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.unregister <- c
		c.closeConn()
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

		// 按接入端类型解析上行消息：adapter 走统一 Envelope，mc_server（鹊桥）为原始 JSON。
		task, ok := c.parseInbound(message)
		if !ok {
			continue
		}

		// 投递到业务队列：绝不在此等待业务完成，避免阻塞读循环与心跳（背压由 EnqueueTimeout 控制）。
		if hub.pool == nil {
			c.logger.Warn("业务队列未初始化，丢弃事件", zap.String("type", task.Type))
			continue
		}
		if err := hub.pool.Enqueue(task); err != nil {
			// 背压溢出或队列已关闭：记录 + 计数，后续可回一个 busy Ack 给客户端做降级。
			c.logger.Warn("事件入队失败",
				zap.String("type", task.Type), zap.Error(err))
		}
	}
}

// parseInbound 根据接入端类型解析上行消息为业务 Task。
// adapter 等共享密钥接入端走统一 Envelope；mc_server（鹊桥）以原始 JSON 直发，按 sub_type 派生 type。
// 返回 (task, false) 表示消息非法/应丢弃。
func (c *Client) parseInbound(message []byte) (queue.Task, bool) {
	if c.ClientType == "mc_server" {
		return c.parseGameServerEvent(message)
	}
	return c.parseEnvelopeEvent(message)
}

// parseEnvelopeEvent 解析统一 Envelope 包装的上行消息（Adapter 等共享密钥接入端）。
func (c *Client) parseEnvelopeEvent(message []byte) (queue.Task, bool) {
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
		ClientID:   c.ClientID,
		Type:       env.Type,
		Payload:    env.Payload,
		ReceivedAt: time.Now(),
	}, true
}

// parseGameServerEvent 解析鹊桥原始 JSON 游戏事件（mc_server）。
// 鹊桥不提供 trace_id，这里生成一次性 trace 以便链路追踪；type 由 sub_type 派生为 event.game.<sub_type>。
func (c *Client) parseGameServerEvent(message []byte) (queue.Task, bool) {
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
		zap.String("sub_type", probe.SubType),
		zap.String("trace_id", traceID))

	return queue.Task{
		TraceID:    traceID,
		ClientID:   c.ClientID,
		Type:       "event.game." + probe.SubType,
		Payload:    message,
		ReceivedAt: time.Now(),
	}, true
}

// writePump 负责向客户端写数据
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second) // 发送心跳
	defer func() {
		ticker.Stop()
		c.closeConn()
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
