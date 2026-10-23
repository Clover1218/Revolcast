package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"

	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// ---- fakes ----

// fakeWebSessionAuth 是 WebSessionAuth 的测试替身。
type fakeWebSessionAuth struct {
	userID    int64
	err       error
	lastToken string
}

func (f *fakeWebSessionAuth) ResolveUserID(_ context.Context, rawToken string) (int64, error) {
	f.lastToken = rawToken
	if f.err != nil {
		return 0, f.err
	}
	return f.userID, nil
}

// fakeWebServerLookup 是 WebServerLookup 的测试替身。
type fakeWebServerLookup struct {
	rec      *repository.GameServerRecord
	err      error
	lastCode string
}

func (f *fakeWebServerLookup) ResolveSubscribableServer(_ context.Context, code string) (*repository.GameServerRecord, error) {
	f.lastCode = code
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

// fakeHandshakeLimiter 记录被计数的限流键，便于断言限流维度。
type fakeHandshakeLimiter struct {
	allowed bool
	err     error

	mu   sync.Mutex
	keys []string
}

func (f *fakeHandshakeLimiter) AllowAttempt(_ context.Context, key string, _ time.Duration, _ int64) (bool, error) {
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	return f.allowed, f.err
}

func (f *fakeHandshakeLimiter) recordedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// ---- helpers ----

// newWebWSServer 按真实路由模式挂载 Web WS 处理器（PathValue 依赖 mux 解析路径参数）。
func newWebWSServer(t *testing.T, hub *Hub, auth WebSessionAuth, lookup WebServerLookup, rl HandshakeRateLimiter, opts WSOptions) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/game-servers/{server_code}/ws", ServeWebWS(hub, auth, lookup, rl, opts, zap.NewNop()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// dialWebWS 携带会话 Cookie（可选 Origin）连接 Web WS 端点。
func dialWebWS(t *testing.T, srv *httptest.Server, code, cookie, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/game-servers/" + code + "/ws"
	header := http.Header{}
	if cookie != "" {
		header.Set("Cookie", sessionCookieName+"="+cookie)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, resp, err
}

// newEnabledGameServer 返回一个启用中的服务器记录。
func newEnabledGameServer() *repository.GameServerRecord {
	return &repository.GameServerRecord{ID: 10, Code: "survival", Name: "生存服", Status: 1}
}

// ---- 握手鉴权与校验 ----

// TestServeWebWS_NoCookie_Unauthorized 未携带会话 Cookie 时返回 401，且不升级连接。
func TestServeWebWS_NoCookie_Unauthorized(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeWebSessionAuth{userID: 1}
	rl := &fakeHandshakeLimiter{allowed: true}
	srv := newWebWSServer(t, hub, auth, &fakeWebServerLookup{rec: newEnabledGameServer()}, rl, WSOptions{})

	_, resp, err := dialWebWS(t, srv, "survival", "", "")
	if err == nil {
		t.Fatal("未登录不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %+v", resp)
	}
	if auth.lastToken != "" {
		t.Fatal("未携带 Cookie 时不应调用会话鉴权")
	}
	if len(rl.recordedKeys()) != 0 {
		t.Fatal("鉴权失败时不应消耗握手限流配额")
	}
}

// TestServeWebWS_InvalidSession_Unauthorized 会话失效返回 401，且不升级、不消耗限流配额。
func TestServeWebWS_InvalidSession_Unauthorized(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeWebSessionAuth{err: biz.ErrNotAuthenticated}
	rl := &fakeHandshakeLimiter{allowed: true}
	srv := newWebWSServer(t, hub, auth, &fakeWebServerLookup{rec: newEnabledGameServer()}, rl, WSOptions{})

	_, resp, err := dialWebWS(t, srv, "survival", "bad-token", "")
	if err == nil {
		t.Fatal("会话失效不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %+v", resp)
	}
	if len(rl.recordedKeys()) != 0 {
		t.Fatal("鉴权失败时不应消耗握手限流配额")
	}
}

// TestServeWebWS_RateLimited 命中握手限流返回 429。
func TestServeWebWS_RateLimited(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	rl := &fakeHandshakeLimiter{allowed: false}
	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 7}, &fakeWebServerLookup{rec: newEnabledGameServer()}, rl, WSOptions{})

	_, resp, err := dialWebWS(t, srv, "survival", "token", "")
	if err == nil {
		t.Fatal("命中限流不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %+v", resp)
	}
}

// TestServeWebWS_HandshakeRateLimitKeys 握手同时按 user_id 与 IP 两个维度计数，窗口 5 分钟。
func TestServeWebWS_HandshakeRateLimitKeys(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	rl := &fakeHandshakeLimiter{allowed: true}
	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 7}, &fakeWebServerLookup{rec: newEnabledGameServer()}, rl, WSOptions{})

	conn, _, err := dialWebWS(t, srv, "survival", "token", "")
	if err != nil {
		t.Fatalf("应连接成功，实际 %v", err)
	}
	_ = conn

	keys := rl.recordedKeys()
	if len(keys) != 2 {
		t.Fatalf("握手应计 user 与 IP 两个维度，实际 %v", keys)
	}
	if !strings.HasPrefix(keys[0], "revolcast:ratelimit:ws:handshake_user:") {
		t.Fatalf("第一维度应为 user_id 限流，实际 %q", keys[0])
	}
	if !strings.HasPrefix(keys[1], "revolcast:ratelimit:ws:handshake_ip:") {
		t.Fatalf("第二维度应为 IP 限流，实际 %q", keys[1])
	}
}

