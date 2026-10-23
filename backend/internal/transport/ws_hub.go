package transport

import (
	"strconv"
	"sync"

	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// Hub 管理所有 WebSocket 连接，并持有全局业务队列。
//
// 三类接入端分别维护自己的索引，避免不同接入端的标识互相冲突：
//   - Adapter：    client_id          -> 连接（同一 client_id 只保留一个有效连接）
//   - GameServer： server_code + server_id -> 连接（同一服务器只保留一个有效连接）
//
// GameServer 同时维护两份索引：server_code 用于「按 code 定位登录态」类日志与展示，
// server_id 用于业务下发（事件的已鉴权归属是 server_id，绑定/解绑通知按它找连接）。
//   - Web：        connection_id      -> 连接，并额外维护 user_id / server_code 两个反向索引
//
// 注意：Web 连接的标识是服务端生成的 connection_id，Session Token 只用于握手鉴权，
// 绝不作为 Map 键（Token 是可被撤销的凭证，不是稳定连接标识）。
type Hub struct {
	pool   *queue.Pool // 业务队列，所有接入端共享（消费者池）
	logger *zap.Logger

	register   chan Conn
	unregister chan Conn
	mu         sync.RWMutex

	adapters    map[string]*AdapterClient    // client_id -> 连接
	gameServers map[string]*GameServerClient // server_code -> 连接
	// gameServersByID 是 game_servers.id -> 连接的索引：业务下行（游戏内私聊通知）只拿得到
	// 已鉴权的 server_id，不能依赖 server_name 反查 code。
	gameServersByID map[int64]*GameServerClient

	webByConn   map[string]*WebClient            // connection_id -> 连接
	webByUser   map[int64]map[string]*WebClient  // user_id -> connection_id -> 连接（按用户推送）
	webByServer map[string]map[string]*WebClient // server_code -> connection_id -> 连接（按服务器推送）

	// webCodeByID 记录「当前存在 Web 订阅者」的 server_id -> server_code 映射：
	// 游戏事件只携带已鉴权的 server_id，推送时需据此定位订阅了该服务器的连接集合。
	// 所有 Web 连接断开后对应条目会被清理，避免无界增长。
	webCodeByID map[int64]string
}

func NewHub(logger *zap.Logger) *Hub {
	return &Hub{
		logger:          logger.With(zap.String("module", "WSHub")),
		register:        make(chan Conn),
		unregister:      make(chan Conn),
		adapters:        make(map[string]*AdapterClient),
		gameServers:     make(map[string]*GameServerClient),
		webByConn:       make(map[string]*WebClient),
		webByUser:       make(map[int64]map[string]*WebClient),
		webByServer:     make(map[string]map[string]*WebClient),
		webCodeByID:     make(map[int64]string),
		gameServersByID: make(map[int64]*GameServerClient),
	}
}

// SetPool 注入业务队列（在 main 中先建 Hub 再建 Pool，由 Pool 的 Handler 反向引用 Hub）。
func (h *Hub) SetPool(p *queue.Pool) { h.pool = p }

// Run 启动 Hub 的事件循环。注册 / 注销都在此单 goroutine 内串行处理，
// 配合 mu 保护读侧（发送、推送）的并发访问。
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.registerConn(c)
		case c := <-h.unregister:
			h.unregisterConn(c)
		}
	}
}

// registerConn 按接入端类型写入对应索引。
// 重复连接处理：Adapter / GameServer 采用「踢旧留新」（新连接接管同一逻辑身份），
// Web 连接每条独立，不做互斥替换。
func (h *Hub) registerConn(c Conn) {
	h.mu.Lock()
	switch v := c.(type) {
	case *AdapterClient:
		if old, exists := h.adapters[v.ConnID()]; exists && old != v {
			h.logger.Warn("检测到重复 client_id 连接，踢掉旧连接", v.logFields()...)
			h.evictLocked(old)
		}
		h.adapters[v.ConnID()] = v
	case *GameServerClient:
		if old, exists := h.gameServers[v.serverCode]; exists && old != v {
			h.logger.Warn("检测到同一服务器重复连接，踢掉旧连接", v.logFields()...)
			h.evictLocked(old)
		}
		h.gameServers[v.serverCode] = v
		h.gameServersByID[v.serverID] = v
	case *WebClient:
		h.addWebLocked(v)
	default:
		h.mu.Unlock()
		h.logger.Error("未知的接入端类型，拒绝注册", zap.String("client_type", string(c.ConnType())))
		return
	}
	h.mu.Unlock()

	h.logger.Info("客户端已连接", c.logFields()...)
}

// unregisterConn 从索引中移除连接（幂等）。
// 旧连接被踢掉后其 readPump 仍会触发一次注销，因此必须做身份比对，
// 避免误删「已接管同一标识的新连接」。
func (h *Hub) unregisterConn(c Conn) {
	h.mu.Lock()
	switch v := c.(type) {
	case *AdapterClient:
		if h.adapters[v.ConnID()] == v {
			delete(h.adapters, v.ConnID())
		}
	case *GameServerClient:
		if h.gameServers[v.serverCode] == v {
			delete(h.gameServers, v.serverCode)
		}
		if h.gameServersByID[v.serverID] == v {
			delete(h.gameServersByID, v.serverID)
		}
	case *WebClient:
		h.removeWebLocked(v)
	}
	h.mu.Unlock()

	c.closeSend()
	h.logger.Info("客户端已断开", c.logFields()...)
}

