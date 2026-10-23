package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// ---- fakes ----

// fakeGameBindSvc 实现 handler 依赖的 gameBindService 端口。
type fakeGameBindSvc struct {
	list []biz.AvailableServer

	bindOutcome *biz.GameRoleBindOutcome
	bindErr     error

	bindCalls    int
	lastServerID int64
	lastRawCode  string
	lastPlayer   protocol.QueqiaoPlayer
	lastTraceID  string

	unbindOutcome *biz.GameRoleUnbindOutcome
	unbindErr     error

	unbindCalls int
	lastQQ      int64
	lastCode    string
}

func (f *fakeGameBindSvc) ListAvailableServers(context.Context) ([]biz.AvailableServer, error) {
	return f.list, nil
}

func (f *fakeGameBindSvc) GenerateBindCode(context.Context, int64, string, string) (*biz.GameBindOutcome, error) {
	return &biz.GameBindOutcome{Kind: biz.GameBindOutcomeCode, Code: "ABCD2345"}, nil
}

func (f *fakeGameBindSvc) BindGameRole(_ context.Context, serverID int64, player protocol.QueqiaoPlayer, rawCode, traceID string) (*biz.GameRoleBindOutcome, error) {
	f.bindCalls++
	f.lastServerID, f.lastPlayer, f.lastRawCode, f.lastTraceID = serverID, player, rawCode, traceID
	if f.bindErr != nil {
		return nil, f.bindErr
	}
	if f.bindOutcome != nil {
		return f.bindOutcome, nil
	}
	return &biz.GameRoleBindOutcome{
		Kind:       biz.GameRoleBindOutcomeBound,
		GameName:   player.Nickname,
		ServerCode: "element",
		ServerName: "元素觉醒",
	}, nil
}

func (f *fakeGameBindSvc) UnbindGameRole(_ context.Context, qqUIN int64, serverCode, traceID string) (*biz.GameRoleUnbindOutcome, error) {
	f.unbindCalls++
	f.lastQQ, f.lastCode = qqUIN, serverCode
	if f.unbindErr != nil {
		return nil, f.unbindErr
	}
	if f.unbindOutcome != nil {
		return f.unbindOutcome, nil
	}
	return &biz.GameRoleUnbindOutcome{Kind: biz.GameRoleUnbindOutcomeNotBound, ServerName: "元素觉醒"}, nil
}

// fakeGameNotifier 记录游戏内通知，便于断言「发了什么、发了几条」。
type fakeGameNotifier struct {
	calls      int
	serverID   int64
	player     protocol.QueqiaoPlayer
	color      string
	text       string
	returnTrue bool
}

func (f *fakeGameNotifier) SendText(serverID int64, player protocol.QueqiaoPlayer, color, text string) bool {
	f.calls++
	f.serverID, f.player, f.color, f.text = serverID, player, color, text
	return f.returnTrue
}

// ---- parseGameCommand ----

func TestParseGameCommand(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind gameCommandKind
		arg  string
	}{
		{"标准", "revolcast bind ABCD2345", gameCommandBind, "abcd2345"},
		{"前导斜杠", "/revolcast bind ABCD2345", gameCommandBind, "abcd2345"},
		{"大写", "REVOLCAST BIND ABCD2345", gameCommandBind, "abcd2345"},
		{"多余空白", "  revolcast   bind    ABCD2345 ", gameCommandBind, "abcd2345"},
		{"Forge 命名空间", "revolcast:bind ABCD2345", gameCommandBind, "abcd2345"},
		{"Forge 命名空间带斜杠", "/revolcast:bind abcd2345", gameCommandBind, "abcd2345"},
		// 用法错误要回复提示（不静默），便于玩家自查。
		{"缺参数", "revolcast bind", gameCommandBindUsage, ""},
		{"多参数", "revolcast bind ABCD2345 extra", gameCommandBindUsage, ""},
		// 非 Revolcast 命令、未实现的子命令：静默。
		{"普通命令", "gamemode creative", gameCommandNone, ""},
		{"tp 命令", "tp Steve 0 0 0", gameCommandNone, ""},
		{"空命令", "", gameCommandNone, ""},
		{"仅空白", "   ", gameCommandNone, ""},
		{"前缀不匹配", "revolca bind ABCD2345", gameCommandNone, ""},
		{"其它命名空间", "other:bind ABCD2345", gameCommandNone, ""},
		{"仅前缀", "revolcast", gameCommandNone, ""},
		{"未知子命令", "revolcast unbind ABCD2345", gameCommandNone, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, arg := parseGameCommand(c.raw)
			if kind != c.kind || arg != c.arg {
				t.Fatalf("parseGameCommand(%q) = (%v, %q)，期望 (%v, %q)", c.raw, kind, arg, c.kind, c.arg)
			}
		})
	}
}

