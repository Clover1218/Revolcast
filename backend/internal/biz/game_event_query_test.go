package biz

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"revolcast-backend/internal/repository"
)

// ---- fakes ----

// fakeEventReader 在内存中按「received_at ASC, _id ASC」实现与仓储层一致的游标语义，
// 用于验证 service 是否正确传递方向与游标（仓储层的 filter 形态另有 repository 单测）。
type fakeEventReader struct {
	docs  []*repository.GameEventRecord // 需已按 received_at ASC, _id ASC 排好
	err   error
	calls []repository.GameEventQuery
}

func (f *fakeEventReader) QueryGameEvents(_ context.Context, q repository.GameEventQuery) ([]*repository.GameEventRecord, bool, error) {
	f.calls = append(f.calls, q)
	if f.err != nil {
		return nil, false, f.err
	}

	matched := make([]*repository.GameEventRecord, 0, len(f.docs))
	for _, d := range f.docs {
		if d.ServerID != q.ServerID {
			continue
		}
		switch q.Direction {
		case repository.GameEventBefore:
			if compareEventPos(d.ReceivedAt, d.ID, q.Cursor.ReceivedAt, q.Cursor.ID) >= 0 {
				continue
			}
		case repository.GameEventAfter:
			if compareEventPos(d.ReceivedAt, d.ID, q.Cursor.ReceivedAt, q.Cursor.ID) <= 0 {
				continue
			}
		}
		matched = append(matched, d)
	}

	if q.Direction == repository.GameEventAfter {
		if len(matched) > q.Limit {
			return matched[:q.Limit], true, nil
		}
		return matched, false, nil
	}
	// 最新 / before：取末尾最近的 q.Limit 条，并模拟「多取 1 条」探测。
	if len(matched) > q.Limit {
		return matched[len(matched)-q.Limit:], true, nil
	}
	return matched, false, nil
}

// fakeEventServerResolver 是 GameServerResolver 的测试替身。
type fakeEventServerResolver struct {
	rec *repository.GameServerRecord
	err error
}

func (f *fakeEventServerResolver) ResolveSubscribableServer(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

// fakeEventLimiter 记录限流键，便于断言 subject 维度。
type fakeEventLimiter struct {
	allowed bool
	err     error
	keys    []string
}

func (f *fakeEventLimiter) AllowAttempt(_ context.Context, key string, _ time.Duration, _ int64) (bool, error) {
	f.keys = append(f.keys, key)
	return f.allowed, f.err
}

// ---- helpers ----

func compareEventPos(t1 time.Time, id1 primitive.ObjectID, t2 time.Time, id2 primitive.ObjectID) int {
	if t1.UnixMilli() != t2.UnixMilli() {
		if t1.UnixMilli() < t2.UnixMilli() {
			return -1
		}
		return 1
	}
	// ObjectId 的十六进制表示保持字节序，可直接按字符串比较。
	switch {
	case id1.Hex() < id2.Hex():
		return -1
	case id1.Hex() > id2.Hex():
		return 1
	}
	return 0
}

// newEventRecord 构造测试事件；msOffset 用于制造相同 received_at 的场景。
func newEventRecord(serverID int64, oidHex string, receivedAt time.Time, eventType string) *repository.GameEventRecord {
	id, _ := primitive.ObjectIDFromHex(oidHex)
	return &repository.GameEventRecord{
		ID:         id,
		EventType:  eventType,
		ServerName: "TestServer",
		ServerID:   serverID,
		Player:     repository.GameEventPlayer{UUID: "u", Nickname: "n", IsOp: true},
		OccurredAt: receivedAt.Add(-time.Second),
		ReceivedAt: receivedAt,
	}
}

func newQueryService(reader GameEventReader, resolver GameServerResolver, limiter ServerRateLimiter) *GameEventQueryService {
	return NewGameEventQueryService(reader, resolver, limiter, GameEventQueryConfig{})
}

func okResolver(serverID int64) *fakeEventServerResolver {
	return &fakeEventServerResolver{rec: &repository.GameServerRecord{ID: serverID, Code: "test", Status: 1}}
}

var (
	oidA = "65000000000000000000000a"
	oidB = "65000000000000000000000b"
	oidC = "65000000000000000000000c"
)

// ---- tests ----

// limit 默认值、上下界与非法值（HTTP 接口文档 §13.2）。
func TestGameEventQuery_LimitValidation(t *testing.T) {
	cases := []struct {
		name      string
		limit     *int
		wantErr   bool
		wantLimit int
	}{
		{"未提供取默认 50", nil, false, GameEventDefaultLimit},
		{"最小 1", intPtr(1), false, 1},
		{"最大 100", intPtr(100), false, 100},
		{"显式 0 非法", intPtr(0), true, 0},
		{"负数非法", intPtr(-1), true, 0},
		{"超上限非法", intPtr(101), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeEventReader{}
			svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

			_, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: tc.limit})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidParam) {
					t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
				}
				if len(reader.calls) != 0 {
					t.Fatal("参数非法时不应查询存储")
				}
				return
			}
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if len(reader.calls) != 1 || reader.calls[0].Limit != tc.wantLimit {
				t.Fatalf("查询 limit = %+v，期望 %d", reader.calls, tc.wantLimit)
			}
		})
	}
}

