package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"revolcast-backend/internal/biz"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// dialRawConn 建立一个真实的 websocket 连接，返回客户端侧 conn。
// 服务端侧只负责保活读取，便于测试 closeConn / 踢旧等生命周期行为。
func dialRawConn(t *testing.T) *websocket.Conn {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, e := c.ReadMessage(); e != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newTestAdapterClient 构造一个带真实 conn 的 Adapter 连接（不启动读写协程）。
func newTestAdapterClient(t *testing.T, hub *Hub, clientID string) *AdapterClient {
	t.Helper()
	return newAdapterClient(hub, dialRawConn(t), clientID, zap.NewNop())
}

// newTestGameServerClient 构造一个带真实 conn 的游戏服务器连接（不启动读写协程）。
func newTestGameServerClient(t *testing.T, hub *Hub, serverID int64, code string) *GameServerClient {
	t.Helper()
	return newGameServerClient(hub, dialRawConn(t), serverID, code, zap.NewNop())
}

// newTestWebClient 构造一个带真实 conn 的 Web 连接（不启动读写协程）。
func newTestWebClient(t *testing.T, hub *Hub, userID int64, serverID int64, code string) *WebClient {
	t.Helper()
	return newWebClient(hub, dialRawConn(t), userID, serverID, code, zap.NewNop())
}

// TestConnection_DeliverCloseRace 并发 deliver 与 closeSend 不得 panic（回归问题 B）。
func TestConnection_DeliverCloseRace(t *testing.T) {
	c := &connection{id: "x", send: make(chan []byte, 64), logger: zap.NewNop()}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.deliver([]byte("x"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		c.closeSend()
	}()
	wg.Wait()

	if c.deliver([]byte("x")) {
		t.Fatal("closeSend 后 deliver 应返回 false")
	}
}

// TestHub_DuplicateAdapterEviction 同一 client_id 第二次连接应踢掉旧连接（回归问题 A）。
func TestHub_DuplicateAdapterEviction(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	c1 := newTestAdapterClient(t, hub, "bot-dup")
	c2 := newTestAdapterClient(t, hub, "bot-dup")

	hub.register <- c1
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.adapters["bot-dup"] == c1
	}) {
		t.Fatal("c1 未注册成功")
	}

	hub.register <- c2
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.adapters["bot-dup"] == c2 && hub.adapters["bot-dup"] != c1
	}) {
		t.Fatal("重复连接后，适配器索引应指向 c2")
	}

	// 旧连接随后触发注销不得删除新连接（身份比对保护）。
	hub.unregister <- c1
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.adapters["bot-dup"] == c2
	}) {
		t.Fatal("旧连接注销误删了新连接")
	}
}

// TestHub_DuplicateGameServerEviction 同一 server_code 的第二次连接应接管旧连接。
func TestHub_DuplicateGameServerEviction(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	c1 := newTestGameServerClient(t, hub, 42, "survival")
	c2 := newTestGameServerClient(t, hub, 42, "survival")

	hub.register <- c1
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.gameServers["survival"] == c1
	}) {
		t.Fatal("c1 未按 server_code 注册")
	}

	hub.register <- c2
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.gameServers["survival"] == c2
	}) {
		t.Fatal("重复连接后，游戏服务器索引应指向 c2")
	}
}

// TestHub_AdapterAndGameServerIndexesIsolated 两类接入端标识即使字符串相同也互不冲突。
func TestHub_AdapterAndGameServerIndexesIsolated(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	adapter := newTestAdapterClient(t, hub, "survival") // 与服务器 code 同名
	server := newTestGameServerClient(t, hub, 7, "survival")

	hub.register <- adapter
	hub.register <- server

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.adapters["survival"] == adapter && hub.gameServers["survival"] == server
	}) {
		t.Fatal("Adapter 与 GameServer 索引应各自独立、互不覆盖")
	}
}

