package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/repository"

	"go.uber.org/zap"
)

// GameServerHandler 处理 /api/v1/game-servers/* 的 HTTP 请求（game_servers 增删改查）。
//
// 鉴权约定（与决策一致）：
//   - 读接口（GET 列表 / GET 详情）完全公开，无需登录。
//   - 管理接口（POST 创建 / PATCH 更新 / DELETE 禁用 / POST 轮换密钥）当前**暂不鉴权**；
//     角色系统引入前请勿在公网直接暴露。代码中以 TODO(role-system) 标注接入点。
type GameServerHandler struct {
	svc *biz.GameServerService
	log *zap.Logger
}

func NewGameServerHandler(svc *biz.GameServerService, logger *zap.Logger) *GameServerHandler {
	return &GameServerHandler{svc: svc, log: logger.With(zap.String("module", "HTTPGameServer"))}
}

// createGameServerRequest 是 POST /api/v1/game-servers 的请求体。
type createGameServerRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"` // 空字符串表示无描述（存 NULL）
}

// updateGameServerRequest 是 PATCH /api/v1/game-servers/{code} 的请求体（均为可选字段）。
// 指针用于区分「未提供」与「显式置空」：Description 非 nil 即更新（空串→NULL）；Status 非 nil 即更新。
type updateGameServerRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Status      *int16  `json:"status"`
}

// gameServerView 是 game_servers 对外视图（绝不暴露 api_secret_hash）。
type gameServerView struct {
	ID          int64   `json:"id"`
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Status      int16   `json:"status"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// createGameServerData 创建响应：视图 + 一次性原始 secret。
type createGameServerData struct {
	*gameServerView
	Secret string `json:"secret"`
}

// rotateSecretData 轮换密钥响应：视图 + 一次性新原始 secret。
type rotateSecretData struct {
	*gameServerView
	Secret string `json:"secret"`
}

// toGameServerView 将仓储记录转换为对外视图（时间统一转 UTC RFC3339，原始 secret 哈希不出现）。
func toGameServerView(r *repository.GameServerRecord) *gameServerView {
	return &gameServerView{
		ID:          r.ID,
		Code:        r.Code,
		Name:        r.Name,
		Description: r.Description,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// Collection 分发 /api/v1/game-servers 的 GET（列表）与 POST（创建）。
func (h *GameServerHandler) Collection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.list(w, r)
	case http.MethodPost:
		h.create(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
	}
}

// Item 分发 /api/v1/game-servers/{code} 的 GET（详情）/ PATCH（更新）/ DELETE（禁用）。
func (h *GameServerHandler) Item(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getByCode(w, r)
	case http.MethodPatch:
		h.update(w, r)
	case http.MethodDelete:
		h.disable(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
	}
}

// RotateSecret 处理 /api/v1/game-servers/{code}/rotate-secret（POST 轮换密钥）。
func (h *GameServerHandler) RotateSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}
	h.rotate(w, r)
}

// create POST /api/v1/game-servers
// 业务：创建游戏服务器（game_servers 写入）。返回一次性原始 secret（仅此一次，库只存其哈希）。
// 鉴权：当前不鉴权（TODO(role-system)）。参数非法 → 400；code 冲突 → 409；内部错误 → 500。
func (h *GameServerHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createGameServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}

	rec, secret, err := h.svc.Create(r.Context(), clientIP(r), req.Code, req.Name, req.Description)
	if err != nil {
		logErr(h.log, "create game-server 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", createGameServerData{gameServerView: toGameServerView(rec), Secret: secret})
}

// list GET /api/v1/game-servers
// 业务：分页列出游戏服务器（公开）。支持 ?status=0|1（可选）、?limit=（默认 20，封顶 100）、?offset=（默认 0）。
func (h *GameServerHandler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var statusFilter *int16
	if sv := q.Get("status"); sv != "" {
		v, perr := strconv.ParseInt(sv, 10, 16)
		if perr != nil || (v != 0 && v != 1) {
			writeJSON(w, http.StatusBadRequest, 400, "invalid param", nil)
			return
		}
		s := int16(v)
		statusFilter = &s
	}

	limit := 20
	if lv := q.Get("limit"); lv != "" {
		v, perr := strconv.Atoi(lv)
		if perr != nil || v <= 0 {
			writeJSON(w, http.StatusBadRequest, 400, "invalid param", nil)
			return
		}
		if v > 100 {
			v = 100
		}
		limit = v
	}

	offset := 0
	if ov := q.Get("offset"); ov != "" {
		v, perr := strconv.Atoi(ov)
		if perr != nil || v < 0 {
			writeJSON(w, http.StatusBadRequest, 400, "invalid param", nil)
			return
		}
		offset = v
	}

	recs, err := h.svc.List(r.Context(), clientIP(r), statusFilter, limit, offset)
	if err != nil {
		logErr(h.log, "list game-servers 失败", err)
		writeBizError(w, err)
		return
	}
	views := make([]*gameServerView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, toGameServerView(rec))
	}
	writeJSON(w, http.StatusOK, 200, "success", views)
}

// getByCode GET /api/v1/game-servers/{code}
// 业务：按 code 查询单个服务器（公开）。不存在 → 404。
func (h *GameServerHandler) getByCode(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	rec, err := h.svc.GetByCode(r.Context(), clientIP(r), code)
	if err != nil {
		logErr(h.log, "get game-server 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", toGameServerView(rec))
}

// update PATCH /api/v1/game-servers/{code}
// 业务：部分更新 name/description/status（code 与 secret 不可改）。无字段可更新时原地返回当前记录。
// 鉴权：当前不鉴权（TODO(role-system)）。目标不存在 → 404。
func (h *GameServerHandler) update(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	var req updateGameServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, 400, "invalid json", nil)
		return
	}
	rec, err := h.svc.Update(r.Context(), clientIP(r), code, repository.GameServerUpdate{
		Name:        req.Name,
		Description: req.Description,
		Status:      req.Status,
	})
	if err != nil {
		logErr(h.log, "update game-server 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", toGameServerView(rec))
}

// disable DELETE /api/v1/game-servers/{code}
// 业务：软禁用（status 1→0）。幂等：已禁用仍返回成功（当前记录）。目标不存在 → 404。
// 鉴权：当前不鉴权（TODO(role-system)）。
func (h *GameServerHandler) disable(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	rec, err := h.svc.Disable(r.Context(), clientIP(r), code)
	if err != nil {
		logErr(h.log, "disable game-server 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", toGameServerView(rec))
}

// rotate POST /api/v1/game-servers/{code}/rotate-secret
// 业务：轮换 API Secret，返回一次性新原始 secret（库仅更新其哈希）。目标不存在 → 404。
// 鉴权：当前不鉴权（TODO(role-system)）。
func (h *GameServerHandler) rotate(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	secret, rec, err := h.svc.RotateSecret(r.Context(), clientIP(r), code)
	if err != nil {
		logErr(h.log, "rotate game-server secret 失败", err)
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, 200, "success", rotateSecretData{gameServerView: toGameServerView(rec), Secret: secret})
}
