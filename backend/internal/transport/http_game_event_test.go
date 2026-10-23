package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// ---- fakes ----

// fakeEventQueryer 记录调用入参，返回预置结果或错误。
type fakeEventQueryer struct {
	page     *biz.GameEventPage
	err      error
	calls    int
	lastKey  string
	lastCode string
	lastArgs biz.GameEventQueryParams
}

func (f *fakeEventQueryer) Query(_ context.Context, actorKey, serverCode string, p biz.GameEventQueryParams) (*biz.GameEventPage, error) {
	f.calls++
	f.lastKey = actorKey
	f.lastCode = serverCode
	f.lastArgs = p
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

// ---- helpers ----

type eventEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type eventPageData struct {
	Items []struct {
		ID         string                     `json:"id"`
		EventType  string                     `json:"event_type"`
		ServerName string                     `json:"server_name"`
		ServerID   int64                      `json:"server_id"`
		Player     protocol.QueqiaoPlayer     `json:"player"`
		OccurredAt string                     `json:"occurred_at"`
		ReceivedAt string                     `json:"received_at"`
		Data       protocol.GameEventPushData `json:"data"`
		Cursor     string                     `json:"cursor"`
	} `json:"items"`
	OldestCursor *string `json:"oldest_cursor"`
	NewestCursor *string `json:"newest_cursor"`
	NextCursor   *string `json:"next_cursor"`
	HasMore      bool    `json:"has_more"`
}

// mustEventOID 解析 24 位十六进制 ObjectId（测试用）。
func mustEventOID(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("非法 ObjectId %q: %v", hex, err)
	}
	return id
}

func newEventTestHandler(q *fakeEventQueryer, auth *fakeWebSessionAuth) *GameEventHandler {
	return NewGameEventHandler(q, auth, zap.NewNop())
}

// doEvents 发起带会话 Cookie 的 GET 请求；withCookie=false 时模拟未登录。
func doEvents(t *testing.T, h *GameEventHandler, target string, withCookie bool) (*httptest.ResponseRecorder, eventEnvelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	}
	req.SetPathValue("server_code", "test-server")

	rec := httptest.NewRecorder()
	h.Events(rec, req)

	var env eventEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	return rec, env
}

// ---- tests ----

func TestGameEventHandler_MethodNotAllowed(t *testing.T) {
	q := &fakeEventQueryer{}
	h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 1})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/game-servers/test-server/events", nil)
	rec := httptest.NewRecorder()
	h.Events(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if q.calls != 0 {
		t.Fatal("非 GET 请求不应触达业务层")
	}
}

// 未携带会话 Cookie → 401（不触达业务层）。
func TestGameEventHandler_NotAuthenticated(t *testing.T) {
	q := &fakeEventQueryer{}
	h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 1})

	rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events", false)
	if rec.Code != http.StatusUnauthorized || env.Code != 401 {
		t.Fatalf("status/code = %d/%d, want 401/401", rec.Code, env.Code)
	}
	if q.calls != 0 {
		t.Fatal("未登录不应触达业务层")
	}
}

// 会话失效（业务层解析报错）→ 401。
func TestGameEventHandler_SessionInvalid(t *testing.T) {
	q := &fakeEventQueryer{}
	h := newEventTestHandler(q, &fakeWebSessionAuth{err: biz.ErrNotAuthenticated})

	rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events", true)
	if rec.Code != http.StatusUnauthorized || env.Code != 401 {
		t.Fatalf("status/code = %d/%d, want 401/401", rec.Code, env.Code)
	}
	if q.calls != 0 {
		t.Fatal("会话无效不应触达业务层")
	}
}

// limit 非整数 → 400，且不触达业务层。
func TestGameEventHandler_InvalidLimitFormat(t *testing.T) {
	q := &fakeEventQueryer{page: &biz.GameEventPage{}}
	h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 9})

	rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events?limit=abc", true)
	if rec.Code != http.StatusBadRequest || env.Code != 400 {
		t.Fatalf("status/code = %d/%d, want 400/400", rec.Code, env.Code)
	}
	if q.calls != 0 {
		t.Fatal("参数非法不应触达业务层")
	}
}

