package transport

import (
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

// ---- fakes（实现 biz.QQBindService 的未导出依赖接口）----

type fakeQQBindSession struct {
	userID int64
	ok     bool
}

func (f *fakeQQBindSession) ResolveSession(_ context.Context, _ string) (int64, bool, error) {
	return f.userID, f.ok, nil
}

type fakeQQBindUserStore struct{ status int16 }

func (f *fakeQQBindUserStore) GetByID(_ context.Context, id int64) (*repository.UserRecord, error) {
	return &repository.UserRecord{ID: id, Status: f.status}, nil
}

type fakeQQBindingStore struct {
	consumeErr error
	unbindErr  error
}

func (f *fakeQQBindingStore) ConsumeBindCodeAndBind(_ context.Context, _ string, _ int64) (int64, string, error) {
	return 0, "", f.consumeErr
}

func (f *fakeQQBindingStore) UnbindByUser(_ context.Context, _ int64) (int64, string, error) {
	return 0, "", f.unbindErr
}

type fakeQQBindLimiter struct{ allowed bool }

func (f *fakeQQBindLimiter) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, nil
}

// newTestUserHandler 组装 UserHandler（日志丢弃，Cookie 非 Secure）。
func newTestUserHandler(sess *fakeQQBindSession, binding *fakeQQBindingStore, allowed bool) *UserHandler {
	svc := biz.NewQQBindService(sess, &fakeQQBindUserStore{status: 1}, binding, &fakeQQBindLimiter{allowed: allowed})
	return NewUserHandler(svc, zap.NewNop(), false)
}

// unbindEnvelope 用于断言标准响应外层（HTTP 接口文档 §1.1）。
type unbindEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func decodeUnbindEnvelope(t *testing.T, rec *httptest.ResponseRecorder) unbindEnvelope {
	t.Helper()
	var env unbindEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return env
}

// ---- UnbindQQ ----

func TestUnbindQQHandler_Success(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{userID: 42, ok: true}, &fakeQQBindingStore{}, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/unbind-QQ", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "raw-token"})
	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	env := decodeUnbindEnvelope(t, rec)
	if env.Code != 200 || env.Msg != "success" {
		t.Fatalf("期望 code=200/msg=success，实际 %+v", env)
	}
	// data 为 null（不返回被解绑的 qq_uin）。
	if string(env.Data) != "null" {
		t.Fatalf("期望 data=null，实际 %s", string(env.Data))
	}
}

func TestUnbindQQHandler_NoCookie_401(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{userID: 42, ok: true}, &fakeQQBindingStore{}, true)

	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, httptest.NewRequest(http.MethodPost, "/api/v1/user/unbind-QQ", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401（未携带会话 Cookie），实际 %d", rec.Code)
	}
}

func TestUnbindQQHandler_SessionInvalid_401(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{ok: false}, &fakeQQBindingStore{}, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/unbind-QQ", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale-token"})
	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401（会话已失效），实际 %d", rec.Code)
	}
}

func TestUnbindQQHandler_NotBound_409(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{userID: 42, ok: true}, &fakeQQBindingStore{unbindErr: repository.ErrNotBound}, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/unbind-QQ", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "raw-token"})
	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("期望 409（当前无有效绑定），实际 %d", rec.Code)
	}
	if env := decodeUnbindEnvelope(t, rec); env.Msg != "not bound" {
		t.Fatalf("期望 msg=not bound，实际 %q", env.Msg)
	}
}

func TestUnbindQQHandler_RateLimited_429(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{userID: 42, ok: true}, &fakeQQBindingStore{}, false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/unbind-QQ", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "raw-token"})
	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d", rec.Code)
	}
}

func TestUnbindQQHandler_NonPost_405(t *testing.T) {
	h := newTestUserHandler(&fakeQQBindSession{userID: 42, ok: true}, &fakeQQBindingStore{}, true)

	rec := httptest.NewRecorder()
	h.UnbindQQ(rec, httptest.NewRequest(http.MethodGet, "/api/v1/user/unbind-QQ", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}
