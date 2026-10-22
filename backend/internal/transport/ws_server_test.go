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

// dialTestClient 建立一个真实的 websocket 连接，返回带可用 conn 的 Client，便于测试 closeConn。
func dialTestClient(t *testing.T, botID string) *Client {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// 保持连接：持续读取直到对端关闭
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
	return &Client{
		ClientID: botID,
		conn:     conn,
		send:     make(chan []byte, 16),
		logger:   zap.NewNop(),
	}
}

// TestClient_DeliverCloseRace 回归问题 B：并发 deliver 与 closeSend 不得 panic。
func TestClient_DeliverCloseRace(t *testing.T) {
	c := &Client{send: make(chan []byte, 64), logger: zap.NewNop()}
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

// TestHub_DuplicateClientIDEviction 回归问题 A：同一 ClientID 第二次连接应踢掉旧连接。
func TestHub_DuplicateClientIDEviction(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	c1 := dialTestClient(t, "bot-dup")
	c2 := dialTestClient(t, "bot-dup")

	hub.register <- c1
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.clientsByID["bot-dup"] == c1
	}) {
		t.Fatal("c1 未注册成功")
	}

	hub.register <- c2

	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.clientsByID["bot-dup"] == c2 && !hub.clients[c1]
	}) {
		t.Fatal("重复连接后，映射应指向 c2 且 c1 应从 clients 集合中移除")
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
	err        error
	lastSecret string
}

func (f *fakeGameServerAuth) ResolveServerBySecret(_ context.Context, rawSecret string) (int64, error) {
	f.lastSecret = rawSecret
	if f.err != nil {
		return 0, f.err
	}
	return f.serverID, nil
}

// TestServeGameServerWS_Success 验证：持正确 secret 连接成功，且 clientID=game_servers.id、类型为 mc_server、
// 自报的 client_id 被忽略（身份完全由 secret 派生）。
func TestServeGameServerWS_Success(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeGameServerAuth{serverID: 42}
	srv := httptest.NewServer(ServeGameServerWS(hub, auth, zap.NewNop()))
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
		c, ok := hub.clientsByID["42"]
		return ok && c.ClientType == "mc_server"
	}) {
		t.Fatal("连接应以 \"42\" 注册且类型为 mc_server（忽略自报 client_id）")
	}
	if !waitFor(func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		_, evil := hub.clientsByID["evil-server"]
		return !evil
	}) {
		t.Fatal("不得以客户端自报的 client_id 注册")
	}
}

// TestServeGameServerWS_Unauthorized 验证所有鉴权失败路径统一 401（不区分成因，避免枚举）。
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
			srv := httptest.NewServer(ServeGameServerWS(hub, tc.auth, zap.NewNop()))
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

// TestServeGameServerWS_MissingToken 验证缺少 token 时直接 401，且不触达鉴权层。
func TestServeGameServerWS_MissingToken(t *testing.T) {
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	auth := &fakeGameServerAuth{serverID: 1}
	srv := httptest.NewServer(ServeGameServerWS(hub, auth, zap.NewNop()))
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
