package biz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

const (
	// GameEventDefaultLimit 事件查询默认条数（HTTP 接口文档 §13.2）。
	GameEventDefaultLimit = 50
	// GameEventMaxLimit 事件查询单页上限；超过不做静默截断，直接返回参数错误。
	GameEventMaxLimit = 100

	// gameEventRateLimitWindow / Max 事件查询限流窗口与上限（subject 为登录用户 user_id）。
	// 该接口支撑前端上滑翻页与断线补齐，配额需显著高于管理类接口。
	gameEventRateLimitWindow = 5 * time.Minute
	gameEventRateLimitMax    = 300
)

// GameEventReader 是 GameEventQueryService 对 game_events 读取的依赖（便于测试注入 fake）。
// 接口定义在 biz 层，由 repository.GameEventRepo 实现；biz 不依赖具体仓储实现。
type GameEventReader interface {
	QueryGameEvents(ctx context.Context, q repository.GameEventQuery) ([]*repository.GameEventRecord, bool, error)
}

// GameServerResolver 是「按 code 解析可访问服务器」的依赖，由 GameServerService 实现：
// code 格式非法 → ErrServerCodeInvalid；不存在 → ErrServerNotFound；已停用 → ErrServerDisabled。
type GameServerResolver interface {
	ResolveSubscribableServer(ctx context.Context, code string) (*repository.GameServerRecord, error)
}

var (
	// 编译期断言：实现方必须满足依赖接口。
	_ GameEventReader    = (*repository.GameEventRepo)(nil)
	_ GameServerResolver = (*GameServerService)(nil)
)

// gameEventInternalOnlyTypes 是不对外暴露的事件类型：只落库供内部审计，
// 不出现在 HTTP 事件查询与 WebSocket 推送中（见《游戏角色绑定与解绑闭环方案》§8）。
// 定义在 biz 层而非 transport：可见性属于数据策略，任何查询入口都必须遵守。
var gameEventInternalOnlyTypes = []string{protocol.SubTypePlayerCommand}

// GameEventQueryParams 是事件查询的入参（来自 HTTP query）。
// Limit 为 nil 表示未提供，按 GameEventDefaultLimit 处理；显式提供则必须落在 1..100
// （用指针区分「未提供」与「显式传 0」，后者是非法值）。Before / After 为空串表示未提供。
type GameEventQueryParams struct {
	Limit  *int
	Before string
	After  string
}

// GameEventItem 是事件查询的单条结果（已含游标）。
// ID 是持久化文档的 MongoDB _id，transport 层以 24 位十六进制形式作为对外 `id`；
// Cursor 与 WebSocket 推送的 payload.cursor 由同一函数、同一输入生成，因此两者恒等。
type GameEventItem struct {
	ID         primitive.ObjectID
	ServerID   int64
	ServerName string
	EventType  string
	Player     protocol.QueqiaoPlayer
	OccurredAt time.Time
	ReceivedAt time.Time
	Data       protocol.GameEventPushData // 与 WS 推送同构，保证两处 data 语义一致
	Cursor     string
}

// GameEventPage 是一页事件结果。
//
// 游标语义（HTTP 接口文档 §13.4）：
//   - OldestCursor / NewestCursor 描述本页边界；
//   - NextCursor 只表示「沿当前方向继续翻页」：默认（最新）/ before → 更早，after → 更新；
//   - HasMore 表示沿 NextCursor 方向是否还有下一页（由多取 1 条探测）。
type GameEventPage struct {
	Items        []*GameEventItem
	OldestCursor *string
	NewestCursor *string
	NextCursor   *string
	HasMore      bool
}

// GameEventQueryConfig 事件查询的可调参数（为零时使用包内默认值）。
type GameEventQueryConfig struct {
	RateLimitWindow time.Duration
	RateLimitMax    int64
}

// GameEventQueryService 提供成游戏事件的历史查询与增量查询能力。
// 平台无关，不依赖 WS 运行时上下文（见 AGENTS.md §3.1）；
// 与 GameEventService（写入）职责分离，避免把「落库」与「查询」耦合成一个巨型服务。
type GameEventQueryService struct {
	reader  GameEventReader
	servers GameServerResolver
	rl      ServerRateLimiter
	cfg     GameEventQueryConfig
}

