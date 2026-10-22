package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"golang.org/x/crypto/bcrypt"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/repository"
)

// ---- fakes（实现 biz 导出的接口）----

type fakeUserStore struct {
	exists       bool
	createErr    error
	createCalled bool
	user         *repository.UserRecord
	getUserErr   error
}

func (f *fakeUserStore) ExistsByUsername(_ context.Context, _ string) (bool, error) {
	return f.exists, nil
}

func (f *fakeUserStore) GetByUsername(_ context.Context, _ string) (*repository.UserRecord, error) {
	if f.getUserErr != nil {
		return nil, f.getUserErr
	}
	if f.user == nil {
		return nil, repository.ErrUserNotFound
	}
	return f.user, nil
}

func (f *fakeUserStore) CreateUser(_ context.Context, _, _ string) error {
	f.createCalled = true
	return f.createErr
}

type fakeRL struct{ allowed bool }

func (f *fakeRL) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, nil
}

type fakeSessionStore struct {
	createCalled   bool
	revokeAffected bool
	revokeErr      error
}

func (f *fakeSessionStore) CreateSession(_ context.Context, _ int64, _, _ string, _ time.Time) error {
	f.createCalled = true
	return nil
}

func (f *fakeSessionStore) RevokeSession(_ context.Context, _ string) (bool, error) {
	return f.revokeAffected, f.revokeErr
}

func newTestAuthHandler(createErr error, exists, allowed bool) *AuthHandler {
	svc := biz.NewAuthService(
		&fakeUserStore{exists: exists, createErr: createErr},
		&fakeRL{allowed: allowed},
		&fakeSessionStore{},
		biz.AuthConfig{},
	)
	return NewAuthHandler(svc, zap.NewNop(), false)
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
	mux.Handle("/api/v1/auth/login", CORSMiddleware(http.HandlerFunc(h.Login)))
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

	// 3) POST login（默认 fake 无用户 → 凭证错误，401；同时验证路由已注册、CORS 凭据头存在）。
	lreq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/login", bytes.NewReader([]byte(`{"username":"clover","password":"password123"}`)))
	lresp, err := http.DefaultClient.Do(lreq)
	if err != nil {
		t.Fatalf("login 请求失败: %v", err)
	}
	defer lresp.Body.Close()
	if lresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401（默认无用户），实际 %d", lresp.StatusCode)
	}
	if lresp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("CORS 应允许凭据: %+v", lresp.Header)
	}
}

// ---- Login 处理器单元测试（BIZ-004）----

func TestLogin_Success_SetsHttpOnlyCookie(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), 12)
	store := &fakeUserStore{user: &repository.UserRecord{ID: 1, PasswordHash: string(hash), Status: 1}}
	svc := biz.NewAuthService(store, &fakeRL{allowed: true}, &fakeSessionStore{}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	body, _ := json.Marshal(loginRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Login(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 200 || env.Msg != "success" {
		t.Fatalf("信封异常: %+v", env)
	}
	// body 不应包含明文 token；应包含 expires_at。
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("data 应为对象: %+v", env.Data)
	}
	if _, hasToken := data["token"]; hasToken {
		t.Fatalf("body 不应返回明文 token: %+v", data)
	}
	if data["expires_at"] == nil {
		t.Fatalf("应包含 expires_at: %+v", data)
	}
	// Set-Cookie 应为 HttpOnly。
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("应下发会话 Cookie %s", sessionCookieName)
	}
	if !found.HttpOnly {
		t.Fatalf("会话 Cookie 必须为 HttpOnly")
	}
	if found.Value == "" {
		t.Fatalf("会话 Cookie 不应为空")
	}
}

func TestLogin_WrongPassword_Returns401(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("rightpass"), 12)
	store := &fakeUserStore{user: &repository.UserRecord{ID: 1, PasswordHash: string(hash), Status: 1}}
	svc := biz.NewAuthService(store, &fakeRL{allowed: true}, &fakeSessionStore{}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	body, _ := json.Marshal(loginRequest{Username: "clover", Password: "wrongpass"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Login(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 401 || env.Msg != "invalid credentials" {
		t.Fatalf("信封异常: %+v", env)
	}
}

func TestLogin_Disabled_Returns403(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), 12)
	store := &fakeUserStore{user: &repository.UserRecord{ID: 1, PasswordHash: string(hash), Status: 0}}
	svc := biz.NewAuthService(store, &fakeRL{allowed: true}, &fakeSessionStore{}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	body, _ := json.Marshal(loginRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Login(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %d", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 403 || env.Msg != "account disabled" {
		t.Fatalf("信封异常: %+v", env)
	}
}

func TestLogin_RateLimited_Returns429(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: false}, &fakeSessionStore{}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)
	body, _ := json.Marshal(loginRequest{Username: "clover", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d", rec.Code)
	}
}

// ---- Logout 处理器单元测试（BIZ-005）----

func getSessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

// assertClearsSessionCookie 断言响应下发了一个用于删除会话的空 Cookie。
func assertClearsSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	c := getSessionCookie(rec)
	if c == nil {
		t.Fatalf("应下发清除 Cookie")
	}
	if c.MaxAge != -1 {
		t.Fatalf("清除 Cookie 的 MaxAge 应为 -1，实际 %d", c.MaxAge)
	}
	if c.Value != "" {
		t.Fatalf("清除 Cookie 的值应为空，实际 %q", c.Value)
	}
	if !c.HttpOnly {
		t.Fatalf("清除 Cookie 必须为 HttpOnly")
	}
}

func TestLogout_Success_ClearsCookie(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{revokeAffected: true}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "raw-token"})
	rec := httptest.NewRecorder()
	h.Logout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 200 || env.Msg != "success" {
		t.Fatalf("信封异常: %+v", env)
	}
	assertClearsSessionCookie(t, rec)
}

func TestLogout_NoCookie_Returns401(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{revokeAffected: false}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	h.Logout(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401（未携带 Cookie），实际 %d", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != 401 || env.Msg != "not authenticated" {
		t.Fatalf("信封异常: %+v", env)
	}
	assertClearsSessionCookie(t, rec)
}

func TestLogout_StaleToken_Returns401(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{revokeAffected: false}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale"})
	rec := httptest.NewRecorder()
	h.Logout(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401（会话已失效），实际 %d", rec.Code)
	}
	assertClearsSessionCookie(t, rec)
}

func TestLogout_DBError_Returns500(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{revokeErr: errors.New("pg down")}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.Logout(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	// DB 错误时不清除 Cookie（保留以便重试）。
	if c := getSessionCookie(rec); c != nil {
		t.Fatalf("DB 错误时不应清除 Cookie，实际 %+v", c)
	}
}

func TestLogout_MethodNotAllowed(t *testing.T) {
	h := NewAuthHandler(biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{}, biz.AuthConfig{}), zap.NewNop(), false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}

// TestLogout_RouteRegistered 验证登出路由已注册（经真实 mux + CORSMiddleware）。
func TestLogout_RouteRegistered(t *testing.T) {
	svc := biz.NewAuthService(&fakeUserStore{}, &fakeRL{allowed: true}, &fakeSessionStore{revokeAffected: true}, biz.AuthConfig{})
	h := NewAuthHandler(svc, zap.NewNop(), false)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/logout", CORSMiddleware(http.HandlerFunc(h.Logout)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "raw-token"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
}