// ---- 文案渲染 ----

func TestRenderGameRoleBindOutcome_Bound(t *testing.T) {
	color, text := renderGameRoleBindOutcome(&biz.GameRoleBindOutcome{
		Kind:       biz.GameRoleBindOutcomeBound,
		GameName:   "PlantClover",
		ServerCode: "element",
		ServerName: "元素觉醒",
	})
	if color != protocol.GameColorGreen {
		t.Fatalf("成功应使用绿色，实际 %q", color)
	}
	for _, want := range []string{"✔ 绑定成功", "角色：PlantClover", "元素觉醒（element）", "revolcast server element unbind"} {
		if !strings.Contains(text, want) {
			t.Fatalf("文案缺少 %q:\n%s", want, text)
		}
	}
}

// TestRenderGameRoleBindOutcome_Idempotent 幂等复用成功文案（重复输入同一码时玩家看到「成功」）。
func TestRenderGameRoleBindOutcome_Idempotent(t *testing.T) {
	color, text := renderGameRoleBindOutcome(&biz.GameRoleBindOutcome{
		Kind: biz.GameRoleBindOutcomeIdempotent, GameName: "Steve", ServerCode: "element", ServerName: "元素觉醒",
	})
	if color != protocol.GameColorGreen || !strings.Contains(text, "✔ 绑定成功") {
		t.Fatalf("幂等应复用成功文案，实际 color=%q text=%s", color, text)
	}
}

func TestRenderGameRoleBindOutcome_CodeInvalid(t *testing.T) {
	color, text := renderGameRoleBindOutcome(&biz.GameRoleBindOutcome{
		Kind: biz.GameRoleBindOutcomeCodeInvalid, ServerCode: "element", ServerName: "元素觉醒",
	})
	if color != protocol.GameColorRed {
		t.Fatalf("失败应使用红色，实际 %q", color)
	}
	if !strings.Contains(text, "绑定码无效或已过期") || !strings.Contains(text, "revolcast server element bind") {
		t.Fatalf("码无效文案不符:\n%s", text)
	}
}

func TestRenderGameRoleBindOutcome_AccountBound(t *testing.T) {
	_, text := renderGameRoleBindOutcome(&biz.GameRoleBindOutcome{
		Kind: biz.GameRoleBindOutcomeAccountBound, GameName: "Steve",
	})
	if !strings.Contains(text, "你在本服务器已绑定角色：Steve") {
		t.Fatalf("已绑定文案不符:\n%s", text)
	}
}

func TestRenderGameRoleBindOutcome_RoleTaken(t *testing.T) {
	_, text := renderGameRoleBindOutcome(&biz.GameRoleBindOutcome{Kind: biz.GameRoleBindOutcomeRoleTaken})
	if !strings.Contains(text, "该角色已被其他 Revolcast 账号绑定") {
		t.Fatalf("角色占用文案不符:\n%s", text)
	}
}

func TestRenderGameRoleBindOutcome_Nil(t *testing.T) {
	_, text := renderGameRoleBindOutcome(nil)
	if !strings.Contains(text, biz.ResolveErrorMeta(biz.ErrInternal).WSMessage) {
		t.Fatalf("nil 结论应回退内部错误文案，实际 %q", text)
	}
}

func TestRenderGameRoleUnbindOutcome(t *testing.T) {
	done := renderGameRoleUnbindOutcome(&biz.GameRoleUnbindOutcome{
		Kind: biz.GameRoleUnbindOutcomeDone, ServerName: "元素觉醒", GameName: "Steve",
	})
	if !strings.Contains(done, "已解除在服务器「元素觉醒」的角色绑定：Steve") {
		t.Fatalf("解绑成功文案不符: %s", done)
	}

	notBound := renderGameRoleUnbindOutcome(&biz.GameRoleUnbindOutcome{
		Kind: biz.GameRoleUnbindOutcomeNotBound, ServerName: "元素觉醒",
	})
	if !strings.Contains(notBound, "没有已绑定的角色，无需解绑") {
		t.Fatalf("无绑定文案不符: %s", notBound)
	}

	qqNotBound := renderGameRoleUnbindOutcome(&biz.GameRoleUnbindOutcome{Kind: biz.GameRoleUnbindOutcomeQQNotBound})
	if !strings.Contains(qqNotBound, "revolcast user bind") {
		t.Fatalf("未绑 QQ 文案应引导 user bind: %s", qqNotBound)
	}
}

// ---- 命令分发 ----