// before 与 after 互斥。
func TestGameEventQuery_BeforeAfterConflict(t *testing.T) {
	reader := &fakeEventReader{}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	_, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{
		Before: encodeGameEventCursor(time.Now(), mustOID(oidA)),
		After:  encodeGameEventCursor(time.Now(), mustOID(oidB)),
	})
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("同时提供 before/after 应返回 ErrInvalidParam，实际 %v", err)
	}
	if len(reader.calls) != 0 {
		t.Fatal("参数非法时不应查询存储")
	}
}

// 游标编解码往返，以及无效游标的各类情况。
func TestGameEventQuery_CursorCodec(t *testing.T) {
	ts := time.Date(2026, 10, 19, 7, 29, 37, 125_000_000, time.UTC)
	raw := encodeGameEventCursor(ts, mustOID(oidA))
	if raw == "" {
		t.Fatal("游标编码不应为空")
	}

	point, err := decodeGameEventCursor(raw)
	if err != nil {
		t.Fatalf("合法游标解码失败: %v", err)
	}
	if !point.ReceivedAt.Equal(ts) {
		t.Errorf("ReceivedAt = %v, want %v", point.ReceivedAt, ts)
	}
	if point.ID.Hex() != oidA {
		t.Errorf("ID = %s, want %s", point.ID.Hex(), oidA)
	}

	// 同一毫秒、不同 _id 的游标必须不同，否则翻页会重复/遗漏。
	other := encodeGameEventCursor(ts, mustOID(oidB))
	if other == raw {
		t.Error("同一 received_at 不同 _id 的游标不应相同")
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{"非 base64", "!!!!"},
		{"base64 但非 JSON", "aGVsbG8"},
		{"空 JSON 对象", "e30"},
		{"缺少 _id", "eyJ0IjoxNzYwODYyMTc3MTI1fQ"},
		{"t 非正数", "eyJ0IjowLCJpZCI6IjY1MDAwMDAwMDAwMDAwMDAwMDAwMDAwYSJ9"},
		{"id 非 ObjectId", "eyJ0IjoxNzYwODYyMTc3MTI1LCJpZCI6Inh4In0"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeGameEventCursor(tc.raw); !errors.Is(err, ErrInvalidParam) {
				t.Fatalf("期望 ErrInvalidParam，实际 %v", err)
			}
		})
	}
}

// 默认模式：返回最新一页，升序，next_cursor 指向更早（oldest_cursor）。
func TestGameEventQuery_LatestPageCursors(t *testing.T) {
	base := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	reader := &fakeEventReader{docs: []*repository.GameEventRecord{
		newEventRecord(1, oidA, base, "player_join"),
		newEventRecord(1, oidB, base.Add(time.Second), "player_chat"),
		newEventRecord(1, oidC, base.Add(2*time.Second), "player_quit"),
	}}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	page, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: intPtr(2)})
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	// 升序：第二、三条（最新两条）。
	if page.Items[0].EventType != "player_chat" || page.Items[1].EventType != "player_quit" {
		t.Errorf("items 顺序错误: %q,%q", page.Items[0].EventType, page.Items[1].EventType)
	}
	if page.OldestCursor == nil || *page.OldestCursor != page.Items[0].Cursor {
		t.Error("oldest_cursor 应等于首条事件游标")
	}
	if page.NewestCursor == nil || *page.NewestCursor != page.Items[1].Cursor {
		t.Error("newest_cursor 应等于末条事件游标")
	}
	if page.NextCursor == nil || *page.NextCursor != *page.OldestCursor {
		t.Error("默认模式下 next_cursor 应指向更早方向（oldest_cursor）")
	}
	if !page.HasMore {
		t.Error("还有更早事件时 has_more 应为 true")
	}
}

