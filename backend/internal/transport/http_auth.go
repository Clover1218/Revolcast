package transport

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"revolcast-backend/internal/biz"

	"go.uber.org/zap"
)

// AuthHandler 处理 /api/v1/auth/* 的 HTTP 请求，是网站/外部客户端进入 Backend 的入口之一。
type AuthHandler struct {
	svc *biz.AuthService
	log *zap.Logger
}

func NewAuthHandler(svc *biz.AuthService, logger *zap.Logger) *AuthHandler {
	return &AuthHandler{svc: svc, log: logger.With(zap.String("module", "HTTPAuth"))}
}

// clientIP 提取客户端 IP：优先 X-Forwarded-For 最左可信值，否则 RemoteAddr 去端口。
// 注意：若部署在反向代理后，需由代理正确设置 X-Forwarded-For 且本服务信任该代理，
// 否则可能被伪造（HTTP 接口文档 §5 待确认项）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// envelope 是 HTTP 接口标准响应结构（HTTP 接口文档 §1.1）。
type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

func writeJSON(w http.ResponseWriter, httpStatus, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(envelope{Code: code, Msg: msg, Data: data})
}

// writeBizError 将 biz 错误映射为标准响应 + HTTP 状态码（HTTP 接口文档 §6）。
func (h *AuthHandler) writeBizError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalidParam):
		writeJSON(w, http.StatusBadRequest, 400, "invalid param", nil)
	case errors.Is(err, biz.ErrUsernameTaken):
		writeJSON(w, http.StatusConflict, 409, "username taken", nil)
	case errors.Is(err, biz.ErrRateLimited):
		writeJSON(w, http.StatusTooManyRequests, 429, "rate limited", nil)
	default:
		writeJSON(w, http.StatusInternalServerError, 500, "internal error", nil)
	}
}

// checkNameRequest 是 POST /api/v1/auth/check-name 的请求体。
type checkNameRequest struct {
	Username string `json:"username"`
}

// checkNameData 是 check-name 响应的 data 体。
type checkNameData struct {
	Exists bool `json:"exists"`
}

// CheckName POST /api/v1/auth/check-name
// 业务：校验用户名可用性（BIZ-002）。
func (h *AuthHandler) CheckName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}
	var req checkNameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}

	exists, err := h.svc.CheckUsername(r.Context(), clientIP(r), req.Username)
	if err != nil {
		h.log.Warn("check-name 失败", zap.Error(err))
		h.writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", checkNameData{Exists: exists})
}

// registerRequest 是 POST /api/v1/auth/register 的请求体。
type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Register POST /api/v1/auth/register
// 业务：注册网站账号（BIZ-003）。
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}

	if err := h.svc.Register(r.Context(), clientIP(r), req.Username, req.Password); err != nil {
		h.log.Warn("register 失败", zap.Error(err))
		h.writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", nil)
}

// CORSMiddleware 为 API 路由附加 CORS 头并处理预检（OPTIONS）。
// 开发期允许任意来源；生产期应改为按白名单设置 Access-Control-Allow-Origin。
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // TODO: 生产期按白名单
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