// 正常响应：字段映射、毫秒时间格式、data 按类型裁剪、事件 id / 游标透传。
func TestGameEventHandler_SuccessMapping(t *testing.T) {
	oldest := "c-oldest"
	newest := "c-newest"
	firstID := mustEventOID(t, "65000000000000000000000a")
	secondID := mustEventOID(t, "65000000000000000000000b")
	page := &biz.GameEventPage{
		Items: []*biz.GameEventItem{
			{
				ID:         firstID,
				ServerID:   1,
				ServerName: "TestServer",
				EventType:  "player_chat",
				Player:     protocol.QueqiaoPlayer{UUID: "u1", Nickname: "PlantClover", IsOp: true},
				OccurredAt: time.Date(2026, 10, 19, 7, 29, 37, 0, time.UTC),
				ReceivedAt: time.Date(2026, 10, 19, 7, 29, 37, 125_000_000, time.UTC),
				Data:       protocol.GameEventPushData{Message: "有人吗", RawMessage: `{"text":"有人吗"}`, MessageID: ""},
				Cursor:     oldest,
			},
			{
				ID:         secondID,
				ServerID:   1,
				ServerName: "TestServer",
				EventType:  "player_command",
				Player:     protocol.QueqiaoPlayer{UUID: "u1", Nickname: "PlantClover"},
				OccurredAt: time.Date(2026, 10, 19, 7, 30, 0, 0, time.UTC),
				ReceivedAt: time.Date(2026, 10, 19, 7, 30, 0, 5_000_000, time.UTC),
				Data:       protocol.GameEventPushData{Command: "gamemode creative", MessageID: "m2"},
				Cursor:     newest,
			},
		},
		OldestCursor: &oldest,
		NewestCursor: &newest,
		NextCursor:   &oldest,
		HasMore:      true,
	}
	q := &fakeEventQueryer{page: page}
	h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 9})

	rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events?limit=2&before=abc", true)
	if rec.Code != http.StatusOK || env.Code != 200 {
		t.Fatalf("status/code = %d/%d, want 200/200", rec.Code, env.Code)
	}
	// 会话 user_id 必须作为限流 subject 传入，server_code 取自路径。
	if q.lastKey != "9" || q.lastCode != "test-server" {
		t.Errorf("业务入参 actorKey/serverCode = %q/%q", q.lastKey, q.lastCode)
	}
	if q.lastArgs.Limit == nil || *q.lastArgs.Limit != 2 || q.lastArgs.Before != "abc" || q.lastArgs.After != "" {
		t.Errorf("业务入参 params = %+v", q.lastArgs)
	}

	var data eventPageData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}
	if len(data.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(data.Items))
	}
	item := data.Items[0]
	if item.EventType != "player_chat" || item.ServerID != 1 || item.Player.Nickname != "PlantClover" {
		t.Errorf("首条字段映射错误: %+v", item)
	}
	// id 为持久化 _id 的 24 位十六进制，与 WS 推送 payload.id 同源。
	if item.ID != firstID.Hex() || len(item.ID) != 24 {
		t.Errorf("items[].id = %q, want %q", item.ID, firstID.Hex())
	}
	if data.Items[1].ID != secondID.Hex() {
		t.Errorf("items[1].id = %q, want %q", data.Items[1].ID, secondID.Hex())
	}
	// 时间固定毫秒格式，与存储精度一致。
	if item.OccurredAt != "2026-10-19T07:29:37.000Z" || item.ReceivedAt != "2026-10-19T07:29:37.125Z" {
		t.Errorf("时间格式错误: %q / %q", item.OccurredAt, item.ReceivedAt)
	}
	if item.Data.Message != "有人吗" || item.Data.RawMessage == "" {
		t.Errorf("chat data 映射错误: %+v", item.Data)
	}
	if item.Cursor != oldest {
		t.Errorf("cursor = %q, want %q", item.Cursor, oldest)
	}
	if data.OldestCursor == nil || *data.OldestCursor != oldest ||
		data.NewestCursor == nil || *data.NewestCursor != newest ||
		data.NextCursor == nil || *data.NextCursor != oldest {
		t.Error("游标字段映射错误")
	}
	if !data.HasMore {
		t.Error("has_more 应为 true")
	}
	// command 事件不下发 raw_message（与 WS 推送一致）。
	if data.Items[1].Data.RawMessage != "" {
		t.Errorf("command 事件不应下发 raw_message: %q", data.Items[1].Data.RawMessage)
	}
	// _id 仅以 24 位十六进制的 id 字段暴露，不返回 MongoDB 原始文档字段。
	if strings.Contains(string(env.Data), `"_id"`) {
		t.Errorf("响应不应包含 _id 字段: %s", env.Data)
	}
}

// 空结果必须序列化为 items: []（不是 null），游标为 null。
func TestGameEventHandler_EmptyResultShape(t *testing.T) {
	q := &fakeEventQueryer{page: &biz.GameEventPage{Items: []*biz.GameEventItem{}}}
	h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 9})

	rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var data eventPageData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}
	if data.Items == nil || len(data.Items) != 0 {
		t.Errorf("items 应为 []，实际 %+v", data.Items)
	}
	if data.OldestCursor != nil || data.NewestCursor != nil || data.NextCursor != nil {
		t.Error("空结果游标应为 null")
	}
	if data.HasMore {
		t.Error("空结果 has_more 应为 false")
	}
	if !strings.Contains(string(env.Data), `"items":[]`) {
		t.Errorf("空数组应序列化为 []，实际 %s", env.Data)
	}
}

// 业务错误 → 统一错误码映射（单一映射源 biz/errors.go）。
func TestGameEventHandler_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int
	}{
		{"参数错误", biz.Wrap(biz.ErrInvalidParam, nil, "bad"), 400, 400},
		{"服务器不存在", biz.Wrap(biz.ErrServerNotFound, nil, "missing"), 404, 404},
		{"服务器停用", biz.Wrap(biz.ErrServerDisabled, nil, "disabled"), 403, 403},
		{"限流", biz.ErrRateLimited, 429, 429},
		{"内部错误", biz.Wrap(biz.ErrInternal, errors.New("mongo down"), "query"), 500, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeEventQueryer{err: tc.err}
			h := newEventTestHandler(q, &fakeWebSessionAuth{userID: 9})

			rec, env := doEvents(t, h, "/api/v1/game-servers/test-server/events", true)
			if rec.Code != tc.wantStatus || env.Code != tc.wantCode {
				t.Fatalf("status/code = %d/%d, want %d/%d", rec.Code, env.Code, tc.wantStatus, tc.wantCode)
			}
			if q.calls != 1 {
				t.Errorf("业务层应被调用 1 次，实际 %d", q.calls)
			}
		})
	}
}