// after 模式：next_cursor 指向更新（newest_cursor），且查询方向为 After。
func TestGameEventQuery_AfterPageCursors(t *testing.T) {
	base := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	reader := &fakeEventReader{docs: []*repository.GameEventRecord{
		newEventRecord(1, oidA, base, "player_join"),
		newEventRecord(1, oidB, base.Add(time.Second), "player_chat"),
	}}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	page, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{
		After: encodeGameEventCursor(base, mustOID(oidA)),
		Limit: intPtr(10),
	})
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].EventType != "player_chat" {
		t.Fatalf("after 查询结果错误: %+v", page.Items)
	}
	if reader.calls[0].Direction != repository.GameEventAfter {
		t.Errorf("Direction = %v, want After", reader.calls[0].Direction)
	}
	if page.NextCursor == nil || *page.NextCursor != *page.NewestCursor {
		t.Error("after 模式下 next_cursor 应指向更新方向（newest_cursor）")
	}
	if page.HasMore {
		t.Error("无更多新事件时 has_more 应为 false")
	}
}

// 空结果不是错误：items 为空切片、游标为 null。
func TestGameEventQuery_EmptyResult(t *testing.T) {
	reader := &fakeEventReader{}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	page, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{})
	if err != nil {
		t.Fatalf("空结果不应返回错误: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 {
		t.Errorf("items 应为非 nil 空切片，实际 %v", page.Items)
	}
	if page.OldestCursor != nil || page.NewestCursor != nil || page.NextCursor != nil {
		t.Error("空结果时游标应为 nil")
	}
	if page.HasMore {
		t.Error("空结果 has_more 应为 false")
	}
}

// 服务器解析错误（格式非法 / 不存在 / 已停用）必须原样向上返回业务错误。
func TestGameEventQuery_ServerErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"code 非法", Wrap(ErrServerCodeInvalid, nil, "bad code")},
		{"服务器不存在", Wrap(ErrServerNotFound, nil, "missing")},
		{"服务器已停用", Wrap(ErrServerDisabled, nil, "disabled")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeEventReader{}
			svc := newQueryService(reader, &fakeEventServerResolver{err: tc.err}, &fakeEventLimiter{allowed: true})

			_, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{})
			if !errors.Is(err, tc.err) {
				t.Fatalf("期望 %v，实际 %v", tc.err, err)
			}
			if len(reader.calls) != 0 {
				t.Fatal("服务器校验失败时不应查询存储")
			}
		})
	}
}

// 限流命中返回 ErrRateLimited，且限流键以登录用户 user_id 为 subject。
func TestGameEventQuery_RateLimited(t *testing.T) {
	reader := &fakeEventReader{}
	limiter := &fakeEventLimiter{allowed: false}
	svc := newQueryService(reader, okResolver(1), limiter)

	_, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
	if len(limiter.keys) != 1 || limiter.keys[0] != repository.UserRateLimitKey("game_events", "7") {
		t.Errorf("限流键 = %v，期望 %s", limiter.keys, repository.UserRateLimitKey("game_events", "7"))
	}
	if len(reader.calls) != 0 {
		t.Fatal("限流命中时不应查询存储")
	}
}

// 存储错误包裹为 ErrInternal 并保留根因。
func TestGameEventQuery_ReaderError(t *testing.T) {
	reader := &fakeEventReader{err: errors.New("mongo down")}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	_, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	// 根因不进入对外错误链（Wrap 用 %v 附带，仅为日志可读），但必须出现在错误文本中。
	if !strings.Contains(err.Error(), "mongo down") {
		t.Errorf("错误文本应含根因，实际 %q", err.Error())
	}
}

// 只返回指定服务器的事件（不同服务器数据互不混淆）。
func TestGameEventQuery_ServerIsolation(t *testing.T) {
	base := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	reader := &fakeEventReader{docs: []*repository.GameEventRecord{
		newEventRecord(1, oidA, base, "player_join"),
		newEventRecord(2, oidB, base.Add(time.Second), "player_chat"),
	}}
	svc := newQueryService(reader, okResolver(2), &fakeEventLimiter{allowed: true})

	page, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: intPtr(10)})
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ServerID != 2 {
		t.Fatalf("应只返回 server_id=2 的事件，实际 %+v", page.Items)
	}
	// 查询条件必须使用解析得到的 server_id，而不是前端任何输入。
	if reader.calls[0].ServerID != 2 {
		t.Errorf("查询 ServerID = %d, want 2", reader.calls[0].ServerID)
	}
}