func newGameCommandPayload(command string) *protocol.GameEventPayload {
	return &protocol.GameEventPayload{
		SubType:   protocol.SubTypePlayerCommand,
		Command:   command,
		Timestamp: time.Now().Unix(),
		Player: protocol.QueqiaoPlayer{
			UUID:     "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
			Nickname: "PlantClover",
		},
	}
}

// TestHandleGameCommand_BindSendsNotification 绑定成功 → 向该服务器上的玩家发一条绿色私聊。
func TestHandleGameCommand_BindSendsNotification(t *testing.T) {
	svc := &fakeGameBindSvc{}
	notifier := &fakeGameNotifier{returnTrue: true}

	handleGameCommand(context.Background(), 7, "trace-1", newGameCommandPayload("revolcast bind ABCD2345"), svc, notifier, zap.NewNop())

	if svc.bindCalls != 1 {
		t.Fatalf("期望调用绑定业务 1 次，实际 %d", svc.bindCalls)
	}
	if svc.lastServerID != 7 || svc.lastRawCode != "abcd2345" || svc.lastTraceID != "trace-1" {
		t.Fatalf("绑定入参不符: server=%d code=%q trace=%q", svc.lastServerID, svc.lastRawCode, svc.lastTraceID)
	}
	if notifier.calls != 1 || notifier.serverID != 7 || notifier.color != protocol.GameColorGreen {
		t.Fatalf("通知不符: calls=%d server=%d color=%q", notifier.calls, notifier.serverID, notifier.color)
	}
	if !strings.Contains(notifier.text, "✔ 绑定成功") {
		t.Fatalf("通知文案不符: %s", notifier.text)
	}
}

// TestHandleGameCommand_NonRevolcastSilent 普通游戏命令不触达业务、不发通知。
func TestHandleGameCommand_NonRevolcastSilent(t *testing.T) {
	svc := &fakeGameBindSvc{}
	notifier := &fakeGameNotifier{returnTrue: true}

	handleGameCommand(context.Background(), 7, "trace", newGameCommandPayload("gamemode creative"), svc, notifier, zap.NewNop())

	if svc.bindCalls != 0 || notifier.calls != 0 {
		t.Fatalf("普通命令应静默: bind=%d notify=%d", svc.bindCalls, notifier.calls)
	}
}

// TestHandleGameCommand_Usage 缺参数 → 回用法提示，不触达业务。
func TestHandleGameCommand_Usage(t *testing.T) {
	svc := &fakeGameBindSvc{}
	notifier := &fakeGameNotifier{returnTrue: true}

	handleGameCommand(context.Background(), 7, "trace", newGameCommandPayload("revolcast bind"), svc, notifier, zap.NewNop())

	if svc.bindCalls != 0 {
		t.Fatalf("用法错误不应触达业务")
	}
	if notifier.calls != 1 || !strings.Contains(notifier.text, "用法：/revolcast bind <8 位绑定码>") {
		t.Fatalf("应回用法提示，实际 %+v", notifier)
	}
}

// TestHandleGameCommand_RateLimited 限流使用游戏内口气文案（非 QQ 文案）。
func TestHandleGameCommand_RateLimited(t *testing.T) {
	svc := &fakeGameBindSvc{bindErr: biz.Wrap(biz.ErrRateLimited, nil, "x")}
	notifier := &fakeGameNotifier{returnTrue: true}

	handleGameCommand(context.Background(), 7, "trace", newGameCommandPayload("revolcast bind ABCD2345"), svc, notifier, zap.NewNop())

	if notifier.calls != 1 || notifier.text != gameRateLimitedText {
		t.Fatalf("期望限流文案，实际 %q", notifier.text)
	}
	if notifier.color != protocol.GameColorYellow {
		t.Fatalf("限流应使用黄色，实际 %q", notifier.color)
	}
}

// TestHandleGameCommand_InternalError 内部错误只回通用文案（不泄露细节）。
func TestHandleGameCommand_InternalError(t *testing.T) {
	svc := &fakeGameBindSvc{bindErr: biz.Wrap(biz.ErrInternal, errors.New("pg down"), "x")}
	notifier := &fakeGameNotifier{returnTrue: true}

	handleGameCommand(context.Background(), 7, "trace", newGameCommandPayload("revolcast bind ABCD2345"), svc, notifier, zap.NewNop())

	if !strings.Contains(notifier.text, biz.ResolveErrorMeta(biz.ErrInternal).WSMessage) {
		t.Fatalf("应回通用错误文案，实际 %q", notifier.text)
	}
	if strings.Contains(notifier.text, "pg down") {
		t.Fatalf("不得向玩家泄露内部细节: %q", notifier.text)
	}
}

