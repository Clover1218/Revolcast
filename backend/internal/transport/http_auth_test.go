package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/repository"
)

// ---- fakes（实现 biz 导出的接口）----

type fakeUserStore struct {
	exists       bool
	createErr    error
	createCalled bool
}

func (f *fakeUserStore) ExistsByUsername(_ context.Context, _ string) (bool, error) {
	return f.exists, nil
}

func (f *fakeUserStore) CreateUser(_ context.Context, _, _ string) error {
	f.createCalled = true
	return f.createErr
}

type fakeRL struct{ allowed bool }

func (f *fakeRL) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, nil
}

func newTestAuthHandler(createErr error, exists, allowed bool) *AuthHandler {
	svc := biz.NewAuthService(
		&fakeUserStore{exists: exists, createErr: createErr},
		&fakeRL{allowed: allowed},
		biz.AuthConfig{},
	)
	return NewAuthHandler(svc, zap.NewNop())
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析响应失败: %v\nbody=%s", err, body)
	}
	return env
}

// 仅允许 POST，其他方法返回 405。
func TestCheckName_MethodNotAllowed(t *testing.T) {
	h := newTestAuthHandler(nil, false, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/check-name", nil)
	rec := httptest.NewRecorder()
	h.CheckName(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}

func TestCheckName_Success(t *testing.T) {
	h := newTestAuthHandler(nil, false, true)
	body, _ := json.Marshal(checkNameRequest{Username: "Clover"}) // 含大写；重复检测在 SQL 层统一小写，入库保留原大小写
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/check-name", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.CheckName(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 200 || env.Msg != "success" {
		t.Fatalf("信封异常: %+v", env)
	}
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("data 应为对象: %+v", env.Data)
	}
	if data["exists"] != false {
		t.Fatalf("期望 exists=false: %+v", data)
	}
}

func TestRegister_Success_DataNull(t *testing.T) {
	h := newTestAuthHandler(nil, false, true)
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 200 || env.Msg != "success" {
		t.Fatalf("信封异常: %+v", env)
	}
	if env.Data != nil {
		t.Fatalf("注册成功 data 应为 null，实际 %+v", env.Data)
	}
}

func TestRegister_Taken_Returns409(t *testing.T) {
	h := newTestAuthHandler(nil, true, true) // 预检已存在
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("期望 409，实际 %d", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 409 || env.Msg != "username taken" {
		t.Fatalf("信封异常: %+v", env)
	}
}

func TestRegister_UniqueConflictOnInsert_Returns409(t *testing.T) {
	// 预检不存在，但插入触发唯一约束（数据库兜底）。
	h := newTestAuthHandler(repository.ErrUsernameTaken, false, true)
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("期望 409（唯一约束兜底），实际 %d", rec.Code)
	}
}

func TestRegister_InvalidParam_Returns400(t *testing.T) {
	h := newTestAuthHandler(nil, false, true)
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "short"}) // 密码过短
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
}

func TestRegister_RateLimited_Returns429(t *testing.T) {
	h := newTestAuthHandler(nil, false, false) // 限流拒绝
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d", rec.Code)
	}
}

// TestAuthRoutes_Integration 通过真实 httptest.Server 验证 main.go 中的路由注册方式：
// mux + CORSMiddleware + 处理器，覆盖预检与 POST。
func TestAuthRoutes_Integration(t *testing.T) {
	h := newTestAuthHandler(nil, false, true)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/check-name", CORSMiddleware(http.HandlerFunc(h.CheckName)))
	mux.Handle("/api/v1/auth/register", CORSMiddleware(http.HandlerFunc(h.Register)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1) 预检 OPTIONS 应返回 204 且带 CORS 头。
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/v1/auth/register", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS 请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("期望 204，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("缺少 CORS 头: %+v", resp.Header)
	}

	// 2) POST register 成功。
	body, _ := json.Marshal(registerRequest{Username: "clover", Password: "password123"})
	resp2, err := http.Post(srv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST 请求失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp2.StatusCode)
	}
	var env envelope
	_ = json.NewDecoder(resp2.Body).Decode(&env)
	if env.Code != 200 || env.Msg != "success" || env.Data != nil {
		t.Fatalf("信封异常: %+v", env)
	}
}
