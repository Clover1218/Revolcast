package transport

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// WebSocket 握手限流默认值：5 分钟窗口内「每用户 20 次 / 每 IP 60 次」。
// 只在握手阶段计数，连接建立后的消息传输与心跳不计入；
// 前端断线重连会重新握手，因此天然纳入限流。
const (
	wsHandshakeWindow  = 5 * time.Minute
	wsHandshakeUserMax = 20
	wsHandshakeIPMax   = 60
)

// WSOptions 是 WebSocket 接入的统一配置（由 main 从 config.Config 转换而来）。
type WSOptions struct {
	// Token 是 Adapter 共享密钥（仅 /api/v1/adapter/ws 使用）。
	Token string
	// AllowedOrigins 是浏览器来源白名单（逗号分隔配置项解析后的列表）。
	AllowedOrigins []string
	// Production 为 true 时，白名单缺失将拒绝一切浏览器来源（fail-closed），
	// 避免生产环境无条件接受任意 Origin。
	Production bool
	// 握手限流参数，零值取默认（5 分钟 / 20 次 / 60 次）。
	HandshakeWindow  time.Duration
	HandshakeUserMax int64
	HandshakeIPMax   int64
	// StatusPollInterval / StatusPollTimeout 是游戏服务器在线状态采集的周期与单次回执超时，
	// 零值取默认（60s / 5s）。仅 /api/v1/game-servers/ws 使用，见 ws_game_server_status.go。
	StatusPollInterval time.Duration
	StatusPollTimeout  time.Duration
}

// withDefaults 为未配置的限流参数填默认值。
func (o WSOptions) withDefaults() WSOptions {
	if o.HandshakeWindow <= 0 {
		o.HandshakeWindow = wsHandshakeWindow
	}
	if o.HandshakeUserMax <= 0 {
		o.HandshakeUserMax = wsHandshakeUserMax
	}
	if o.HandshakeIPMax <= 0 {
		o.HandshakeIPMax = wsHandshakeIPMax
	}
	if o.StatusPollInterval <= 0 {
		o.StatusPollInterval = defaultStatusPollInterval
	}
	if o.StatusPollTimeout <= 0 {
		o.StatusPollTimeout = defaultStatusPollTimeout
	}
	return o
}

// newUpgrader 为具体接入端构造 Upgrader。Origin 校验策略由调用方按接入端类型选择，
// 不再使用「无条件接受所有来源」的全局 Upgrader。
func newUpgrader(checkOrigin func(*http.Request) bool) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     checkOrigin,
	}
}

// normalizeOrigins 归一化白名单：去空白、统一小写、去掉末尾斜杠（Origin 不带路径）。
func normalizeOrigins(allowed []string) map[string]struct{} {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		key := normalizeOrigin(o)
		if key != "" {
			set[key] = struct{}{}
		}
	}
	return set
}

func normalizeOrigin(origin string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(origin), "/"))
}

// newWebOriginChecker 返回 Web 端（浏览器 + 会话 Cookie）的 Origin 校验函数。
//
// 策略：
//  1. 配置了白名单：必须携带 Origin 且精确命中（大小写与末尾斜杠已归一化）。
//     浏览器建立 WebSocket 必然携带 Origin，因此缺省一律拒绝，避免绕过同源策略。
//  2. 未配置白名单：开发环境放行（便于本地联调）；生产环境拒绝所有来源（fail-closed）。
func newWebOriginChecker(allowed []string, production bool) func(*http.Request) bool {
	set := normalizeOrigins(allowed)
	return func(r *http.Request) bool {
		origin := normalizeOrigin(r.Header.Get("Origin"))
		if len(set) == 0 {
			return !production
		}
		if origin == "" {
			return false
		}
		_, ok := set[origin]
		return ok
	}
}

// newServerOriginChecker 返回服务端接入端（Adapter / GameServer）的 Origin 校验函数。
//
// 这类客户端由服务端程序发起，通常不带 Origin，携带 Cookie 的风险不存在，
// 因此无 Origin 时直接放行；一旦携带 Origin（例如被浏览器页面直接连接），
// 则与 Web 端使用同一套白名单策略。
func newServerOriginChecker(allowed []string, production bool) func(*http.Request) bool {
	set := normalizeOrigins(allowed)
	return func(r *http.Request) bool {
		origin := normalizeOrigin(r.Header.Get("Origin"))
		if origin == "" {
			return true
		}
		if len(set) == 0 {
			return !production
		}
		_, ok := set[origin]
		return ok
	}
}

