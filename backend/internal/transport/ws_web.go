package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/repository"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// WebSessionAuth 是 Web 端 WebSocket 握手所需的会话鉴权依赖（Cookie 原始 Token → user_id）。
// 由 biz.AuthService 实现：内部统一走 sessionTokenHash，与登录/登出同源。
type WebSessionAuth interface {
	ResolveUserID(ctx context.Context, rawSessionToken string) (int64, error)
}

// WebServerLookup 是 Web 端 WebSocket 握手所需的服务器校验依赖。
// 由 biz.GameServerService 实现：校验 code 存在且 status=1。
type WebServerLookup interface {
	ResolveSubscribableServer(ctx context.Context, code string) (*repository.GameServerRecord, error)
}

// HandshakeRateLimiter 是 WebSocket 握手限流的依赖（Redis 共享计数，多实例间一致）。
type HandshakeRateLimiter interface {
	AllowAttempt(ctx context.Context, key string, window time.Duration, max int64) (bool, error)
}

// WebClient 是网站前端（已登录用户）的连接，只用于「服务器游戏事件」的单向推送。
//
// 标识约定：
//   - connection_id：服务端为每条连接生成的独立标识，也是 Hub 的唯一索引键；
//     Session Token 只用于握手鉴权，绝不作为连接标识（Token 可被撤销，不是稳定标识）。
//   - user_id：握手时由会话解析得到，用于「按用户推送」。
//   - server_code / server_id：订阅目标，推送时只下发该服务器的事件。
//
// 上行消息不进入业务队列：Web 端没有可上报的业务事件，
// 也绝不能误入 Adapter / GameServer 的协议处理流程。
type WebClient struct {
	*connection
	hub        *Hub
	userID     int64
	serverID   int64
	serverCode string
}

// newWebClient 构造 Web 连接。connectionID 由服务端生成，不可由客户端指定。
func newWebClient(hub *Hub, conn *websocket.Conn, userID int64, serverID int64, serverCode string, logger *zap.Logger) *WebClient {
	return &WebClient{
		connection: newConnection(newConnectionID(), ClientTypeWeb, conn, logger),
		hub:        hub,
		userID:     userID,
		serverID:   serverID,
		serverCode: serverCode,
	}
}

// newConnectionID 生成 16 字节随机十六进制连接标识（32 字符）。
// 生成失败时退化为时间戳，仍保证进程内基本唯一（不承载任何安全语义）。
func newConnectionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("conn-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (c *WebClient) logFields() []zap.Field {
	return []zap.Field{
		zap.String("connection_id", c.ConnID()),
		zap.String("client_type", string(ClientTypeWeb)),
		zap.Int64("user_id", c.userID),
		zap.String("server_code", c.serverCode),
	}
}

// start 启动读写协程。读协程退出时向 Hub 注销并关闭底层连接。
func (c *WebClient) start() {
	go c.writePump()
	go c.readPump()
}

func (c *WebClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.closeConn()
	}()
	c.readLoop(c.handleInbound)
}

// handleInbound 忽略 Web 端上行消息（仅保活读循环，用于感知断连与处理 Pong）。
func (c *WebClient) handleInbound(message []byte) {
	c.logger.Debug("忽略 Web 客户端上行消息", zap.Int("bytes", len(message)))
}