// TestHub_SendToClient_PrefersAdapter Adapter 与 GameServer 同名时，指令优先回传 Adapter。
func TestHub_SendToClient_PrefersAdapter(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	hub.register <- newTestAdapterClient(t, hub, "42")
	hub.register <- newTestGameServerClient(t, hub, 42, "survival")

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.adapters["42"] != nil
	}) {
		t.Fatal("注册未完成")
	}
	if !hub.SendToClient("42", []byte("cmd")) {
		t.Fatal("应成功投递给 Adapter")
	}
	if hub.SendToAdapter("survival", []byte("cmd")) {
		t.Fatal("SendToAdapter 不应命中游戏服务器索引")
	}
}

// TestHub_PushToWeb_OnlySubscribedServer 推送只发给订阅同一 server_code 的连接，不广播。
func TestHub_PushToWeb_OnlySubscribedServer(t *testing.T) {
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
		return len(hub.webByConn) == 2 && len(hub.webByServer["survival"]) == 1
	}) {
		t.Fatal("Web 连接未注册完成")
	}

	if n := hub.PushToWeb("survival", []byte("evt")); n != 1 {
		t.Fatalf("应只推送给 survival 的 1 个订阅者，实际 %d", n)
	}
	// creative 的订阅者不应收到 survival 的事件（读取 a/b 的队列长度判断）。
	if got := len(b.send); got != 0 {
		t.Fatalf("其他服务器订阅者不应收到事件，实际队列长度 %d", got)
	}
	if got := len(a.send); got != 1 {
		t.Fatalf("订阅者应收到 1 条事件，实际队列长度 %d", got)
	}
}

// TestHub_WebServerCode 订阅存在时可按 server_id 反查 server_code，全部断开后映射被清理。
func TestHub_WebServerCode(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	if _, ok := hub.WebServerCode(10); ok {
		t.Fatal("无订阅者时不应返回 server_code")
	}

	c := newTestWebClient(t, hub, 1, 10, "survival")
	hub.register <- c
	if !waitFor(func() bool {
		code, ok := hub.WebServerCode(10)
		return ok && code == "survival"
	}) {
		t.Fatal("订阅后应能按 server_id 反查 server_code")
	}

	hub.unregister <- c
	if !waitFor(func() bool {
		_, ok := hub.WebServerCode(10)
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return !ok && len(hub.webCodeByID) == 0
	}) {
		t.Fatal("最后一个订阅者断开后应清理 server_id -> server_code 映射")
	}
}

// TestHub_SendToUser 同一用户的多条 Web 连接都应收到按用户推送的消息。
func TestHub_SendToUser(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	c1 := newTestWebClient(t, hub, 9, 10, "survival")
	c2 := newTestWebClient(t, hub, 9, 11, "creative")
	other := newTestWebClient(t, hub, 10, 10, "survival")
	hub.register <- c1
	hub.register <- c2
	hub.register <- other

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.webByUser[9]) == 2 && len(hub.webByUser[10]) == 1
	}) {
		t.Fatal("按 user_id 索引未建立")
	}

	if n := hub.SendToUser(9, []byte("notice")); n != 2 {
		t.Fatalf("应推送到该用户的 2 条连接，实际 %d", n)
	}
	if got := len(other.send); got != 0 {
		t.Fatalf("其他用户不应收到消息，实际 %d", got)
	}
}

// TestHub_WebMultipleConnectionsPerUser Web 端同一用户的多条连接并存，不做互斥替换。
func TestHub_WebMultipleConnectionsPerUser(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	c1 := newTestWebClient(t, hub, 1, 10, "survival")
	c2 := newTestWebClient(t, hub, 1, 10, "survival")
	hub.register <- c1
	hub.register <- c2

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.webByConn) == 2 && len(hub.webByServer["survival"]) == 2
	}) {
		t.Fatal("同一用户的重复连接应并存（不同 connection_id）")
	}
	if c1.ConnID() == c2.ConnID() {
		t.Fatal("每条 Web 连接必须拥有独立 connection_id")
	}
}