// NewGameEventQueryService 构造事件查询服务。reader 与 servers 不允许为 nil。
func NewGameEventQueryService(reader GameEventReader, servers GameServerResolver, rl ServerRateLimiter, cfg GameEventQueryConfig) *GameEventQueryService {
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = gameEventRateLimitWindow
	}
	if cfg.RateLimitMax <= 0 {
		cfg.RateLimitMax = gameEventRateLimitMax
	}
	return &GameEventQueryService{reader: reader, servers: servers, rl: rl, cfg: cfg}
}

// Query 按 limit / before / after 查询指定服务器的事件。
//
// 流程：参数校验（limit 区间、before 与 after 互斥、游标解码）→ 限流（fail-open）
// → 解析服务器（400 / 404 / 403）→ 查询 → 组装游标。
// 空结果不是错误：返回空 Items 与 has_more=false。
//
// actorKey 为限流 subject（登录用户 user_id），由 transport 层传入。
func (s *GameEventQueryService) Query(ctx context.Context, actorKey, serverCode string, p GameEventQueryParams) (*GameEventPage, error) {
	limit, dir, cursor, err := normalizeGameEventQuery(p)
	if err != nil {
		return nil, err
	}

	allowed, rlErr := s.rl.AllowAttempt(ctx, repository.UserRateLimitKey("game_events", actorKey), s.cfg.RateLimitWindow, s.cfg.RateLimitMax)
	if rlErr != nil {
		// fail-open：仅记录（具体日志由 transport 层处理），不因 Redis 故障锁死查询。
		_ = rlErr
	}
	if !allowed {
		return nil, ErrRateLimited
	}

	// TODO(visibility): 角色系统 / server_game_bindings 落地后，此处须与 WS 推送（ws_push.go）共用同一套
	// 可见性（公开/隐身）判定，不能只在某一入口单独实现。
	rec, err := s.servers.ResolveSubscribableServer(ctx, serverCode)
	if err != nil {
		return nil, err
	}

	records, hasMore, err := s.reader.QueryGameEvents(ctx, repository.GameEventQuery{
		ServerID:  rec.ID, // 归属来自已校验的 server_code，不接受前端传入 server_id
		Direction: dir,
		Cursor:    cursor,
		Limit:     limit,
		// player_command 是内部审计事件，命令原文可能含绑定码等敏感参数，不对外返回。
		// 规则集中在 biz 层，保证任何查询入口都不会漏掉（详见方案 §8）。
		ExcludeEventTypes: gameEventInternalOnlyTypes,
	})
	if err != nil {
		return nil, Wrap(ErrInternal, err, "查询游戏事件失败")
	}

	page := &GameEventPage{Items: make([]*GameEventItem, 0, len(records)), HasMore: hasMore}
	for _, r := range records {
		page.Items = append(page.Items, toGameEventItem(r))
	}
	if n := len(page.Items); n > 0 {
		oldest := page.Items[0].Cursor
		newest := page.Items[n-1].Cursor
		page.OldestCursor = &oldest
		page.NewestCursor = &newest
		// 默认（最新）与 before 都是「向前加载历史」，继续翻页用最早游标；
		// after 是「向后补齐更新」，继续翻页用最新游标。
		next := oldest
		if dir == repository.GameEventAfter {
			next = newest
		}
		page.NextCursor = &next
	}
	return page, nil
}

// normalizeGameEventQuery 校验并归一化查询参数。
// 非法参数统一返回 ErrInvalidParam（携带具体上下文供日志输出），不区分对外的细分原因。
func normalizeGameEventQuery(p GameEventQueryParams) (int, repository.GameEventDirection, repository.GameEventCursorPoint, error) {
	var zero repository.GameEventCursorPoint

	limit := GameEventDefaultLimit
	if p.Limit != nil {
		limit = *p.Limit
		if limit < 1 || limit > GameEventMaxLimit {
			return 0, repository.GameEventLatest, zero,
				Wrap(ErrInvalidParam, nil, "limit 必须是 1-100 的整数")
		}
	}

	if p.Before != "" && p.After != "" {
		return 0, repository.GameEventLatest, zero,
			Wrap(ErrInvalidParam, nil, "before 与 after 不能同时提供")
	}

	dir := repository.GameEventLatest
	raw := ""
	switch {
	case p.Before != "":
		dir, raw = repository.GameEventBefore, p.Before
	case p.After != "":
		dir, raw = repository.GameEventAfter, p.After
	}
	if raw == "" {
		return limit, dir, zero, nil
	}

	cursor, err := decodeGameEventCursor(raw)
	if err != nil {
		return 0, dir, zero, err
	}
	return limit, dir, cursor, nil
}

