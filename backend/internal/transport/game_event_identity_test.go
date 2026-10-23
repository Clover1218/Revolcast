package transport

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

// 本文件验证「统一事件身份」这条跨链路契约（HTTP 历史查询 ↔ WebSocket 实时推送）：
//   - 同一条持久化事件在两侧的 id / cursor / received_at 完全一致；
//   - 同一毫秒内的多条事件 id 与游标互不相同；
//   - 推送丢失（无订阅者 / 投递失败）时，HTTP after 仍能补齐该事件；
//   - 时间统一到毫秒，不存在「BSON Date 精度 vs 游标 t」的不一致。
//
// 这里刻意使用真实链路（biz.Ingest → biz.GameEventQueryService → HTTP handler / buildGameEventPush），
// 只把 MongoDB 替换为内存实现，避免只改协议示例却在两端各生成一套身份。

// identityStore 同时实现 biz.GameEventStore（写入）与 biz.GameEventReader（查询），
// 模拟 MongoDB 的 _id 分配与 `received_at ASC, _id ASC` 排序语义。
type identityStore struct {
	recs []*repository.GameEventRecord
	ids  []primitive.ObjectID // 预置 _id（按写入顺序）；耗尽后自动生成
}

func (s *identityStore) InsertGameEvent(_ context.Context, doc *repository.GameEventDoc) (primitive.ObjectID, error) {
	id := primitive.NewObjectID()
	if len(s.recs) < len(s.ids) {
		id = s.ids[len(s.recs)]
	}
	s.recs = append(s.recs, &repository.GameEventRecord{
		ID:         id,
		EventType:  doc.EventType,
		ServerName: doc.ServerName,
		ServerID:   doc.ServerID,
		Player:     doc.Player,
		OccurredAt: doc.OccurredAt,
		ReceivedAt: doc.ReceivedAt, // 原样落库：Ingest 已截断到毫秒，模拟 BSON Date 精度
		Data: repository.GameEventData{
			Message:    bsonString(doc.Data, "message"),
			RawMessage: bsonString(doc.Data, "raw_message"),
			MessageID:  bsonString(doc.Data, "message_id"),
			Command:    bsonString(doc.Data, "command"),
		},
	})
	return id, nil
}

func (s *identityStore) QueryGameEvents(_ context.Context, q repository.GameEventQuery) ([]*repository.GameEventRecord, bool, error) {
	matched := make([]*repository.GameEventRecord, 0, len(s.recs))
	for _, r := range s.recs {
		if r.ServerID != q.ServerID {
			continue
		}
		cmp := compareIdentityPos(r.ReceivedAt, r.ID, q.Cursor.ReceivedAt, q.Cursor.ID)
		if q.Direction == repository.GameEventBefore && cmp >= 0 {
			continue
		}
		if q.Direction == repository.GameEventAfter && cmp <= 0 {
			continue
		}
		matched = append(matched, r)
	}
	sort.Slice(matched, func(i, j int) bool {
		return compareIdentityPos(matched[i].ReceivedAt, matched[i].ID, matched[j].ReceivedAt, matched[j].ID) < 0
	})

	if q.Direction == repository.GameEventAfter {
		if len(matched) > q.Limit {
			return matched[:q.Limit], true, nil
		}
		return matched, false, nil
	}
	if len(matched) > q.Limit {
		return matched[len(matched)-q.Limit:], true, nil
	}
	return matched, false, nil
}

// compareIdentityPos 按 received_at（毫秒）+ _id（hex 字典序 ≡ 字节序）比较两个事件位置。
func compareIdentityPos(t1 time.Time, id1 primitive.ObjectID, t2 time.Time, id2 primitive.ObjectID) int {
	if m1, m2 := t1.UnixMilli(), t2.UnixMilli(); m1 != m2 {
		if m1 < m2 {
			return -1
		}
		return 1
	}
	switch {
	case id1.Hex() < id2.Hex():
		return -1
	case id1.Hex() > id2.Hex():
		return 1
	}
	return 0
}

