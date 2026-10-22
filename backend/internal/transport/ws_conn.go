package transport

import (
	"sync"
	"time"

	"revolcast-backend/internal/queue"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// ClientType 是接入端类型标识。类型由路由决定，客户端不可自行声明（避免冒用其他接入端类型）。
type ClientType string

const (
	// ClientTypeAdapter QQ Adapter（共享密钥接入端），按 client_id 管理连接。
	ClientTypeAdapter ClientType = "adapter"
	// ClientTypeGameServer MC 游戏服务器（鹊桥，per-server API Secret），按 server_code 管理连接。
	// 取值保持 "mc_server"，与既有日志与文档语义一致。
	ClientTypeGameServer ClientType = "mc_server"
	// ClientTypeWeb 网站前端（登录用户，Session Cookie 鉴权），每条连接分配独立 connection_id。
	ClientTypeWeb ClientType = "web"
)

// WebSocket 连接的时间与容量参数（各接入端共用）。
const (
	wsSendQueueSize = 256              // 每连接待发送队列长度，满则丢弃（由业务侧决定是否降级）
	wsWriteWait     = 10 * time.Second // 单次写超时
	wsPongWait      = 60 * time.Second // 读超时：超过该时间未收到任何消息/Pong 视为断连
	wsPingPeriod    = 30 * time.Second // 心跳间隔，须显著小于 wsPongWait
	wsMaxMessageLen = 512 * 1024       // 单条上行消息上限 512KB
)

// Conn 是 Hub 统一管理的连接契约，只包含与协议无关的基础能力：
// 连接标识、接入端类型、非阻塞发送、幂等关闭。
//
// 协议解析与业务处理不在此接口内，由各接入端类型分别实现
// （见 ws_adapter.go / ws_game_server.go / ws_web.go），
// 避免在公共连接结构体中持续堆积类型判断与业务逻辑。
//
// 方法故意保持非导出：连接实现只应存在于本包，防止外部绕过统一的生命周期管理。
type Conn interface {
	// ConnID 返回连接唯一标识：
	// adapter 为 client_id，game_server 为 server_code，web 为随机 connection_id。
	ConnID() string
	// ConnType 返回接入端类型。
	ConnType() ClientType
	// deliver 非阻塞地投递一条待发送数据；连接已关闭或队列已满返回 false。
	deliver(data []byte) bool
	// closeSend 幂等关闭发送通道。
	closeSend()
	// closeConn 幂等关闭底层连接。
	closeConn()
	// logFields 返回该连接的统一日志字段。
	logFields() []zap.Field
}

// connection 是所有接入端共享的 WebSocket 连接实现，负责：
//   - 底层连接与发送队列的生命周期；
//   - 心跳（Ping）与读超时维护；
//   - 发送 / 关闭的并发安全（sendMu + closed + connCloseOnce）。
//
// 上行消息的协议解析通过 readLoop 的回调注入，具体接入端自行决定如何解析、是否入队。
type connection struct {
	id     string
	ctype  ClientType
	conn   *websocket.Conn
	send   chan []byte
	logger *zap.Logger

	// sendMu 保证 deliver（发送）与 closeSend（关闭）互斥，杜绝「向已关闭 channel 发送」的 panic；
	// closed 标记配合 sendMu 实现幂等关闭；
	// connCloseOnce 保护底层连接不被 readPump/writePump/CloseAll/踢旧 并发或重复关闭。
	sendMu        sync.Mutex
	closed        bool
	connCloseOnce sync.Once
}

// newConnection 构造公共连接。logger 应已带上该连接的身份字段。
func newConnection(id string, ctype ClientType, conn *websocket.Conn, logger *zap.Logger) *connection {
	return &connection{
		id:     id,
		ctype:  ctype,
		conn:   conn,
		send:   make(chan []byte, wsSendQueueSize),
		logger: logger,
	}
}

func (c *connection) ConnID() string       { return c.id }
func (c *connection) ConnType() ClientType { return c.ctype }

func (c *connection) logFields() []zap.Field {
	return []zap.Field{
		zap.String("client_id", c.id),
		zap.String("client_type", string(c.ctype)),
	}
}

// deliver 非阻塞地向客户端发送数据；连接已关闭或发送队列已满时返回 false。
// 必须在 sendMu 保护下执行，与 closeSend 互斥，避免向已关闭的 channel 发送引发 panic。
func (c *connection) deliver(data []byte) bool {
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
func (c *connection) closeSend() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}

// closeConn 安全关闭底层连接，幂等；避免多处并发调用 conn.Close()。
func (c *connection) closeConn() {
	c.connCloseOnce.Do(func() { _ = c.conn.Close() })
}

// readLoop 读取上行消息并交给 onMessage 处理，直到连接出错或被关闭。
// 调用方（各接入端的 readPump）负责在返回后向 Hub 注销并关闭连接。
func (c *connection) readLoop(onMessage func(msg []byte)) {
	c.conn.SetReadLimit(wsMaxMessageLen)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Error("WS 读取异常断开", zap.Error(err))
			}
			return
		}
		// 刷新读取超时
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		onMessage(message)
	}
}

// writePump 负责向客户端写数据，并周期性发送 Ping 维持心跳。
func (c *connection) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.closeConn()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				// Hub 关闭了 channel：尽力发送关闭帧
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// enqueueTask 把已解析的上行事件投递到共享业务队列。
// 读循环绝不在此等待业务完成（背压由 Pool 的 EnqueueTimeout 控制）；
// 队列未初始化或入队失败（背压溢出 / 队列已关闭）只记日志，不中断连接。
func enqueueTask(pool *queue.Pool, logger *zap.Logger, task queue.Task) {
	if pool == nil {
		logger.Warn("业务队列未初始化，丢弃事件", zap.String("type", task.Type))
		return
	}
	if err := pool.Enqueue(task); err != nil {
		logger.Warn("事件入队失败", zap.String("type", task.Type), zap.Error(err))
	}
}
