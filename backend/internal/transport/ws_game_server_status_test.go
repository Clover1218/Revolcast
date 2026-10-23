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

	"revolcast-backend/internal/protocol"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const statusTestEchoText = "There are 1 of a max of 20 players online: " +
	"Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058)"

// fakeStatusSink 记录采集结果与清理调用，用 channel 做同步，避免 sleep 轮询。
type fakeStatusSink struct {
	mu       sync.Mutex
	records  []statusRecordCall
	cleared  []string
	recordCh chan statusRecordCall
	clearCh  chan string
	recErr   error
}

type statusRecordCall struct {
	serverID int64
	code     string
	raw      string
}

func newFakeStatusSink() *fakeStatusSink {
	return &fakeStatusSink{
		recordCh: make(chan statusRecordCall, 8),
		clearCh:  make(chan string, 8),
	}
}

func (f *fakeStatusSink) RecordListUUIDs(_ context.Context, serverID int64, serverCode, rawEcho string) error {
	if f.recErr != nil {
		return f.recErr
	}
	call := statusRecordCall{serverID: serverID, code: serverCode, raw: rawEcho}
	f.mu.Lock()
	f.records = append(f.records, call)
	f.mu.Unlock()
	f.recordCh <- call
	return nil
}

func (f *fakeStatusSink) ClearStatus(_ context.Context, serverCode string) error {
	f.mu.Lock()
	f.cleared = append(f.cleared, serverCode)
	f.mu.Unlock()
	f.clearCh <- serverCode
	return nil
}

