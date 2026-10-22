package repository

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func mustOID(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("构造 ObjectId 失败: %v", err)
	}
	return id
}

// 最新模式：仅按 server_id 过滤（不接受前端传入的其它过滤条件）。
func TestGameEventFilter_Latest(t *testing.T) {
	got := gameEventFilter(GameEventQuery{ServerID: 3})
	want := bson.D{{Key: "server_id", Value: int64(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter = %v, want %v", got, want)
	}
}

// before：严格早于游标，且同一 received_at 时以 _id 兜底（避免重复/遗漏）。
func TestGameEventFilter_Before(t *testing.T) {
	ts := time.Date(2026, 10, 19, 7, 29, 37, 125_000_000, time.UTC)
	oid := mustOID(t, "65000000000000000000000a")

	got := gameEventFilter(GameEventQuery{
		ServerID:  3,
		Direction: GameEventBefore,
		Cursor:    GameEventCursorPoint{ReceivedAt: ts, ID: oid},
	})
	want := bson.D{
		{Key: "server_id", Value: int64(3)},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "received_at", Value: bson.D{{Key: "$lt", Value: ts}}}},
			bson.D{
				{Key: "received_at", Value: ts},
				{Key: "_id", Value: bson.D{{Key: "$lt", Value: oid}}},
			},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter = %v, want %v", got, want)
	}
}

// after：严格晚于游标，方向符号相反。
func TestGameEventFilter_After(t *testing.T) {
	ts := time.Date(2026, 10, 19, 7, 29, 37, 125_000_000, time.UTC)
	oid := mustOID(t, "65000000000000000000000b")

	got := gameEventFilter(GameEventQuery{
		ServerID:  3,
		Direction: GameEventAfter,
		Cursor:    GameEventCursorPoint{ReceivedAt: ts, ID: oid},
	})
	want := bson.D{
		{Key: "server_id", Value: int64(3)},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "received_at", Value: bson.D{{Key: "$gt", Value: ts}}}},
			bson.D{
				{Key: "received_at", Value: ts},
				{Key: "_id", Value: bson.D{{Key: "$gt", Value: oid}}},
			},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter = %v, want %v", got, want)
	}
}

// 时间字段必须以 BSON Date 参与比较；若误存字符串，Mongo 会按不同比较域处理，此处锁死类型。
func TestGameEventFilter_UsesDateType(t *testing.T) {
	ts := time.Date(2026, 10, 19, 7, 29, 37, 0, time.UTC)
	filter := gameEventFilter(GameEventQuery{
		ServerID:  1,
		Direction: GameEventBefore,
		Cursor:    GameEventCursorPoint{ReceivedAt: ts, ID: primitive.NewObjectID()},
	})

	or, ok := filter[1].Value.(bson.A)
	if !ok || len(or) != 2 {
		t.Fatalf("$or 结构异常: %v", filter[1].Value)
	}
	first := or[0].(bson.D)
	cmp := first[0].Value.(bson.D)[0].Value
	if _, isTime := cmp.(time.Time); !isTime {
		t.Errorf("游标时间比较值类型 = %T，期望 time.Time（BSON Date）", cmp)
	}
}

// limit 非法时不得触达数据库（coll 为 nil 也不应 panic）。
func TestGameEventRepo_QueryRejectsInvalidLimit(t *testing.T) {
	repo := &GameEventRepo{}
	if _, _, err := repo.QueryGameEvents(context.Background(), GameEventQuery{ServerID: 1, Limit: 0}); err == nil {
		t.Fatal("limit=0 应返回错误")
	}
}

// 索引定义必须包含游标分页所需的 {server_id, received_at, _id} 复合索引，且键序为
// 「等值前缀 → 排序字段 → 同毫秒 tie-break」（HTTP 接口文档 §13.6）。
func TestGameEventIndexModels(t *testing.T) {
	want := bson.D{
		{Key: "server_id", Value: 1},
		{Key: "received_at", Value: 1},
		{Key: "_id", Value: 1},
	}

	models := gameEventIndexModels()
	for _, m := range models {
		if reflect.DeepEqual(m.Keys, want) {
			return
		}
	}
	t.Fatalf("缺少复合索引 %v，现有定义：", want)
	for _, m := range models {
		t.Logf("  %v", m.Keys)
	}
}