// TestServeWebWS_ServerNotFound 订阅不存在的服务器返回 404。
func TestServeWebWS_ServerNotFound(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 1},
		&fakeWebServerLookup{err: biz.ErrServerNotFound}, &fakeHandshakeLimiter{allowed: true}, WSOptions{})

	_, resp, err := dialWebWS(t, srv, "missing", "token", "")
	if err == nil {
		t.Fatal("服务器不存在不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %+v", resp)
	}
}

// TestServeWebWS_ServerDisabled 已停用服务器返回 403。
func TestServeWebWS_ServerDisabled(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 1},
		&fakeWebServerLookup{err: biz.ErrServerDisabled}, &fakeHandshakeLimiter{allowed: true}, WSOptions{})

	_, resp, err := dialWebWS(t, srv, "survival", "token", "")
	if err == nil {
		t.Fatal("停用服务器不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %+v", resp)
	}
}

// TestServeWebWS_OriginRejected Origin 不在白名单时返回 403（先于会话与 DB 校验）。
func TestServeWebWS_OriginRejected(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeWebSessionAuth{userID: 1}
	rl := &fakeHandshakeLimiter{allowed: true}
	opts := WSOptions{AllowedOrigins: []string{"https://app.example"}}
	srv := newWebWSServer(t, hub, auth, &fakeWebServerLookup{rec: newEnabledGameServer()}, rl, opts)

	_, resp, err := dialWebWS(t, srv, "survival", "token", "https://evil.example")
	if err == nil {
		t.Fatal("非白名单来源不应连接成功")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %+v", resp)
	}
	if len(rl.recordedKeys()) != 0 {
		t.Fatal("Origin 校验失败时不应消耗限流配额")
	}

	// 白名单内来源可正常连接。
	conn, _, err := dialWebWS(t, srv, "survival", "token", "https://app.example")
	if err != nil {
		t.Fatalf("白名单来源应可连接，实际 %v", err)
	}
	_ = conn
}

// TestServeWebWS_ProductionRequiresWhitelist 生产环境未配置白名单时，即使携带合法 Origin 也拒绝（fail-closed）。
func TestServeWebWS_ProductionRequiresWhitelist(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 1},
		&fakeWebServerLookup{rec: newEnabledGameServer()}, &fakeHandshakeLimiter{allowed: true},
		WSOptions{Production: true})

	_, resp, err := dialWebWS(t, srv, "survival", "token", "https://app.example")
	if err == nil {
		t.Fatal("生产环境缺失白名单时应拒绝所有浏览器来源")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %+v", resp)
	}
}

// ---- 连接注册与事件推送 ----

// TestServeWebWS_Success_AndPush 握手成功后：
//   - 连接以独立 connection_id 注册，并关联 user_id / server_code；
//   - 该服务器的事件可被推送到连接，且消息为统一 Envelope + 转换后 payload。
func TestServeWebWS_Success_AndPush(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	server := newEnabledGameServer()
	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 7},
		&fakeWebServerLookup{rec: server}, &fakeHandshakeLimiter{allowed: true}, WSOptions{})

	conn, _, err := dialWebWS(t, srv, "survival", "token", "")
	if err != nil {
		t.Fatalf("应连接成功，实际 %v", err)
	}

	var client *WebClient
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		for _, c := range hub.webByConn {
			client = c
			return true
		}
		return false
	}) {
		t.Fatal("Web 连接未注册")
	}
	if client.userID != 7 || client.serverCode != "survival" || client.serverID != 10 {
		t.Fatalf("连接身份关联错误: user=%d code=%q id=%d", client.userID, client.serverCode, client.serverID)
	}
	if client.ConnID() == "" || len(client.ConnID()) != 32 {
		t.Fatalf("connection_id 应为服务端生成的 32 字符随机串，实际 %q", client.ConnID())
	}

	// 通过 server_id 定位订阅者并推送一条 chat 事件。
	// ID / Cursor 由 Ingest 从持久化结果产出，这里直接给出契约要求的值（与 HTTP items[] 同源）。
	eventID := primitive.NewObjectID()
	view := &biz.GameEventView{
		ID:         eventID,
		Cursor:     "cursor-1",
		ServerID:   10,
		EventType:  "player_chat",
		Player:     protocol.QueqiaoPlayer{UUID: "uuid-1", Nickname: "Clover", IsOp: false},
		OccurredAt: time.Unix(1792395025, 0).UTC(),
		ReceivedAt: time.Date(2026, 10, 19, 7, 30, 25, 125000000, time.UTC),
		Message:    "hello",
		RawMessage: `{"text":"hello"}`,
		MessageID:  "m1",
	}
	pushGameEventToWeb(hub, "trace-1", view, zap.NewNop())

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("应收到推送消息，实际 %v", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("推送消息应为统一 Envelope: %v", err)
	}
	if env.Version != "v1" || env.Type != "event.game.player_chat" || env.TraceID != "trace-1" {
		t.Fatalf("Envelope 头不符: %+v", env)
	}

	var payload protocol.GameEventPushPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if payload.ServerCode != "survival" || payload.ServerID != 10 {
		t.Fatalf("payload 服务器信息错误: %+v", payload)
	}
	if payload.EventType != "player_chat" || payload.Data.Message != "hello" || payload.Data.MessageID != "m1" {
		t.Fatalf("payload 事件内容错误: %+v", payload)
	}
	if payload.Player.Nickname != "Clover" {
		t.Fatalf("payload 玩家快照错误: %+v", payload.Player)
	}
	// 事件身份必须随推送下发：id 为持久化 _id 的十六进制，cursor 与 HTTP 查询同源。
	if payload.ID != eventID.Hex() || len(payload.ID) != 24 {
		t.Fatalf("payload.id = %q, want %q", payload.ID, eventID.Hex())
	}
	if payload.Cursor != "cursor-1" {
		t.Fatalf("payload.cursor = %q, want cursor-1", payload.Cursor)
	}
	// 时间与 HTTP 查询统一为 UTC RFC3339 固定毫秒格式（不再是 RFC3339Nano）。
	if payload.OccurredAt != "2026-10-19T07:30:25.000Z" || payload.ReceivedAt != "2026-10-19T07:30:25.125Z" {
		t.Fatalf("时间字段应为 UTC RFC3339 毫秒格式: %q / %q", payload.OccurredAt, payload.ReceivedAt)
	}
}

