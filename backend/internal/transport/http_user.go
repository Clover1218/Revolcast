package transport

import (
	"encoding/json"
	"net/http"

	"revolcast-backend/internal/biz"

	"go.uber.org/zap"
)

// UserHandler 处理 /api/v1/user/* 的 HTTP 请求（已认证接口，需有效会话）。
type UserHandler struct {
	svc          *biz.QQBindService
	log          *zap.Logger
	secureCookie bool // 与 AuthHandler 一致的 Cookie 安全策略（见 HTTP 接口文档 §5.3）
}

func NewUserHandler(svc *biz.QQBindService, logger *zap.Logger, secureCookie bool) *UserHandler {
	return &UserHandler{svc: svc, log: logger.With(zap.String("module", "HTTPUser")), secureCookie: secureCookie}
}

// bindQQRequest 是 POST /api/v1/user/bind-QQ 的请求体（snake_case，见 HTTP 接口文档）。
type bindQQRequest struct {
	BindCode string `json:"bind_code"`
}

// BindQQ POST /api/v1/user/bind-QQ
// 业务：网站赎回 QQ 绑定码（BIZ-006）。登录用户提交 QQ 机器人下发的绑定码，原子消费并写入 qq_bindings。
// 鉴权：读取 HttpOnly Cookie(revolcast_session) 中的原始 Token 并交由业务层解析会话；
// 未携带有效 Cookie / 会话已失效 → 401。其余错误映射见 writeBizError（HTTP 接口文档 §6）。
func (h *UserHandler) BindQQ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeBizError(w, biz.ErrNotAuthenticated)
		return
	}

	var req bindQQRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}

	if err := h.svc.BindQQ(r.Context(), cookie.Value, req.BindCode); err != nil {
		logErr(h.log, "bind-QQ 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", nil)
}

// UnbindQQ POST /api/v1/user/unbind-QQ
// 业务：解绑当前登录账号的 QQ 绑定（BIZ-007）。只校验会话：无请求体、无绑定码入参。
// 鉴权：读取 HttpOnly Cookie(revolcast_session) 中的原始 Token 并交由业务层解析会话；
// 未携带有效 Cookie / 会话已失效 → 401；当前无有效绑定 → 409（not bound）。
// 成功返回 200 + data=null，不返回被解绑的 qq_uin（最小信息暴露）。
func (h *UserHandler) UnbindQQ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeBizError(w, biz.ErrNotAuthenticated)
		return
	}

	if err := h.svc.UnbindQQ(r.Context(), cookie.Value); err != nil {
		logErr(h.log, "unbind-QQ 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", nil)
}