// 同一 received_at 下翻页不重复、不遗漏：连续用 next_cursor 走完两个方向。
func TestGameEventQuery_PaginationNoDupNoMiss(t *testing.T) {
	// 9 条事件，其中 4 条共享同一毫秒（专测 tie-break）。
	base := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	var docs []*repository.GameEventRecord
	for i := 0; i < 9; i++ {
		ms := i
		if i >= 3 && i <= 6 {
			ms = 3 // 3..6 落在同一毫秒
		}
		docs = append(docs, newEventRecord(1,
			primitive.NewObjectIDFromTimestamp(base.Add(time.Duration(i)*time.Second)).Hex(),
			base.Add(time.Duration(ms)*time.Millisecond), "player_join"))
	}
	// ObjectId 的时间戳前缀在这里只是构造手段，需显式按 (received_at, _id) 排序后再交给 fake。
	sort.Slice(docs, func(i, j int) bool {
		return compareEventPos(docs[i].ReceivedAt, docs[i].ID, docs[j].ReceivedAt, docs[j].ID) < 0
	})

	reader := &fakeEventReader{docs: docs}
	svc := newQueryService(reader, okResolver(1), &fakeEventLimiter{allowed: true})

	// 1) 默认取最新一页，然后不断向前（before）翻页，直到没有更早事件。
	seen := map[string]int{}
	var forward []*GameEventItem
	first, err := svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: intPtr(3)})
	if err != nil {
		t.Fatalf("首屏查询失败: %v", err)
	}
	for _, it := range first.Items {
		seen[it.ID.Hex()]++
	}
	forward = append(forward, first.Items...)

	page := first
	for page.HasMore {
		page, err = svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: intPtr(3), Before: *page.NextCursor})
		if err != nil {
			t.Fatalf("before 翻页失败: %v", err)
		}
		if len(page.Items) == 0 {
			t.Fatal("has_more=true 却返回空页，翻页无法收敛")
		}
		for _, it := range page.Items {
			seen[it.ID.Hex()]++
		}
		// 向上加载是「前置追加」：更早的一页拼到列表前面。
		forward = append(append([]*GameEventItem{}, page.Items...), forward...)
	}
	if len(seen) != len(docs) {
		t.Fatalf("before 翻页覆盖 %d 条，期望 %d 条（有遗漏或重复）", len(seen), len(docs))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("事件 %s 出现 %d 次（重复）", id, n)
		}
	}
	// 覆盖全量后，拼起来应等于升序全集。
	for i := 1; i < len(forward); i++ {
		if compareEventPos(forward[i-1].ReceivedAt, forward[i-1].ID, forward[i].ReceivedAt, forward[i].ID) >= 0 {
			t.Fatalf("翻页结果非严格升序，第 %d 条与第 %d 条逆序", i-1, i)
		}
	}

	// 2) 从最早一条之后开始，用 after 连续补齐，同样不得重复/遗漏。
	afterSeen := map[string]int{}
	cursor := encodeGameEventCursor(docs[0].ReceivedAt, docs[0].ID)
	afterSeen[docs[0].ID.Hex()]++ // 起点自身不在结果中（严格晚于游标）
	for {
		p, qerr := svc.Query(context.Background(), "7", "test", GameEventQueryParams{Limit: intPtr(3), After: cursor})
		if qerr != nil {
			t.Fatalf("after 补齐失败: %v", qerr)
		}
		for _, it := range p.Items {
			afterSeen[it.ID.Hex()]++
		}
		if !p.HasMore {
			break
		}
		if p.NextCursor == nil {
			t.Fatal("has_more=true 时 next_cursor 不应为 nil")
		}
		cursor = *p.NextCursor
	}
	if len(afterSeen) != len(docs) {
		t.Fatalf("after 补齐覆盖 %d 条，期望 %d 条", len(afterSeen), len(docs))
	}
	for id, n := range afterSeen {
		if n != 1 {
			t.Errorf("after 补齐时事件 %s 出现 %d 次（重复）", id, n)
		}
	}
}

func intPtr(v int) *int { return &v }

func mustOID(hex string) primitive.ObjectID {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		panic(err)
	}
	return id
}
