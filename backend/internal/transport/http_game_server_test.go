package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/repository"

	"go.uber.org/zap"
)

// ---- fakes ----

type fakeGSStore struct {
	created    *repository.GameServerRecord
	createErr  error
	list       []*repository.GameServerRecord
	listErr    error
	byCode     *repository.GameServerRecord
	getErr     error
	updated    *repository.GameServerRecord
	updateErr  error
	disabled   *repository.GameServerRecord
	disableErr error
	rotated    *repository.GameServerRecord
	rotateErr  error
}

func (f *fakeGSStore) CreateGameServer(_ context.Context, code, name, description, _ string) (*repository.GameServerRecord, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	rec := f.created
	if rec == nil {
		rec = &repository.GameServerRecord{ID: 1, Code: code, Name: name, Status: 1}
	}
	return rec, nil
}
func (f *fakeGSStore) ListGameServers(_ context.Context, _ *int16, _ int, _ int) ([]*repository.GameServerRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}
func (f *fakeGSStore) GetGameServerByCode(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byCode, nil
}
func (f *fakeGSStore) GetGameServerBySecretHash(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	return nil, repository.ErrServerNotFound
}
func (f *fakeGSStore) UpdateGameServer(_ context.Context, _ string, _ repository.GameServerUpdate) (*repository.GameServerRecord, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updated, nil
}
func (f *fakeGSStore) DisableGameServer(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	if f.disableErr != nil {
		return nil, f.disableErr
	}
	return f.disabled, nil
}
func (f *fakeGSStore) RotateGameServerSecret(_ context.Context, _ string, _ string) (*repository.GameServerRecord, error) {
	if f.rotateErr != nil {
		return nil, f.rotateErr
	}
	return f.rotated, nil
}

type fakeGSLimiter struct{ allowed bool }

func (f *fakeGSLimiter) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, nil
}

// ---- helpers ----

type gsEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type gsCreateData struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status int16  `json:"status"`
	Secret string `json:"secret"`
}

func newGSTestMux(h *GameServerHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/game-servers", http.HandlerFunc(h.Collection))
	mux.Handle("/api/v1/game-servers/{code}", http.HandlerFunc(h.Item))
	mux.Handle("/api/v1/game-servers/{code}/rotate-secret", http.HandlerFunc(h.RotateSecret))
	return mux
}

// ---- tests ----

func TestGameServerHandler_Create_Success(t *testing.T) {
	store := &fakeGSStore{created: &repository.GameServerRecord{ID: 1, Code: "survival-01", Name: "生存服", Status: 1}}
	svc := biz.NewGameServerService(store, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	body, _ := json.Marshal(map[string]string{"code": "survival-01", "name": "生存服", "description": "desc"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/game-servers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var env gsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	var d gsCreateData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if d.Secret == "" {
		t.Fatalf("期望返回一次性 secret，实际为空")
	}
	if d.Code != "survival-01" {
		t.Fatalf("期望 code=survival-01，实际 %q", d.Code)
	}
}

func TestGameServerHandler_Create_InvalidCode(t *testing.T) {
	svc := biz.NewGameServerService(&fakeGSStore{}, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	body, _ := json.Marshal(map[string]string{"code": "Bad_Code", "name": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/game-servers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
}

func TestGameServerHandler_List_Success(t *testing.T) {
	store := &fakeGSStore{list: []*repository.GameServerRecord{{ID: 1, Code: "a", Name: "A", Status: 1}}}
	svc := biz.NewGameServerService(store, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/game-servers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var env gsEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var list []gsCreateData
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(list))
	}
}

func TestGameServerHandler_GetByCode_NotFound(t *testing.T) {
	store := &fakeGSStore{getErr: repository.ErrServerNotFound}
	svc := biz.NewGameServerService(store, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/game-servers/missing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", rec.Code)
	}
}

func TestGameServerHandler_RotateSecret_Success(t *testing.T) {
	store := &fakeGSStore{rotated: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := biz.NewGameServerService(store, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/game-servers/a/rotate-secret", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var env gsEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var d gsCreateData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if d.Secret == "" {
		t.Fatalf("期望返回一次性新 secret，实际为空")
	}
}

func TestGameServerHandler_MethodNotAllowed(t *testing.T) {
	svc := biz.NewGameServerService(&fakeGSStore{}, &fakeGSLimiter{allowed: true}, nil, biz.GameServerConfig{})
	h := NewGameServerHandler(svc, zap.NewNop())
	mux := newGSTestMux(h)

	// 集合路径只接受 GET/POST，PUT 应被分发器拒绝为 405。
	req := httptest.NewRequest(http.MethodPut, "/api/v1/game-servers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}