// waitFor 轮询条件直到超时（2s），用于等待 Hub 单 goroutine 处理 channel 事件。
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ---- /api/v1/game-servers/ws 鉴权 ----

// fakeGameServerAuth 是 GameServerAuth 的测试替身。
type fakeGameServerAuth struct {
	serverID   int64
	serverCode string
	err        error
	lastSecret string
}

func (f *fakeGameServerAuth) ResolveServerBySecret(_ context.Context, rawSecret string) (int64, string, error) {
	f.lastSecret = rawSecret
	if f.err != nil {
		return 0, "", f.err
	}
	return f.serverID, f.serverCode, nil
}

// TestServeGameServerWS_Success 验证：持正确 secret 连接成功，连接按 server_code 注册、
// 类型为 mc_server，自报的 client_id 被忽略（身份完全由 secret 派生）。
func TestServeGameServerWS_Success(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeGameServerAuth{serverID: 42, serverCode: "survival"}
	srv := httptest.NewServer(ServeGameServerWS(hub, auth, WSOptions{}, zap.NewNop()))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "?token=raw-secret&client_id=evil-server"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("正确 secret 应连接成功，实际 %v", err)
	}
	defer conn.Close()

	if auth.lastSecret != "raw-secret" {
		t.Fatalf("应向鉴权层传原始 secret，实际 %q", auth.lastSecret)
	}
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		c, ok := hub.gameServers["survival"]
		return ok && c.ConnType() == ClientTypeGameServer && c.serverID == 42
	}) {
		t.Fatal("连接应以 server_code=survival 注册且类型为 mc_server")
	}
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		_, evil := hub.gameServers["evil-server"]
		return !evil
	}) {
		t.Fatal("不得以客户端自报的 client_id 注册")
	}
}

// TestServeGameServerWS_Unauthorized 所有鉴权失败路径统一 401（不区分成因，避免枚举）。
func TestServeGameServerWS_Unauthorized(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	cases := []struct {
		name string
		auth *fakeGameServerAuth
	}{
		{"secret 错误/不存在/已停用", &fakeGameServerAuth{err: fmt.Errorf("fake: %w", biz.ErrServerNotFound)}},
		{"仓库内部错误", &fakeGameServerAuth{err: fmt.Errorf("fake: %w", biz.ErrInternal)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(ServeGameServerWS(hub, tc.auth, WSOptions{}, zap.NewNop()))
			defer srv.Close()

			resp, err := http.Get(srv.URL + "?token=wrong")
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("期望 401，实际 %d", resp.StatusCode)
			}
		})
	}
}

// TestServeGameServerWS_MissingToken 缺少 token 时直接 401，且不触达鉴权层。
func TestServeGameServerWS_MissingToken(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeGameServerAuth{serverID: 1, serverCode: "s"}
	srv := httptest.NewServer(ServeGameServerWS(hub, auth, WSOptions{}, zap.NewNop()))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", resp.StatusCode)
	}
	if auth.lastSecret != "" {
		t.Fatalf("缺少 token 时不应调用鉴权层，实际收到 %q", auth.lastSecret)
	}
}

// TestServeWS_OriginPolicy 生产环境未配置白名单时，携带 Origin 的浏览器连接必须被拒绝（fail-closed）。
func TestServeWS_OriginPolicy(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	opts := WSOptions{Token: "t", Production: true}
	srv := httptest.NewServer(ServeWS(hub, opts, zap.NewNop(), string(ClientTypeAdapter)))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "?token=t&client_id=a"
	header := http.Header{"Origin": []string{"https://evil.example"}}
	if _, _, err := websocket.DefaultDialer.Dial(wsURL, header); err == nil {
		t.Fatal("生产环境未配置白名单时，浏览器来源应被拒绝")
	}

	// 服务端客户端（无 Origin）仍可接入。
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("无 Origin 的服务端客户端应可接入，实际 %v", err)
	}
	defer conn.Close()
}