// dialScriptedGameServer 启动一个会应答 RCON 命令的假鹊桥，返回客户端侧连接与收到的命令通道。
func dialScriptedGameServer(t *testing.T, replyData string) (*websocket.Conn, chan protocol.GameRCONCommandRequest) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	reqs := make(chan protocol.GameRCONCommandRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, rErr := c.ReadMessage()
			if rErr != nil {
				return
			}
			var req protocol.GameRCONCommandRequest
			if json.Unmarshal(msg, &req) != nil || req.API != protocol.GameAPISendRCONCommand {
				continue
			}
			reqs <- req
			resp, _ := json.Marshal(map[string]any{
				"code":      200,
				"api":       req.API,
				"post_type": protocol.GameServerPostTypeResponse,
				"status":    protocol.GameServerStatusSuccess,
				"message":   "success",
				"data":      replyData,
				"echo":      req.Echo,
			})
			if err := c.WriteMessage(websocket.TextMessage, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, reqs
}

// dialSilentGameServer 启动一个只收不回应的假鹊桥，用于超时 / 关闭唤醒测试。
func dialSilentGameServer(t *testing.T) *websocket.Conn {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, rErr := c.ReadMessage(); rErr != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestStatusPoller_CollectsThenClearsOnStop 验证采集闭环：
// 等满一个采集周期后下发 list uuids → 回执经 echo 关联交给 sink → 停止连接时清理快照。
func TestStatusPoller_CollectsThenClearsOnStop(t *testing.T) {
	conn, reqs := dialScriptedGameServer(t, statusTestEchoText)

	c := newGameServerClient(nil, conn, 7, "element", zap.NewNop())
	sink := newFakeStatusSink()
	c.enableStatusPolling(sink, 50*time.Millisecond, time.Second)
	go c.writePump()
	go c.readLoop(c.handleInbound) // 只启动读循环（不启动 readPump，避免依赖 Hub 注销）
	c.startStatusPoller()

	select {
	case req := <-reqs:
		if req.Data.Command != protocol.GameRCONListUUIDs {
			t.Fatalf("下发的命令应为 %q，实际 %q", protocol.GameRCONListUUIDs, req.Data.Command)
		}
		if !strings.HasPrefix(req.Echo, "rcon:") {
			t.Fatalf("echo 应以 rcon: 开头，实际 %q", req.Echo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 RCON 请求")
	}

	select {
	case call := <-sink.recordCh:
		if call.serverID != 7 || call.code != "element" {
			t.Fatalf("归属字段错误: %+v", call)
		}
		if call.raw != statusTestEchoText {
			t.Fatalf("回显不应被改写: %q", call.raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("采集结果未落地")
	}

	c.stop()
	select {
	case code := <-sink.clearCh:
		if code != "element" {
			t.Fatalf("清理的 code 错误: %q", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("停止后应清理状态快照")
	}
}

// TestStatusPoller_WaitsOneIntervalBeforeFirstCollect 首次采集必须等满一个周期，
// 连接建立瞬间不得下发（服务器刚接入时 RCON 往往尚未就绪）。
func TestStatusPoller_WaitsOneIntervalBeforeFirstCollect(t *testing.T) {
	conn, reqs := dialScriptedGameServer(t, statusTestEchoText)

	c := newGameServerClient(nil, conn, 7, "element", zap.NewNop())
	sink := newFakeStatusSink()
	const interval = 600 * time.Millisecond
	c.enableStatusPolling(sink, interval, time.Second)
	go c.writePump()
	go c.readLoop(c.handleInbound)
	t.Cleanup(c.stop)

	started := time.Now()
	c.startStatusPoller()

	// 半个周期内必须没有任何下行。
	select {
	case req := <-reqs:
		t.Fatalf("首个周期内不应下发 RCON，%.0fms 就收到了 %q",
			float64(time.Since(started).Milliseconds()), req.Data.Command)
	case <-time.After(interval / 2):
	}

	// 一个周期后必须发出首次采集。
	select {
	case req := <-reqs:
		if req.Data.Command != protocol.GameRCONListUUIDs {
			t.Fatalf("首次下发命令应为 %q，实际 %q", protocol.GameRCONListUUIDs, req.Data.Command)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("一个周期后仍未下发首次 RCON")
	}
}

// TestRequestRCON_Timeout 无回执时按超时失败，不阻塞采集协程。
func TestRequestRCON_Timeout(t *testing.T) {
	c := newGameServerClient(nil, dialSilentGameServer(t), 7, "element", zap.NewNop())
	go c.writePump()

	start := time.Now()
	if _, err := c.requestRCON(protocol.GameRCONListUUIDs, 200*time.Millisecond); err == nil {
		t.Fatal("无回执应返回 error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效，耗时 %s", elapsed)
	}
}

// TestRequestRCON_EmptyEchoIsFailure 回执 data 为空（RCON 未启用等）视为采集失败。
func TestRequestRCON_EmptyEchoIsFailure(t *testing.T) {
	conn, _ := dialScriptedGameServer(t, "")
	c := newGameServerClient(nil, conn, 7, "element", zap.NewNop())
	go c.writePump()

	if _, err := c.requestRCON(protocol.GameRCONListUUIDs, time.Second); err == nil {
		t.Fatal("空回执应返回 error")
	}
}

// TestRequestRCON_ConnectionClosed 连接关闭后等待方必须被唤醒，避免 goroutine 泄漏。
func TestRequestRCON_ConnectionClosed(t *testing.T) {
	c := newGameServerClient(nil, dialSilentGameServer(t), 7, "element", zap.NewNop())
	go c.writePump()

	done := make(chan error, 1)
	go func() {
		_, err := c.requestRCON(protocol.GameRCONListUUIDs, 5*time.Second)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("连接关闭后应返回 error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("连接关闭后等待方未被唤醒")
	}
}

// TestDeliverRCONEcho_NoWaiter 无等待方的回执（如 send_private_msg）不应被当作采集结果。
func TestDeliverRCONEcho_NoWaiter(t *testing.T) {
	c := newGameServerClient(nil, nil, 7, "element", zap.NewNop())
	if c.deliverRCONEcho(protocol.GameServerResponse{API: protocol.GameAPISendRCONCommand, Echo: "rcon:unknown"}) {
		t.Fatal("无等待方应返回 false")
	}
	if c.deliverRCONEcho(protocol.GameServerResponse{API: protocol.GameAPISendPrivateMsg, Echo: "x"}) {
		t.Fatal("非 RCON 回执应返回 false")
	}
}

// TestParseGameServerEvent_RCONResponseNotEnqueued RCON 回执只交给等待方，不得进入业务队列。
func TestParseGameServerEvent_RCONResponseNotEnqueued(t *testing.T) {
	c := newGameServerClient(nil, nil, 7, "element", zap.NewNop())
	echo := "rcon:test"
	ch := make(chan string, 1)
	c.rconWait[echo] = ch

	raw := `{"code":200,"api":"send_rcon_command","post_type":"response","status":"SUCCESS",` +
		`"data":"` + statusTestEchoText + `","echo":"` + echo + `"}`
	if _, ok := c.parseGameServerEvent([]byte(raw)); ok {
		t.Fatal("回执不应产生业务任务")
	}
	select {
	case got := <-ch:
		if got != statusTestEchoText {
			t.Fatalf("等待方收到的回显错误: %q", got)
		}
	default:
		t.Fatal("等待方未收到回显")
	}
}