// gameEventCursor 是游标的明文结构（编码前的 JSON 形态）。
// 前端不得解析，只做原样回传；字段名保持最短以控制 URL 长度。
type gameEventCursor struct {
	ReceivedAt int64  `json:"t"`  // received_at 的 Unix 毫秒
	ID         string `json:"id"` // MongoDB ObjectId 的 24 位十六进制
}

// encodeGameEventCursor 把「received_at + _id」编码为 base64url(无 padding) 的不透明游标。
//
// 这是游标生成的**唯一入口**：HTTP 查询（toGameEventItem）与 WebSocket 推送（Ingest 返回的
// GameEventView.Cursor）都调用本函数。只要两边的 receivedAt 已按毫秒截断、ID 同源于持久化文档，
// 同一事件在两条链路上必然得到完全相同的游标——不要在其他位置另写一套编码逻辑。
func encodeGameEventCursor(receivedAt time.Time, id primitive.ObjectID) string {
	b, err := json.Marshal(gameEventCursor{ReceivedAt: receivedAt.UnixMilli(), ID: id.Hex()})
	if err != nil {
		// 结构固定（两个基本类型字段），序列化不会失败；兜底返回空串使该条不产生游标。
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeGameEventCursor 解码并校验游标：base64url 可解、JSON 结构合法、t > 0、id 为合法 ObjectId。
// 任一环节失败均映射为 ErrInvalidParam（对外 400），不暴露内部细节。
func decodeGameEventCursor(raw string) (repository.GameEventCursorPoint, error) {
	var point repository.GameEventCursorPoint

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return point, Wrap(ErrInvalidParam, err, "游标 base64 解码失败")
	}
	var c gameEventCursor
	if err := json.Unmarshal(decoded, &c); err != nil {
		return point, Wrap(ErrInvalidParam, err, "游标 JSON 解析失败")
	}
	if c.ReceivedAt <= 0 || c.ID == "" {
		return point, Wrap(ErrInvalidParam, nil, "游标缺少必填字段")
	}
	id, err := primitive.ObjectIDFromHex(c.ID)
	if err != nil {
		return point, Wrap(ErrInvalidParam, err, "游标 id 非法")
	}

	point.ReceivedAt = time.UnixMilli(c.ReceivedAt).UTC()
	point.ID = id
	return point, nil
}

// toGameEventItem 把仓储记录转换为查询结果项，并按事件类型筛选下发的 data 字段。
// 规则与 WS 推送（protocol.GameEventPushData）一致：
//   - join/quit：data 为空对象；
//   - chat：message / raw_message / message_id；
//   - command：command / message_id（raw_message 为 Java 对象 toString，无业务价值，不下发）。
//
// 零值字段由 JSON omitempty 省略。
func toGameEventItem(r *repository.GameEventRecord) *GameEventItem {
	item := &GameEventItem{
		ID:         r.ID,
		ServerID:   r.ServerID,
		ServerName: r.ServerName,
		EventType:  r.EventType,
		Player: protocol.QueqiaoPlayer{
			Nickname: r.Player.Nickname,
			UUID:     r.Player.UUID,
			IsOp:     r.Player.IsOp,
		},
		OccurredAt: r.OccurredAt.UTC(),
		ReceivedAt: r.ReceivedAt.UTC(),
		Cursor:     encodeGameEventCursor(r.ReceivedAt, r.ID),
	}
	switch r.EventType {
	case "player_chat":
		item.Data = protocol.GameEventPushData{
			Message:    r.Data.Message,
			MessageID:  r.Data.MessageID,
			RawMessage: r.Data.RawMessage,
		}
	case "player_command":
		item.Data = protocol.GameEventPushData{
			Command:   r.Data.Command,
			MessageID: r.Data.MessageID,
		}
	}
	return item
}
