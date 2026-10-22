package transport

import (
	"context"
	"net/http"
	"strconv"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"

	"go.uber.org/zap"
)

// eventTimeLayout 是事件时间对外的统一格式：UTC RFC3339，固定毫秒精度。
// 与 MongoDB BSON Date 的毫秒精度一致，避免 RFC3339Nano 裁剪尾部 0 导致的格式漂移。
const eventTimeLayout = "2006-01-02T15:04:05.000Z"

// GameEventQueryer 是事件查询处理器对业务层的依赖（便于测试注入 fake）。
type GameEventQueryer interface {
	Query(ctx context.Context, actorKey, serverCode string, p biz.GameEventQueryParams) (*biz.GameEventPage, error)
}

// GameEventHandler 处理 /api/v1/game-servers/{server_code}/events 的 HTTP 请求。
//
// 鉴权：需携带有效会话 Cookie（revolcast_session），未登录 / 会话失效 → 401。
// 可见性边界与 WS 订阅（ws_web.go）一致：仅校验「已登录 + 服务器存在且启用」，
// 尚未接入 server_game_bindings 归属与 visibility 过滤（见 biz.GameEventQueryService 的 TODO(visibility)）。
type GameEventHandler struct {
	svc  GameEventQueryer
	auth WebSessionAuth
	log  *zap.Logger
}

func NewGameEventHandler(svc GameEventQueryer, auth WebSessionAuth, logger *zap.Logger) *GameEventHandler {
	return &GameEventHandler{svc: svc, auth: auth, log: logger.With(zap.String("module", "HTTPGameEvent"))}
}

// gameEventItemView 是单条事件的对外视图（不暴露 MongoDB _id，游标独立提供）。
type gameEventItemView struct {
	EventType  string                     `json:"event_type"`
	ServerName string                     `json:"server_name"`
	ServerID   int64                      `json:"server_id"`
	Player     protocol.QueqiaoPlayer     `json:"player"`
	OccurredAt string                     `json:"occurred_at"`
	ReceivedAt string                     `json:"received_at"`
	Data       protocol.GameEventPushData `json:"data"`
	Cursor     string                     `json:"cursor"`
}

// gameEventPageView 是一页事件的对外视图。
// Items 恒为非 nil 切片，空结果序列化为 []，前端无需区分 null。
type gameEventPageView struct {
	Items        []*gameEventItemView `json:"items"`
	OldestCursor *string              `json:"oldest_cursor"`
	NewestCursor *string              `json:"newest_cursor"`
	NextCursor   *string              `json:"next_cursor"`
	HasMore      bool                 `json:"has_more"`
}

// Events GET /api/v1/game-servers/{server_code}/events
// 业务：查询指定服务器的游戏事件历史/增量（HTTP 接口文档 §13）。
// 参数：?limit=（默认 50，1..100）、?before=（更早历史）、?after=（增量补齐）；before 与 after 互斥。
func (h *GameEventHandler) Events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, 405, "method not allowed", nil)
		return
	}

	// 1. 会话鉴权：未携带 / 失效 / DB 异常统一按 biz 错误注册表映射（401 / 500）。
	cookie, cerr := r.Cookie(sessionCookieName)
	if cerr != nil || cookie.Value == "" {
		writeBizError(w, biz.ErrNotAuthenticated)
		return
	}
	userID, err := h.auth.ResolveUserID(r.Context(), cookie.Value)
	if err != nil {
		logErr(h.log, "events 会话鉴权失败", err)
		writeBizError(w, err)
		return
	}

	// 2. limit：仅解析「是否为整数」，区间校验（1..100）由业务层统一负责。
	//    未提供时传 nil，业务层取默认值；显式传入（含 0 / 负数）一律交给业务层判非法。
	q := r.URL.Query()
	var limit *int
	if lv := q.Get("limit"); lv != "" {
		v, perr := strconv.Atoi(lv)
		if perr != nil {
			writeBizError(w, biz.Wrap(biz.ErrInvalidParam, perr, "limit 不是整数"))
			return
		}
		limit = &v
	}

	// 3. 查询（参数互斥 / 游标格式 / 服务器校验均在业务层完成）。
	page, err := h.svc.Query(r.Context(), strconv.FormatInt(userID, 10), r.PathValue("server_code"),
		biz.GameEventQueryParams{
			Limit:  limit,
			Before: q.Get("before"),
			After:  q.Get("after"),
		})
	if err != nil {
		logErr(h.log, "events 查询失败", err)
		writeBizError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, 200, "success", toGameEventPageView(page))
}

// toGameEventPageView 把业务结果转换为对外视图；Items 始终初始化为非 nil 切片。
func toGameEventPageView(page *biz.GameEventPage) *gameEventPageView {
	view := &gameEventPageView{
		Items:        make([]*gameEventItemView, 0, len(page.Items)),
		OldestCursor: page.OldestCursor,
		NewestCursor: page.NewestCursor,
		NextCursor:   page.NextCursor,
		HasMore:      page.HasMore,
	}
	for _, item := range page.Items {
		view.Items = append(view.Items, &gameEventItemView{
			EventType:  item.EventType,
			ServerName: item.ServerName,
			ServerID:   item.ServerID,
			Player:     item.Player,
			OccurredAt: item.OccurredAt.UTC().Format(eventTimeLayout),
			ReceivedAt: item.ReceivedAt.UTC().Format(eventTimeLayout),
			Data:       item.Data,
			Cursor:     item.Cursor,
		})
	}
	return view
}