func bsonString(m bson.M, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// newIdentityHandler 用真实查询服务 + 真实处理器组装 HTTP 入口（MongoDB 由 identityStore 替代）。
func newIdentityHandler(t *testing.T, store *identityStore) *GameEventHandler {
	t.Helper()
	qs := biz.NewGameEventQueryService(
		store,
		&fakeWebServerLookup{rec: newEnabledGameServer()},
		&fakeHandshakeLimiter{allowed: true},
		biz.GameEventQueryConfig{},
	)
	return NewGameEventHandler(qs, &fakeWebSessionAuth{userID: 7}, zap.NewNop())
}

// queryEvents 发起一次 GET .../events 并返回 items 与分页游标。
func queryEvents(t *testing.T, h *GameEventHandler, target string) eventPageData {
	t.Helper()
	rec, env := doEvents(t, h, target, true)
	if rec.Code != 200 {
		t.Fatalf("events 查询失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var data eventPageData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}
	return data
}

func chatPayload() *protocol.GameEventPayload {
	return &protocol.GameEventPayload{
		Timestamp:  1792395025,
		PostType:   "message",
		EventName:  "PlayerChatEvent",
		ServerName: "TestServer",
		SubType:    "player_chat",
		Player:     protocol.QueqiaoPlayer{Nickname: "PlantClover", UUID: "a3b91ae7", IsOp: true},
		Message:    "hi",
		RawMessage: `{"text":"hi"}`,
		MessageID:  "m1",
	}
}

// 同一事件在 HTTP 与 WebSocket 中必须给出相同的 id / cursor / received_at。
func TestGameEventIdentity_HTTPAndWSShareIdentity(t *testing.T) {
	store := &identityStore{ids: []primitive.ObjectID{mustEventOID(t, "65000000000000000000000a")}}
	svc := biz.NewGameEventService(store)

	// 带亚毫秒部分：落库前必须被截断（BSON Date 精度为毫秒）。
	receivedAt := time.Date(2026, 10, 22, 15, 9, 34, 401_987_654, time.UTC)
	view, err := svc.Ingest(context.Background(), 10, receivedAt, chatPayload())
	if err != nil {
		t.Fatalf("Ingest 失败: %v", err)
	}

	// 写入的必须是毫秒精度，且与视图时刻一致（游标 t 来源于此）。
	stored := store.recs[0]
	if stored.ReceivedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("落库 received_at 未截断到毫秒: %v", stored.ReceivedAt)
	}
	if !stored.ReceivedAt.Equal(view.ReceivedAt) {
		t.Fatalf("视图时刻 %v 与落库值 %v 不一致", view.ReceivedAt, stored.ReceivedAt)
	}

	// WebSocket 侧：无订阅者 → 推送不投递（模拟「落库成功但推送丢失」），随后仍可从 HTTP 补齐。
	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()
	pushGameEventToWeb(hub, "trace-1", view, zap.NewNop())

	raw, err := buildGameEventPush("trace-1", view, "survival")
	if err != nil {
		t.Fatalf("构造推送失败: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("推送消息应为 Envelope: %v", err)
	}
	var ws protocol.GameEventPushPayload
	if err := json.Unmarshal(env.Payload, &ws); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if env.TraceID != "trace-1" || env.Type != "event.game.player_chat" {
		t.Fatalf("Envelope 头不符: %+v", env)
	}

	// HTTP 侧：真实查询服务 + 真实 handler。
	page := queryEvents(t, newIdentityHandler(t, store), "/api/v1/game-servers/test-server/events")
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	item := page.Items[0]

	// 三处身份必须完全一致。
	if item.ID != ws.ID || item.ID != view.ID.Hex() {
		t.Errorf("id 不一致: http=%q ws=%q ingest=%q", item.ID, ws.ID, view.ID.Hex())
	}
	if len(item.ID) != 24 {
		t.Errorf("id 应为 24 位十六进制，实际 %q", item.ID)
	}
	if item.Cursor != ws.Cursor || item.Cursor != view.Cursor {
		t.Errorf("cursor 不一致: http=%q ws=%q ingest=%q", item.Cursor, ws.Cursor, view.Cursor)
	}
	if item.ReceivedAt != ws.ReceivedAt {
		t.Errorf("received_at 不一致: http=%q ws=%q", item.ReceivedAt, ws.ReceivedAt)
	}
	// 统一毫秒格式：亚毫秒部分被丢弃，固定 3 位小数。
	if item.ReceivedAt != "2026-10-22T15:09:34.401Z" || ws.ReceivedAt != "2026-10-22T15:09:34.401Z" {
		t.Errorf("received_at 应为 UTC RFC3339 毫秒: http=%q ws=%q", item.ReceivedAt, ws.ReceivedAt)
	}
	if item.OccurredAt != "2026-10-19T07:30:25.000Z" || ws.OccurredAt != item.OccurredAt {
		t.Errorf("occurred_at 应统一为毫秒格式: http=%q ws=%q", item.OccurredAt, ws.OccurredAt)
	}
	// 页面边界游标取自同一条事件。
	if page.OldestCursor == nil || *page.OldestCursor != item.Cursor {
		t.Errorf("oldest_cursor 应等于该事件游标")
	}
	if page.NewestCursor == nil || *page.NewestCursor != item.Cursor {
		t.Errorf("newest_cursor 应等于该事件游标")
	}
}

// 同一毫秒内的多条事件必须具有不同的 id 与游标，且按 (received_at, _id) 升序返回。
func TestGameEventIdentity_SameMillisecondEvents(t *testing.T) {
	oidA := mustEventOID(t, "65000000000000000000000a")
	oidB := mustEventOID(t, "65000000000000000000000b")
	store := &identityStore{ids: []primitive.ObjectID{oidA, oidB}}
	svc := biz.NewGameEventService(store)

	ms := time.Date(2026, 10, 22, 15, 9, 34, 401_000_000, time.UTC)
	first, err := svc.Ingest(context.Background(), 10, ms.Add(120*time.Microsecond), chatPayload())
	if err != nil {
		t.Fatalf("Ingest 第一条失败: %v", err)
	}
	second, err := svc.Ingest(context.Background(), 10, ms.Add(870*time.Microsecond), chatPayload())
	if err != nil {
		t.Fatalf("Ingest 第二条失败: %v", err)
	}

	// 同一毫秒：时间相同但 _id 不同 → 身份与游标都必须不同。
	if !first.ReceivedAt.Equal(second.ReceivedAt) {
		t.Fatalf("前置条件不成立：两条事件应在同一毫秒 (%v / %v)", first.ReceivedAt, second.ReceivedAt)
	}
	if first.ID == second.ID {
		t.Fatal("同一毫秒的两条事件 id 不应相同")
	}
	if first.Cursor == second.Cursor {
		t.Fatal("同一毫秒的两条事件游标不应相同")
	}

	h := newIdentityHandler(t, store)
	page := queryEvents(t, h, "/api/v1/game-servers/test-server/events")
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	// 升序：_id 较小的在前（ObjectId 十六进制字典序 ≡ 字节序）。
	if page.Items[0].ID != oidA.Hex() || page.Items[1].ID != oidB.Hex() {
		t.Fatalf("同毫秒事件排序错误: %q, %q", page.Items[0].ID, page.Items[1].ID)
	}
	if page.Items[0].Cursor != first.Cursor || page.Items[1].Cursor != second.Cursor {
		t.Errorf("HTTP 游标与 Ingest 生成值不一致")
	}

	// 以第一条为锚点 after 补齐：只返回第二条，不重复返回锚点自身。
	after := queryEvents(t, h, "/api/v1/game-servers/test-server/events?after="+first.Cursor)
	if len(after.Items) != 1 || after.Items[0].ID != second.ID.Hex() {
		t.Fatalf("after 补齐结果错误: %+v", after.Items)
	}
}

// 事件写入成功但推送失败（无订阅者）时，HTTP after 必须能补齐该事件。
func TestGameEventIdentity_AfterBackfillsLostPush(t *testing.T) {
	store := &identityStore{}
	svc := biz.NewGameEventService(store)

	hub := NewHub(zap.NewNop())
	go hub.Run()
	defer hub.CloseAll()

	base := time.Date(2026, 10, 22, 15, 9, 34, 401_000_000, time.UTC)
	first, err := svc.Ingest(context.Background(), 10, base, chatPayload())
	if err != nil {
		t.Fatalf("Ingest 第一条失败: %v", err)
	}
	pushGameEventToWeb(hub, "trace-1", first, zap.NewNop()) // 无订阅者：模拟推送丢失

	second, err := svc.Ingest(context.Background(), 10, base.Add(2*time.Millisecond), chatPayload())
	if err != nil {
		t.Fatalf("Ingest 第二条失败: %v", err)
	}
	pushGameEventToWeb(hub, "trace-2", second, zap.NewNop()) // 同上

	// 客户端只处理到第一条（其 cursor 已保存），断线重连后用 after 补齐。
	page := queryEvents(t, newIdentityHandler(t, store), "/api/v1/game-servers/test-server/events?after="+first.Cursor)
	if len(page.Items) != 1 {
		t.Fatalf("after 应补齐 1 条，实际 %d", len(page.Items))
	}
	got := page.Items[0]
	if got.ID != second.ID.Hex() || got.Cursor != second.Cursor {
		t.Errorf("补齐事件身份不一致: id=%q/%q cursor=%q/%q", got.ID, second.ID.Hex(), got.Cursor, second.Cursor)
	}
	if got.ReceivedAt != protocol.FormatEventTime(second.ReceivedAt) {
		t.Errorf("补齐事件时间不一致: %q / %q", got.ReceivedAt, protocol.FormatEventTime(second.ReceivedAt))
	}
}