// ---- handleGameEvent：命令事件不推送 ----

// TestHandleGameEvent_PlayerCommandRunsBusinessButNotPushed 命令事件先落库，再进业务分发，
// 且不进入 Web 推送（pushGameEventToWeb 对 player_command 直接返回）。
func TestHandleGameEvent_PlayerCommandRunsBusinessButNotPushed(t *testing.T) {
	store := &identityStore{}
	eventSvc := biz.NewGameEventService(store)
	bindSvc := &fakeGameBindSvc{}
	notifier := &fakeGameNotifier{returnTrue: true}
	log := zap.NewNop()

	payload := newGameCommandPayload("revolcast bind ABCD2345")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}

	task := queue.Task{
		TraceID:    "trace-cmd",
		ClientID:   "7",
		Type:       protocol.TypeGamePlayerCommand,
		Payload:    raw,
		ReceivedAt: time.Now(),
	}
	if err := handleGameEvent(context.Background(), task, NewHub(log), eventSvc, bindSvc, notifier, log); err != nil {
		t.Fatalf("命令事件处理应成功，实际 %v", err)
	}

	// 审计落库不受可见性策略影响。
	if len(store.recs) != 1 || store.recs[0].EventType != protocol.SubTypePlayerCommand {
		t.Fatalf("命令事件应落库审计，实际 %+v", store.recs)
	}
	if bindSvc.bindCalls != 1 {
		t.Fatalf("应进入绑定业务，实际 %d", bindSvc.bindCalls)
	}
	if notifier.calls != 1 {
		t.Fatalf("应下发游戏内回执，实际 %d", notifier.calls)
	}
}

// TestHandleGameEvent_ChatNotTreatedAsCommand 聊天事件不进入命令分发。
func TestHandleGameEvent_ChatNotTreatedAsCommand(t *testing.T) {
	store := &identityStore{}
	eventSvc := biz.NewGameEventService(store)
	bindSvc := &fakeGameBindSvc{}
	log := zap.NewNop()

	raw := []byte(`{"sub_type":"player_chat","message":"revolcast bind ABCD2345","player":{"uuid":"a3b91ae7-9efd-3e2e-8907-3db4a6c53509","nickname":"Steve"},"timestamp":1792395025}`)
	task := queue.Task{
		TraceID: "t", ClientID: "7", Type: protocol.TypeGamePlayerChat, Payload: raw, ReceivedAt: time.Now(),
	}
	if err := handleGameEvent(context.Background(), task, NewHub(log), eventSvc, bindSvc, nil, log); err != nil {
		t.Fatalf("聊天事件应正常处理，实际 %v", err)
	}
	if bindSvc.bindCalls != 0 {
		t.Fatalf("聊天事件不应触发绑定业务")
	}
}

// ---- 事件可见性 ----

func TestIsEventPubliclyVisible(t *testing.T) {
	if isEventPubliclyVisible(protocol.SubTypePlayerCommand) {
		t.Fatal("player_command 属于内部审计事件，不应对外可见")
	}
	for _, et := range []string{protocol.SubTypePlayerJoin, protocol.SubTypePlayerQuit, protocol.SubTypePlayerChat} {
		if !isEventPubliclyVisible(et) {
			t.Fatalf("%s 应对外可见", et)
		}
	}
}

// ---- 鹊桥下行回执 ----

// TestParseGameServerEvent_Response 下行回执不进入业务队列（也不会被当作缺 sub_type 的事件告警）。
func TestParseGameServerEvent_Response(t *testing.T) {
	c := parseOnlyGameServer(7, "element")
	raw := []byte(`{"code":200,"api":"send_private_msg","post_type":"response","status":"SUCCESS","message":"success","echo":"e1"}`)
	if _, ok := c.parseGameServerEvent(raw); ok {
		t.Fatal("回执消息不应投递业务队列")
	}
}

// TestHub_SendToGameServerByID 游戏内下发按 server_id 定位连接（不是 server_code）。
func TestHub_SendToGameServerByID(t *testing.T) {
	h := NewHub(zap.NewNop())
	c := newGameServerClient(nil, nil, 7, "element", zap.NewNop())
	h.gameServersByID[7] = c

	if !h.SendToGameServerByID(7, []byte(`{"api":"send_private_msg"}`)) {
		t.Fatal("期望投递成功")
	}
	select {
	case got := <-c.send:
		if !strings.Contains(string(got), "send_private_msg") {
			t.Fatalf("投递内容不符: %s", got)
		}
	default:
		t.Fatal("期望发送队列收到数据")
	}

	if h.SendToGameServerByID(8, []byte("x")) {
		t.Fatal("不存在的 server_id 应投递失败")
	}
}