// ServeWebWS 处理前端订阅游戏服务器事件的 WebSocket 入口：
// GET /api/v1/game-servers/{server_code}/ws
//
// 握手流程（任一步失败都不升级连接）：
//  1. Origin 校验（会话 Cookie 随请求发送，必须防跨站劫持）；
//  2. 从 Session Cookie 读取会话并验证登录状态，解析出 user_id；
//  3. WebSocket 握手限流（Redis 共享计数，按 user_id 与客户端 IP 双维度，5 分钟窗口）；
//  4. 校验 server_code 存在且启用；
//  5. 升级 WebSocket；
//  6. 创建 Web 连接，关联 user_id / connection_id / server_code；
//  7. 注册到 Hub，此后仅接收该 server_code 的游戏事件推送。
//
// 错误响应使用 HTTP 接口统一信封（§1.1），状态码与业务 code 由 biz 错误注册表映射。
func ServeWebWS(hub *Hub, auth WebSessionAuth, servers WebServerLookup, rl HandshakeRateLimiter, opts WSOptions, baseLogger *zap.Logger) http.HandlerFunc {
	opts = opts.withDefaults()
	checkOrigin := newWebOriginChecker(opts.AllowedOrigins, opts.Production)
	upgrader := newUpgrader(checkOrigin)
	logger := baseLogger.With(zap.String("module", "WSWeb"))

	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Origin：不通过直接拒绝，不读取 Cookie、不触达 DB。
		if !checkOrigin(r) {
			logger.Warn("Web WS 握手 Origin 校验失败",
				zap.String("origin", r.Header.Get("Origin")),
				zap.String("remote_ip", r.RemoteAddr))
			writeJSON(w, http.StatusForbidden, http.StatusForbidden, "origin not allowed", nil)
			return
		}

		// 2. 会话鉴权：未登录 / 会话失效 / DB 异常一律不升级。
		cookie, cerr := r.Cookie(sessionCookieName)
		if cerr != nil || cookie.Value == "" {
			writeBizError(w, biz.ErrNotAuthenticated)
			return
		}
		userID, err := auth.ResolveUserID(r.Context(), cookie.Value)
		if err != nil {
			logErr(logger, "Web WS 会话鉴权失败", err)
			writeBizError(w, err)
			return
		}

		// 3. 握手限流：连接建立后的消息与心跳不计数，仅本次握手计 1 次。
		if !allowHandshake(r.Context(), rl, opts, userID, clientIP(r), logger) {
			logger.Warn("Web WS 握手命中限流",
				zap.Int64("user_id", userID),
				zap.String("remote_ip", r.RemoteAddr))
			writeBizError(w, biz.ErrRateLimited)
			return
		}

		// 4. 订阅目标校验：code 格式、是否存在、是否启用。
		serverCode := r.PathValue("server_code")
		rec, err := servers.ResolveSubscribableServer(r.Context(), serverCode)
		if err != nil {
			logErr(logger, "Web WS 订阅服务器校验失败", err)
			writeBizError(w, err)
			return
		}

		// 5. 升级连接。至此身份与订阅目标均已确定，后续不再读取 HTTP 请求体。
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade 失败时已向客户端写入错误响应（如非 101 状态），此处仅记录。
			logger.Error("Web WS 协议升级失败", zap.Error(err))
			return
		}

		// 6 + 7. 创建并注册 Web 连接。
		client := newWebClient(hub, conn, userID, rec.ID, rec.Code, logger.With(
			zap.Int64("user_id", userID),
			zap.String("server_code", rec.Code),
			zap.String("remote_ip", r.RemoteAddr)))
		hub.register <- client
		client.start()

		logger.Info("Web 客户端已订阅服务器事件",
			zap.String("connection_id", client.ConnID()),
			zap.Int64("user_id", userID),
			zap.String("server_code", rec.Code))
	}
}

// allowHandshake 按 user_id 与客户端 IP 双维度计数，任一维度超限即拒绝本次握手。
// Redis 故障时 fail-open（与既有服务端限流策略一致），仅记录日志，避免基础设施故障锁死用户。
func allowHandshake(ctx context.Context, rl HandshakeRateLimiter, opts WSOptions, userID int64, ip string, logger *zap.Logger) bool {
	checks := []struct {
		key string
		max int64
	}{
		{repository.WSRateLimitKey("handshake_user", strconv.FormatInt(userID, 10)), opts.HandshakeUserMax},
		{repository.WSRateLimitKey("handshake_ip", ip), opts.HandshakeIPMax},
	}
	for _, c := range checks {
		allowed, err := rl.AllowAttempt(ctx, c.key, opts.HandshakeWindow, c.max)
		if err != nil {
			logger.Warn("WS 握手限流计数失败（fail-open）",
				zap.String("key", c.key), zap.Error(err))
		}
		if !allowed {
			return false
		}
	}
	return true
}
