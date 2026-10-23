package biz

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"revolcast-backend/internal/repository"
)

// fakeStatusCache 是 ServerStatusCache 的内存实现，同时记录写入的 TTL。
type fakeStatusCache struct {
	vals   map[string][]byte
	ttls   map[string]time.Duration
	getErr error
	setErr error
	delErr error
}

func newFakeStatusCache() *fakeStatusCache {
	return &fakeStatusCache{vals: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (f *fakeStatusCache) GetCache(_ context.Context, key string, dest any) (bool, error) {
	if f.getErr != nil {
		return false, f.getErr
	}
	raw, ok := f.vals[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeStatusCache) SetCache(_ context.Context, key string, val any, ttl time.Duration) error {
	if f.setErr != nil {
		return f.setErr
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	f.vals[key] = raw
	f.ttls[key] = ttl
	return nil
}

func (f *fakeStatusCache) DelCache(_ context.Context, keys ...string) error {
	if f.delErr != nil {
		return f.delErr
	}
	for _, k := range keys {
		delete(f.vals, k)
		delete(f.ttls, k)
	}
	return nil
}

const testListUUIDsEcho = "There are 2 of a max of 20 players online: " +
	"Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058), " +
	"Bot2 (16beaa94-ff1b-37a7-8afb-c22dcb8c16d1)"

// TestRecordListUUIDs_WritesSnapshot 采集成功必须写入完整快照，且 TTL = 3 × 采集周期。
func TestRecordListUUIDs_WritesSnapshot(t *testing.T) {
	cache := newFakeStatusCache()
	svc := NewGameStatusService(cache, GameStatusConfig{})

	if err := svc.RecordListUUIDs(context.Background(), 7, "element", testListUUIDsEcho); err != nil {
		t.Fatalf("采集失败: %v", err)
	}

	key := repository.ServerStatusKey("element")
	if got := cache.ttls[key]; got != 180*time.Second {
		t.Fatalf("TTL 应为 3×60s，实际 %s", got)
	}

	view, err := svc.GetStatus(context.Background(), "element")
	if err != nil || view == nil {
		t.Fatalf("查询应命中: err=%v view=%v", err, view)
	}
	if view.Online != 2 || view.MaxPlayers != 20 {
		t.Fatalf("人数错误: %+v", view)
	}
	if len(view.Players) != 2 || view.Players[0].Nickname != "Bot1" {
		t.Fatalf("玩家列表错误: %+v", view.Players)
	}
	if view.Source != "rcon" || view.UpdatedAt.IsZero() {
		t.Fatalf("元数据错误: source=%q updated_at=%v", view.Source, view.UpdatedAt)
	}

	var rec repository.ServerStatusRecord
	if _, err := cache.GetCache(context.Background(), key, &rec); err != nil {
		t.Fatalf("读取原始记录失败: %v", err)
	}
	if rec.ServerID != 7 || rec.ServerCode != "element" {
		t.Fatalf("归属字段错误: %+v", rec)
	}
}

// TestRecordListUUIDs_ParseFailureKeepsOldValue 解析失败（如中文 locale）不得覆盖已有快照。
func TestRecordListUUIDs_ParseFailureKeepsOldValue(t *testing.T) {
	cache := newFakeStatusCache()
	svc := NewGameStatusService(cache, GameStatusConfig{})
	if err := svc.RecordListUUIDs(context.Background(), 7, "element", testListUUIDsEcho); err != nil {
		t.Fatalf("首次采集失败: %v", err)
	}

	if err := svc.RecordListUUIDs(context.Background(), 7, "element", "当前有 0 名玩家，最多 20 名玩家在线："); err == nil {
		t.Fatal("本地化回显应返回 error")
	}

	view, err := svc.GetStatus(context.Background(), "element")
	if err != nil || view == nil {
		t.Fatalf("旧快照不应被删除: err=%v view=%v", err, view)
	}
	if view.Online != 2 || len(view.Players) != 2 {
		t.Fatalf("旧快照被覆盖: %+v", view)
	}
}

// TestRecordListUUIDs_EmptyValueNotWritten Redis 写失败时不留下半成品。
func TestRecordListUUIDs_CacheFailure(t *testing.T) {
	cache := newFakeStatusCache()
	cache.setErr = errors.New("redis down")
	svc := NewGameStatusService(cache, GameStatusConfig{})
	if err := svc.RecordListUUIDs(context.Background(), 7, "element", testListUUIDsEcho); err == nil {
		t.Fatal("写缓存失败应返回 error")
	}
}

// TestGetStatus_NotFound 键不存在返回 (nil, nil)：属正常业务分支，不用 error 表达。
func TestGetStatus_NotFound(t *testing.T) {
	svc := NewGameStatusService(newFakeStatusCache(), GameStatusConfig{})
	view, err := svc.GetStatus(context.Background(), "nope")
	if err != nil || view != nil {
		t.Fatalf("未命中应返回 (nil, nil)，实际 (%v, %v)", view, err)
	}
}

// TestGetStatus_RedisFailure Redis 故障必须返回 error（渲染「暂时不可用」而非「未找到」）。
func TestGetStatus_RedisFailure(t *testing.T) {
	cache := newFakeStatusCache()
	cache.getErr = errors.New("redis down")
	svc := NewGameStatusService(cache, GameStatusConfig{})
	_, err := svc.GetStatus(context.Background(), "element")
	if err == nil || !errors.Is(err, ErrInternal) {
		t.Fatalf("应返回 ErrInternal 包裹的错误，实际 %v", err)
	}
}

// TestClearStatus 断连清理必须删除键；删除失败返回 error（TTL 兜底）。
func TestClearStatus(t *testing.T) {
	cache := newFakeStatusCache()
	svc := NewGameStatusService(cache, GameStatusConfig{})
	if err := svc.RecordListUUIDs(context.Background(), 7, "element", testListUUIDsEcho); err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if err := svc.ClearStatus(context.Background(), "element"); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if view, _ := svc.GetStatus(context.Background(), "element"); view != nil {
		t.Fatal("清理后不应再命中")
	}

	cache.delErr = errors.New("redis down")
	if err := svc.ClearStatus(context.Background(), "element"); err == nil {
		t.Fatal("清理失败应返回 error")
	}
}

// TestNewGameStatusService_TTLConfig 显式 TTL 与周期推导两条路径。
func TestNewGameStatusService_TTLConfig(t *testing.T) {
	if got := NewGameStatusService(nil, GameStatusConfig{}).TTL(); got != 180*time.Second {
		t.Fatalf("默认 TTL 应为 180s，实际 %s", got)
	}
	if got := NewGameStatusService(nil, GameStatusConfig{PollInterval: 30 * time.Second}).TTL(); got != 90*time.Second {
		t.Fatalf("按周期推导应为 90s，实际 %s", got)
	}
	if got := NewGameStatusService(nil, GameStatusConfig{TTL: time.Minute}).TTL(); got != time.Minute {
		t.Fatalf("显式 TTL 应生效，实际 %s", got)
	}
}