// TestPushGameEventToWeb_NoSubscriber 无订阅者时直接跳过，不阻塞也不报错。
func TestPushGameEventToWeb_NoSubscriber(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	view := &biz.GameEventView{ServerID: 99, EventType: "player_join"}
	pushGameEventToWeb(hub, "trace", view, zap.NewNop()) // 不应 panic / 阻塞
	pushGameEventToWeb(hub, "trace", nil, zap.NewNop())
}

// TestPushGameEventToWeb_DoesNotCrossServersA 推送到 A 服务器的事件不得流向订阅 B 的连接。
func TestPushGameEventToWeb_DoesNotCrossServers(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	a := newTestWebClient(t, hub, 1, 10, "survival")
	b := newTestWebClient(t, hub, 2, 11, "creative")
	hub.register <- a
	hub.register <- b
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.webByConn) == 2
	}) {
		t.Fatal("注册未完成")
	}

	pushGameEventToWeb(hub, "t", &biz.GameEventView{ServerID: 10, EventType: "player_join"}, zap.NewNop())
	if len(a.send) != 1 {
		t.Fatalf("survival 订阅者应收到 1 条，实际 %d", len(a.send))
	}
	if len(b.send) != 0 {
		t.Fatalf("creative 订阅者不应收到 survival 的事件，实际 %d", len(b.send))
	}
}

// TestServeWebWS_MultipleConnectionsPerUser 同一用户可建立多条 Web 连接，均按 user_id 索引。
func TestServeWebWS_MultipleConnectionsPerUser(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	srv := newWebWSServer(t, hub, &fakeWebSessionAuth{userID: 7},
		&fakeWebServerLookup{rec: newEnabledGameServer()}, &fakeHandshakeLimiter{allowed: true}, WSOptions{})

	if _, _, err := dialWebWS(t, srv, "survival", "token", ""); err != nil {
		t.Fatalf("第一条连接失败: %v", err)
	}
	if _, _, err := dialWebWS(t, srv, "survival", "token", ""); err != nil {
		t.Fatalf("第二条连接失败: %v", err)
	}

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.webByUser[7]) == 2
	}) {
		t.Fatal("同一用户的连接应全部挂在 user_id 索引下")
	}
}

// TestBuildGameEventPush_CommandOmitsRawMessage command 事件的 raw_message（Java toString）不应下发给前端。
func TestBuildGameEventPush_CommandOmitsRawMessage(t *testing.T) {
	view := &biz.GameEventView{
		ServerID:   10,
		EventType:  "player_command",
		OccurredAt: time.Unix(1792395025, 0).UTC(),
		ReceivedAt: time.Unix(1792395025, 0).UTC(),
		Command:    "gamemode creative",
		RawMessage: "CommandEvent{...}",
		MessageID:  "m2",
	}
	data, err := buildGameEventPush("t", view, "survival")
	if err != nil {
		t.Fatalf("构造推送失败: %v", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("推送消息应为 Envelope: %v", err)
	}
	var payload protocol.GameEventPushPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if payload.Data.Command != "gamemode creative" {
		t.Fatalf("command 字段错误: %+v", payload.Data)
	}
	if payload.Data.RawMessage != "" {
		t.Fatalf("command 事件不应下发 raw_message，实际 %q", payload.Data.RawMessage)
	}
}
