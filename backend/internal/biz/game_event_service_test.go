package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

// fakeGameEventStore 是 GameEventStore 的测试替身，捕获插入的文档以做断言。
// 与真实仓储一致：写入时由「MongoDB 侧」生成 _id 并回传，biz 不得自行生成事件 ID。
type fakeGameEventStore struct {
	inserted []*repository.GameEventDoc
	ids      []primitive.ObjectID // 预置回传的 _id（按写入顺序消费）；耗尽后自动生成
	err      error
}

func (f *fakeGameEventStore) InsertGameEvent(_ context.Context, doc *repository.GameEventDoc) (primitive.ObjectID, error) {
	if f.err != nil {
		return primitive.NilObjectID, f.err
	}
	f.inserted = append(f.inserted, doc)
	if len(f.ids) >= len(f.inserted) {
		return f.ids[len(f.inserted)-1], nil
	}
	return primitive.NewObjectID(), nil
}

func baseGameEventPayload(subType string) *protocol.GameEventPayload {
	return &protocol.GameEventPayload{
		Timestamp:  1792395025,
		PostType:   "notice",
		EventName:  "PlayerJoinEvent",
		ServerName: "TestServer",
		SubType:    subType,
		Player:     protocol.QueqiaoPlayer{Nickname: "PlantClover", UUID: "a3b91ae7", IsOp: true},
	}
}

func TestGameEventService_Ingest_OK(t *testing.T) {
	cases := []struct {
		subType  string
		wantData int // 期望 data 子文档键数
	}{
		{"player_join", 0},
		{"player_quit", 0},
		{"player_chat", 3},
		{"player_command", 3},
	}
	for _, tc := range cases {
		t.Run(tc.subType, func(t *testing.T) {
			store := &fakeGameEventStore{}
			svc := NewGameEventService(store)

			p := baseGameEventPayload(tc.subType)
			p.Message = "hi"
			p.RawMessage = `{"text":"hi"}`
			p.MessageID = "m1"
			p.Command = "gamemode creative"

			recv := time.Date(2026, 10, 19, 7, 30, 25, 125000000, time.UTC)
			view, err := svc.Ingest(context.Background(), 7, recv, p)
			if err != nil {
				t.Fatalf("Ingest 不应返回错误: %v", err)
			}
			if len(store.inserted) != 1 {
				t.Fatalf("期望插入 1 条，实际 %d", len(store.inserted))
			}
			doc := store.inserted[0]
			if doc.ServerID != 7 {
				t.Errorf("ServerID = %d, want 7", doc.ServerID)
			}
			if doc.EventType != tc.subType {
				t.Errorf("EventType = %q, want %q", doc.EventType, tc.subType)
			}
			if doc.ServerName != "TestServer" {
				t.Errorf("ServerName = %q", doc.ServerName)
			}
			if doc.Player.UUID != "a3b91ae7" || doc.Player.Nickname != "PlantClover" || !doc.Player.IsOp {
				t.Errorf("Player 快照错误: %+v", doc.Player)
			}
			// occurred_at 由原始 timestamp（秒）转换：1792395025 -> 2026-10-19T07:30:25Z
			// 时间为 BSON Date（time.Time），不再是字符串（见 repository.GameEventDoc 注释）。
			wantOccurred := time.Date(2026, 10, 19, 7, 30, 25, 0, time.UTC)
			if !doc.OccurredAt.Equal(wantOccurred) {
				t.Errorf("OccurredAt = %v, want %v", doc.OccurredAt, wantOccurred)
			}
			// received_at 取后端收到时间，并截断到毫秒（与 BSON Date 精度、HTTP 输出一致）。
			if !doc.ReceivedAt.Equal(recv) || doc.ReceivedAt.Nanosecond()%int(time.Millisecond) != 0 {
				t.Errorf("ReceivedAt = %v, want %v（毫秒精度）", doc.ReceivedAt, recv)
			}
			if len(doc.Data) != tc.wantData {
				t.Errorf("Data 键数 = %d, want %d (%v)", len(doc.Data), tc.wantData, doc.Data)
			}
			if tc.subType == "player_chat" && doc.Data["message"] != "hi" {
				t.Errorf("chat data.message = %v, want hi", doc.Data["message"])
			}
			if tc.subType == "player_command" && doc.Data["command"] != "gamemode creative" {
				t.Errorf("command data.command = %v, want 'gamemode creative'", doc.Data["command"])
			}
			// 事件身份来自持久化结果：_id 由存储回传，游标由 (received_at, _id) 确定性生成。
			// 两者在写入成功后必须立即可用，供 WS 推送与 HTTP 查询给出同一身份。
			if view.ID.IsZero() {
				t.Fatal("Ingest 必须回填持久化文档的 _id")
			}
			if want := encodeGameEventCursor(doc.ReceivedAt, view.ID); view.Cursor != want {
				t.Errorf("Cursor = %q, want %q（应由落库时刻与 _id 生成）", view.Cursor, want)
			}
			// 落库值与视图时刻必须一致（毫秒截断后同源），否则游标 t 与 BSON Date 会对不上。
			if !view.ReceivedAt.Equal(doc.ReceivedAt) {
				t.Errorf("视图 ReceivedAt = %v 与落库值 %v 不一致", view.ReceivedAt, doc.ReceivedAt)
			}
		})
	}
}

func TestGameEventService_Ingest_UnknownSubType(t *testing.T) {
	store := &fakeGameEventStore{}
	svc := NewGameEventService(store)
	_, err := svc.Ingest(context.Background(), 1, time.Now(), baseGameEventPayload("player_explode"))
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam, 实际 %v", err)
	}
	if len(store.inserted) != 0 {
		t.Fatal("非法事件不应写入存储")
	}
}

func TestGameEventService_Ingest_Nil(t *testing.T) {
	svc := NewGameEventService(&fakeGameEventStore{})
	if _, err := svc.Ingest(context.Background(), 1, time.Now(), nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatal("nil 事件应返回 ErrInvalidParam")
	}
}

func TestGameEventService_Ingest_StoreError(t *testing.T) {
	store := &fakeGameEventStore{err: errors.New("mongo down")}
	svc := NewGameEventService(store)
	_, err := svc.Ingest(context.Background(), 1, time.Now(), baseGameEventPayload("player_join"))
	// 写入失败必须包裹为 ErrInternal 且保留根因（供日志输出完整链）。
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("写入失败应包裹为 ErrInternal, 实际 %v", err)
	}
}