// GameServerAuth 是 /api/v1/game-servers/ws 接入鉴权所需的最小接口（依赖倒置，便于测试注入 fake）。
type GameServerAuth interface {
	// ResolveServerBySecret 以原始 API Secret 解析出 game_servers.id 与 code；鉴权失败返回 error。
	ResolveServerBySecret(ctx context.Context, rawSecret string) (serverID int64, serverCode string, err error)
}

// ServeWS 处理 HTTP 升级为 WS 的入口（供 QQ Adapter 等共享密钥接入端）。
//
// clientType 由路由决定（/api/v1/adapter/ws => adapter），客户端不可自行声明类型，从而避免冒用。
// 鉴权：query.token 比对共享密钥；query.client_id 为自声明全局身份（用于回传路由与去重）。
func ServeWS(hub *Hub, opts WSOptions, baseLogger *zap.Logger, clientType string) http.HandlerFunc {
	opts = opts.withDefaults()
	upgrader := newUpgrader(newServerOriginChecker(opts.AllowedOrigins, opts.Production))
	logger := baseLogger.With(zap.String("module", "WSAdapter"))

	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		clientID := r.URL.Query().Get("client_id")

		if token != opts.Token {
			logger.Warn("WS 连接鉴权失败，Token 错误",
				zap.String("client_type", clientType), zap.String("remote_ip", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if clientID == "" {
			http.Error(w, "Missing client_id", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("WS 协议升级失败", zap.Error(err))
			return
		}

		client := newAdapterClient(hub, conn, clientID, logger.With(
			zap.String("client_id", clientID),
			zap.String("client_type", clientType),
			zap.String("remote_ip", r.RemoteAddr)))
		hub.register <- client
		client.start()
	}
}

// ServeGameServerWS 处理游戏服务器经 WebSocket 接入的鉴权与连接（/api/v1/game-servers/ws）。
//
// 鉴权：query.token 为创建/轮换服务器时一次性下发的「原始 API Secret」；
// 服务端对其计算 SHA256 后与 game_servers.api_secret_hash 比对，并要求 status=1（停用服务器不允许接入）。
// 命中则连接标识为 server_code、业务归属为 game_servers.id；身份由 secret 派生，不可冒用。
//
// 鉴权失败（secret 错误 / 服务器不存在 / 已停用 / DB 异常）一律返回 401，不向客户端区分具体成因，避免枚举与信息泄露。
// 该路由不接受自声明的 client_id。
//
// statusSink 为在线状态采集的落地端口（BIZ-012）；传 nil 则该连接不采集状态（便于单测）。
func ServeGameServerWS(hub *Hub, auth GameServerAuth, statusSink ServerStatusSink, opts WSOptions, baseLogger *zap.Logger) http.HandlerFunc {
	opts = opts.withDefaults()
	upgrader := newUpgrader(newServerOriginChecker(opts.AllowedOrigins, opts.Production))
	logger := baseLogger.With(zap.String("module", "WSGameServer"))

	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			logger.Warn("WS 游戏服务器连接鉴权失败：缺少 token", zap.String("remote_ip", r.RemoteAddr))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		serverID, serverCode, err := auth.ResolveServerBySecret(r.Context(), token)
		if err != nil {
			// 不区分 secret 错误 / 服务器不存在 / 已停用 / DB 异常，统一 401，避免枚举与信息泄露。
			logger.Warn("WS 游戏服务器连接鉴权失败",
				zap.String("remote_ip", r.RemoteAddr),
				zap.Error(err))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("WS 协议升级失败", zap.Error(err))
			return
		}

		client := newGameServerClient(hub, conn, serverID, serverCode, logger.With(
			zap.String("client_id", serverCode),
			zap.String("client_type", string(ClientTypeGameServer)),
			zap.Int64("server_id", serverID),
			zap.String("remote_ip", r.RemoteAddr)))
		client.enableStatusPolling(statusSink, opts.StatusPollInterval, opts.StatusPollTimeout)
		hub.register <- client
		client.start()
	}
}
