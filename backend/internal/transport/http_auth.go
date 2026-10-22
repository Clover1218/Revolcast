package transport

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"revolcast-backend/internal/biz"

	"go.uber.org/zap"
)

// sessionCookieName 是登录会话 Token 的 Cookie 名（HttpOnly，见 HTTP 接口文档 §5.3）。
const sessionCookieName = "revolcast_session"

// AuthHandler 处理 /api/v1/auth/* 的 HTTP 请求，是网站/外部客户端进入 Backend 的入口之一。
type AuthHandler struct {
	svc          *biz.AuthService
	log          *zap.Logger
	secureCookie bool // 生产环境置 true，使会话 Cookie 仅经 HTTPS 传输（见 HTTP 接口文档 §5.3）
}

func NewAuthHandler(svc *biz.AuthService, logger *zap.Logger, secureCookie bool) *AuthHandler {
	return &AuthHandler{svc: svc, log: logger.With(zap.String("module", "HTTPAuth")), secureCookie: secureCookie}
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

// writeBizError 将 biz 错误映射为标准响应 + HTTP 状态码（HTTP 接口文档 §6 / §10）。
// 文案、状态码、业务 code 全部取自 biz.ResolveErrorMeta（单一映射源：biz/errors.go），
// 不在 transport 层散落硬编码，新增错误只需在 biz 注册表登记。
func writeBizError(w http.ResponseWriter, err error) {
	meta := biz.ResolveErrorMeta(err)
	writeJSON(w, meta.HTTPStatus, meta.HTTPCode, meta.HTTPMsg, nil)
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
		logErr(h.log, "check-name 失败", err)
		writeBizError(w, err)
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
		logErr(h.log, "register 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", nil)
}

// loginRequest 是 POST /api/v1/auth/login 的请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginData 是 login 成功响应的 data 体（Token 不进 body，由 HttpOnly Cookie 下发）。
type loginData struct {
	ExpiresAt string `json:"expires_at"` // RFC3339 UTC
}

// Login POST /api/v1/auth/login
// 业务：网站账号登录（BIZ-004）。成功后将原始 Token 写入 HttpOnly Cookie，body 仅返回过期时间。
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}

	deviceName := r.UserAgent() // device_name 由 User-Agent 推导（web_sessions 允许 NULL）
	rawToken, expiresAt, err := h.svc.Login(r.Context(), clientIP(r), req.Username, req.Password, deviceName)
	if err != nil {
		logErr(h.log, "login 失败", err)
		writeBizError(w, err)
		return
	}

	// 原始 Token 仅通过 HttpOnly Cookie 下发，body 不返回明文 Token（防 XSS 窃取）。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
	writeJSON(w, http.StatusOK, 200, "success", loginData{ExpiresAt: expiresAt.UTC().Format(time.RFC3339)})
}

// Logout POST /api/v1/auth/logout
// 业务：账号登出（BIZ-005）。读取 HttpOnly Cookie 中的原始 Token，撤销对应会话并清除本地 Cookie。
// 未携带有效 Cookie 或会话已失效（撤销/过期/不存在）返回 401；成功返回 200 并清除 Cookie。
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	// 未携带 Cookie 或值为空：本就是未登录态，尽量清除（若存在）后返回 401。
	if err != nil || cookie.Value == "" {
		h.clearSessionCookie(w)
		writeJSON(w, http.StatusUnauthorized, 401, "not authenticated", nil)
		return
	}
	if err := h.svc.Logout(r.Context(), cookie.Value); err != nil {
		if errors.Is(err, biz.ErrNotAuthenticated) {
			// 会话已失效（撤销/过期/令牌不存在）：清除本地 Cookie 并 401。
			h.clearSessionCookie(w)
			writeJSON(w, http.StatusUnauthorized, 401, "not authenticated", nil)
			return
		}
		// 数据库等内部错误：保留 Cookie 以便重试，不盲目清除。
		logErr(h.log, "logout 失败", err)
		writeBizError(w, err)
		return
	}
	h.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, 200, "success", nil)
}

// clearSessionCookie 下发同名空 Cookie 让浏览器删除本地会话凭证。
// 属性须与 Login 下发时一致（Path/HttpOnly/SameSite/Secure），否则浏览器可能不匹配而不删除。
func (h *AuthHandler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

// CORSMiddleware 为 API 路由附加 CORS 头并处理预检（OPTIONS）。
// 开发期允许任意来源；生产期应改为按白名单设置 Access-Control-Allow-Origin。
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 会话 Cookie 需随跨站请求携带，故按请求 Origin 反射并允许凭据；
		// 生产期应将 Allow-Origin 收敛为受信前端域名白名单（见 HTTP 接口文档 §5.3）。
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