// addWebLocked 建立 Web 连接的三份索引（调用方须持有 h.mu 写锁）。
func (h *Hub) addWebLocked(c *WebClient) {
	h.webByConn[c.ConnID()] = c

	if h.webByUser[c.userID] == nil {
		h.webByUser[c.userID] = make(map[string]*WebClient)
	}
	h.webByUser[c.userID][c.ConnID()] = c

	if h.webByServer[c.serverCode] == nil {
		h.webByServer[c.serverCode] = make(map[string]*WebClient)
	}
	h.webByServer[c.serverCode][c.ConnID()] = c

	h.webCodeByID[c.serverID] = c.serverCode
}

// removeWebLocked 清除 Web 连接的三份索引，并在该服务器已无订阅者时清理 server_id 映射。
func (h *Hub) removeWebLocked(c *WebClient) {
	if h.webByConn[c.ConnID()] == c {
		delete(h.webByConn, c.ConnID())
	}

	if set := h.webByUser[c.userID]; set != nil {
		if set[c.ConnID()] == c {
			delete(set, c.ConnID())
			if len(set) == 0 {
				delete(h.webByUser, c.userID)
			}
		}
	}

	if set := h.webByServer[c.serverCode]; set != nil {
		if set[c.ConnID()] == c {
			delete(set, c.ConnID())
			if len(set) == 0 {
				delete(h.webByServer, c.serverCode)
				// 该服务器已无 Web 订阅者，推送不再需要 server_id 映射。
				if h.webCodeByID[c.serverID] == c.serverCode {
					delete(h.webCodeByID, c.serverID)
				}
			}
		}
	}
}

// evictLocked 踢掉被接管 / 已失效的旧连接（调用方须持有 h.mu 写锁）。
// 只关闭连接与发送通道，不复用 unregister（其索引删除由旧连接自己的注销流程负责）。
func (h *Hub) evictLocked(c Conn) {
	c.closeConn()
	c.closeSend()
}

// SendToClient 向指定 client_id 的接入端回传 Action / 指令（非阻塞）。
// 返回 false 表示该接入端不在线或发送队列已满；调用方（Worker）不应因此阻塞。
//
// 查找顺序为 Adapter -> GameServer。游戏服务器的业务标识是 game_servers.id
// （解析为十进制整数后按 id 索引查找），不能直接用它匹配 server_code 键。
func (h *Hub) SendToClient(clientID string, data []byte) bool {
	if h.SendToAdapter(clientID, data) {
		return true
	}
	if id, err := strconv.ParseInt(clientID, 10, 64); err == nil {
		return h.SendToGameServerByID(id, data)
	}
	return false
}

// SendToGameServerByID 向指定游戏服务器（game_servers.id）下发数据（非阻塞）。
//
// 这是游戏内下行通知的唯一出口（见 ws_push / game_notice）：业务侧只持有事件的已鉴权
// server_id，不持有 server_code，也不应通过 server_name 反查。
// 返回 false 表示该服务器当前无在线连接或发送队列已满，调用方只记日志、不重试。
func (h *Hub) SendToGameServerByID(serverID int64, data []byte) bool {
	h.mu.RLock()
	gs, ok := h.gameServersByID[serverID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	return gs.deliver(data)
}

// SendToAdapter 向指定 client_id 的 Adapter 回传指令（非阻塞）。
func (h *Hub) SendToAdapter(clientID string, data []byte) bool {
	h.mu.RLock()
	c, ok := h.adapters[clientID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	return c.deliver(data)
}

// SendToUser 向某个登录用户的全部 Web 连接推送数据（非阻塞），返回成功入队数量。
// 供后续「按用户推送」的业务（如个人通知）使用。
func (h *Hub) SendToUser(userID int64, data []byte) int {
	h.mu.RLock()
	conns := make([]*WebClient, 0, len(h.webByUser[userID]))
	for _, c := range h.webByUser[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	delivered := 0
	for _, c := range conns {
		if c.deliver(data) {
			delivered++
		}
	}
	return delivered
}

// WebServerCode 返回 server_id 对应的 server_code（仅当存在该服务器的 Web 订阅者时 ok=true）。
// 游戏事件只携带已鉴权的 server_id，推送前需借此定位订阅集合。
func (h *Hub) WebServerCode(serverID int64) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	code, ok := h.webCodeByID[serverID]
	return code, ok
}

// PushToWeb 向订阅指定 server_code 的全部 Web 客户端推送数据（非阻塞），返回成功入队数量。
// 不广播给其他服务器的订阅者；单个连接队列满时只丢弃该连接，不影响其他连接。
func (h *Hub) PushToWeb(serverCode string, data []byte) int {
	h.mu.RLock()
	conns := make([]*WebClient, 0, len(h.webByServer[serverCode]))
	for _, c := range h.webByServer[serverCode] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	delivered := 0
	for _, c := range conns {
		if c.deliver(data) {
			delivered++
		}
	}
	return delivered
}

// CloseAll 关闭所有接入端连接，使 readPump 退出、停止产生新任务。
// 用于优雅关闭阶段，先于 pool.Close 执行。
func (h *Hub) CloseAll() {
	h.mu.Lock()
	for _, c := range h.adapters {
		c.closeConn()
		c.closeSend()
	}
	for _, c := range h.gameServers {
		c.closeConn()
		c.closeSend()
	}
	for _, c := range h.webByConn {
		c.closeConn()
		c.closeSend()
	}
	h.adapters = make(map[string]*AdapterClient)
	h.gameServers = make(map[string]*GameServerClient)
	h.gameServersByID = make(map[int64]*GameServerClient)
	h.webByConn = make(map[string]*WebClient)
	h.webByUser = make(map[int64]map[string]*WebClient)
	h.webByServer = make(map[string]map[string]*WebClient)
	h.webCodeByID = make(map[int64]string)
	h.mu.Unlock()
}
