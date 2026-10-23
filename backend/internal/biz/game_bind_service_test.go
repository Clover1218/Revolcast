package biz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

// ---- fakes ----

// fakeAvailableServerSource 同时承担列表、单条校验（含 status）与「存在即可」解析三个职责
// （生产由 GameServerService 实现）。
type fakeAvailableServerSource struct {
	list    []AvailableServer
	listErr error

	byCode     *repository.GameServerRecord
	resolveErr error

	// resolveServerErr 仅作用于 ResolveServer（解绑路径），用于区分两条解析语义的测试。
	resolveServerErr error

	listCalls    int
	resolveCalls int
	// resolveServerCalls 记录 ResolveServer 调用次数：解绑必须走这条（不判 status）。
	resolveServerCalls int
	lastCode           string // 记录 ResolveSubscribableServer 收到的 code（应已转小写）
	lastResolveCode    string // 记录 ResolveServer 收到的 code
}

func (f *fakeAvailableServerSource) ListAvailable(_ context.Context) ([]AvailableServer, error) {
	f.listCalls++
	return f.list, f.listErr
}

func (f *fakeAvailableServerSource) ResolveSubscribableServer(_ context.Context, code string) (*repository.GameServerRecord, error) {
	f.resolveCalls++
	f.lastCode = code
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.byCode, nil
}

func (f *fakeAvailableServerSource) ResolveServer(_ context.Context, code string) (*repository.GameServerRecord, error) {
	f.resolveServerCalls++
	f.lastResolveCode = code
	if f.resolveServerErr != nil {
		return nil, f.resolveServerErr
	}
	return f.byCode, nil
}

// fakeQQActiveBindingReader 返回「该 QQ 对应的网站账号」。
type fakeQQActiveBindingReader struct {
	info *repository.QQBindingInfo
	err  error
}

func (f *fakeQQActiveBindingReader) GetActiveQQBinding(_ context.Context, _ int64) (*repository.QQBindingInfo, error) {
	return f.info, f.err
}

// fakeServerRoleBindingReader 返回「该账号在该服务器已有的角色绑定」。
type fakeServerRoleBindingReader struct {
	rec *repository.ServerGameBindingRecord
	err error
}

func (f *fakeServerRoleBindingReader) FindActive(_ context.Context, _, _ int64) (*repository.ServerGameBindingRecord, error) {
	return f.rec, f.err
}

// fakeGameBindCodeStore 记录发码仓储的调用，便于断言幂等与落库参数。
type fakeGameBindCodeStore struct {
	pending        string
	pendingExpires time.Time
	pendingFound   bool
	pendingErr     error

	created    string
	createErr  error
	createCall int
	lastSrvID  int64
	lastUserID int64
	lastQQ     int64
	lastTrace  string
	lastExpiry time.Time
}

func (f *fakeGameBindCodeStore) GetPendingGameBindCode(_ context.Context, _, _ int64) (string, time.Time, bool, error) {
	return f.pending, f.pendingExpires, f.pendingFound, f.pendingErr
}

func (f *fakeGameBindCodeStore) CreateGameBindCode(_ context.Context, serverID, userID, qqUIN int64, traceID string, expiresAt time.Time) (string, error) {
	f.createCall++
	f.lastSrvID, f.lastUserID, f.lastQQ, f.lastTrace, f.lastExpiry = serverID, userID, qqUIN, traceID, expiresAt
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.created, nil
}

// fakeGameRoleBindingWriter 记录赎回/解绑仓储调用，便于断言事务入参与结论映射。
type fakeGameRoleBindingWriter struct {
	result    *repository.GameRoleBindResult
	redeemErr error

	redeemCalls int
	lastInput   repository.GameRoleBindInput

	unbound   *repository.ServerGameBindingRecord
	unbindErr error

	unbindCalls      int
	lastUnbindSrvID  int64
	lastUnbindUserID int64
}

func (f *fakeGameRoleBindingWriter) RedeemGameBindCode(_ context.Context, in repository.GameRoleBindInput) (*repository.GameRoleBindResult, error) {
	f.redeemCalls++
	f.lastInput = in
	if f.redeemErr != nil {
		return nil, f.redeemErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &repository.GameRoleBindResult{
		Outcome:    repository.GameRoleBindOK,
		BindingID:  1,
		UserID:     42,
		GameName:   in.GameName,
		ServerCode: "element",
		ServerName: "元素觉醒",
	}, nil
}

func (f *fakeGameRoleBindingWriter) UnbindActive(_ context.Context, serverID, userID int64) (*repository.ServerGameBindingRecord, error) {
	f.unbindCalls++
	f.lastUnbindSrvID, f.lastUnbindUserID = serverID, userID
	if f.unbindErr != nil {
		return nil, f.unbindErr
	}
	return f.unbound, nil
}

// gameBindFixture 组装一个可用的 GameBindService 及其全部 fake，便于各用例按需改写。
type gameBindFixture struct {
	svc     *GameBindService
	servers *fakeAvailableServerSource
	users   *fakeUserStatusStore
	qqBind  *fakeQQActiveBindingReader
	roles   *fakeServerRoleBindingReader
	codes   *fakeGameBindCodeStore
	writer  *fakeGameRoleBindingWriter
	rl      *fakeBindRateLimiter
}

func newGameBindFixture() *gameBindFixture {
	desc := "转生成魔法师拯救世界"
	f := &gameBindFixture{
		servers: &fakeAvailableServerSource{
			list: []AvailableServer{{Code: "element", Name: "元素觉醒", Description: desc}},
			byCode: &repository.GameServerRecord{
				ID: 7, Code: "element", Name: "元素觉醒", Description: &desc, Status: 1,
			},
		},
		users:  &fakeUserStatusStore{user: &repository.UserRecord{ID: 42, Status: 1}},
		qqBind: &fakeQQActiveBindingReader{info: &repository.QQBindingInfo{UserID: 42, QQUIN: 10001}},
		roles:  &fakeServerRoleBindingReader{},
		codes:  &fakeGameBindCodeStore{created: "ABCD2345"},
		writer: &fakeGameRoleBindingWriter{},
		rl:     &fakeBindRateLimiter{allowed: true},
	}
	f.svc = NewGameBindService(f.servers, f.users, f.qqBind, f.roles, f.codes, f.writer, f.rl, GameBindConfig{})
	return f
}

// ---- BIZ-008：列出可用服务器 ----

func TestGameBindListAvailableServers_Delegates(t *testing.T) {
	f := newGameBindFixture()
	list, err := f.svc.ListAvailableServers(context.Background())
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if len(list) != 1 || list[0].Code != "element" {
		t.Fatalf("列表不符: %+v", list)
	}
	if f.servers.listCalls != 1 {
		t.Fatalf("期望委托下层查询 1 次，实际 %d", f.servers.listCalls)
	}
}

// ---- BIZ-009：生成游戏角色绑定码 ----

// TestGenerateBindCode_Success 正常路径：QQ 已绑定、无角色绑定 → 生成新码并落库。
func TestGenerateBindCode_Success(t *testing.T) {
	f := newGameBindFixture()
	before := time.Now()

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace-1")
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if out.Kind != GameBindOutcomeCode || out.Code != "ABCD2345" {
		t.Fatalf("期望返回新码，实际 %+v", out)
	}
	if out.Server.Name != "元素觉醒" || out.Server.Code != "element" {
		t.Fatalf("期望回显服务器信息，实际 %+v", out.Server)
	}
	if f.codes.createCall != 1 {
		t.Fatalf("期望落库 1 次，实际 %d", f.codes.createCall)
	}
	// 落库参数：server_id / user_id 来自权威查询，qq_uin 为审计快照，trace_id 透传。
	if f.codes.lastSrvID != 7 || f.codes.lastUserID != 42 || f.codes.lastQQ != 10001 || f.codes.lastTrace != "trace-1" {
		t.Fatalf("落库参数不符: srv=%d user=%d qq=%d trace=%s",
			f.codes.lastSrvID, f.codes.lastUserID, f.codes.lastQQ, f.codes.lastTrace)
	}
	// 有效期应为 10 分钟（默认 CodeTTL）。
	if ttl := f.codes.lastExpiry.Sub(before); ttl < 9*time.Minute || ttl > 11*time.Minute {
		t.Fatalf("期望有效期约 10 分钟，实际 %v", ttl)
	}
}

// TestGenerateBindCode_CodeNormalized 服务器代号大小写/空白不敏感。
func TestGenerateBindCode_CodeNormalized(t *testing.T) {
	f := newGameBindFixture()
	if _, err := f.svc.GenerateBindCode(context.Background(), 10001, "  ELEMENT  ", "trace"); err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if f.servers.lastCode != "element" {
		t.Fatalf("期望 code 归一化为 element，实际 %q", f.servers.lastCode)
	}
}

// TestGenerateBindCode_Idempotent 已有未过期待使用码 → 复用同一码，不重复落库。
func TestGenerateBindCode_Idempotent(t *testing.T) {
	f := newGameBindFixture()
	exp := time.Now().Add(3 * time.Minute)
	f.codes.pending, f.codes.pendingFound, f.codes.pendingExpires = "ZZZZ9999", true, exp

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if out.Code != "ZZZZ9999" {
		t.Fatalf("期望复用待使用码，实际 %q", out.Code)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("幂等命中时不应落库，实际落库 %d 次", f.codes.createCall)
	}
}

// TestGenerateBindCode_AlreadyBound 已有有效角色绑定 → 提示已绑定并回显角色名，不发码。
func TestGenerateBindCode_AlreadyBound(t *testing.T) {
	f := newGameBindFixture()
	f.roles.rec = &repository.ServerGameBindingRecord{ID: 1, ServerID: 7, UserID: 42, GameName: "Steve", Status: 1}

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功返回提示，实际 %v", err)
	}
	if out.Kind != GameBindOutcomeAlreadyBound || out.GameName != "Steve" {
		t.Fatalf("期望已绑定结论并回显角色名，实际 %+v", out)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("已绑定不应发码，实际落库 %d 次", f.codes.createCall)
	}
}

// TestGenerateBindCode_AlreadyBoundIgnoresRateLimit 限流在角色校验之后：
// 已绑定属于确定性答案，即使配额耗尽也应正常提示，而不是报「请求过于频繁」。
func TestGenerateBindCode_AlreadyBoundIgnoresRateLimit(t *testing.T) {
	f := newGameBindFixture()
	f.roles.rec = &repository.ServerGameBindingRecord{GameName: "Steve", Status: 1}
	f.rl.allowed = false

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望返回已绑定而不受限流影响，实际 %v", err)
	}
	if out.Kind != GameBindOutcomeAlreadyBound {
		t.Fatalf("期望已绑定结论，实际 %+v", out)
	}
}

// TestGenerateBindCode_QQNotBound 该 QQ 未绑定网站账号 → 引导先做 QQ 绑定。
func TestGenerateBindCode_QQNotBound(t *testing.T) {
	f := newGameBindFixture()
	f.qqBind.info = nil

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功返回提示，实际 %v", err)
	}
	if out.Kind != GameBindOutcomeQQNotBound {
		t.Fatalf("期望未绑 QQ 结论，实际 %+v", out)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("未绑 QQ 不应发码")
	}
}

// TestGenerateBindCode_QQBindingPointsToMissingUser 绑定指向已消失的账号 → 按未绑定处理（兜底）。
func TestGenerateBindCode_QQBindingPointsToMissingUser(t *testing.T) {
	f := newGameBindFixture()
	f.users.err = repository.ErrUserNotFound

	out, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功返回提示，实际 %v", err)
	}
	if out.Kind != GameBindOutcomeQQNotBound {
		t.Fatalf("期望按未绑 QQ 处理，实际 %+v", out)
	}
}

// TestGenerateBindCode_ServerNotFound 服务器不存在 / 格式非法 / 已停用统一映射为 ErrServerNotFound
// （生产实现 GameServerService.ResolveSubscribableServer 返回的都是 biz 哨兵错误）。
func TestGenerateBindCode_ServerNotFound(t *testing.T) {
	for _, srcErr := range []error{ErrServerNotFound, ErrServerCodeInvalid, ErrServerDisabled} {
		f := newGameBindFixture()
		f.servers.resolveErr = Wrap(srcErr, errors.New("cause"), "x")

		_, err := f.svc.GenerateBindCode(context.Background(), 10001, "nope", "trace")
		if !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("源错误 %v 期望映射为 ErrServerNotFound，实际 %v", srcErr, err)
		}
		if f.codes.createCall != 0 {
			t.Fatalf("服务器不可用不应发码")
		}
	}
}

// TestGenerateBindCode_ServerStoreInternal DB 故障保持 ErrInternal 并保留根因。
func TestGenerateBindCode_ServerStoreInternal(t *testing.T) {
	f := newGameBindFixture()
	f.servers.resolveErr = Wrap(ErrInternal, errors.New("pg down"), "查询游戏服务器失败")

	_, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}

// TestGenerateBindCode_AccountDisabled 账号被禁用 → 拒绝发码。
func TestGenerateBindCode_AccountDisabled(t *testing.T) {
	f := newGameBindFixture()
	f.users.user = &repository.UserRecord{ID: 42, Status: 0}

	if _, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("期望 ErrAccountDisabled，实际 %v", err)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("禁用账号不应发码")
	}
}

// TestGenerateBindCode_RateLimited 配额耗尽 → ErrRateLimited，且不查/写绑定码。
func TestGenerateBindCode_RateLimited(t *testing.T) {
	f := newGameBindFixture()
	f.rl.allowed = false

	_, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("限流后不应发码")
	}
}

// TestGenerateBindCode_RateLimiterFailOpen Redis 故障（限流器返回错误）应放行。
func TestGenerateBindCode_RateLimiterFailOpen(t *testing.T) {
	f := newGameBindFixture()
	f.rl.err = errors.New("redis down")
	f.rl.allowed = true // AllowAttempt 故障时返回放行

	if _, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace"); err != nil {
		t.Fatalf("限流器故障应 fail-open，实际 %v", err)
	}
	if f.codes.createCall != 1 {
		t.Fatalf("期望正常发码，实际 %d", f.codes.createCall)
	}
}

// TestGenerateBindCode_PendingQueryError 查询待使用码失败 → ErrInternal。
func TestGenerateBindCode_PendingQueryError(t *testing.T) {
	f := newGameBindFixture()
	f.codes.pendingErr = errors.New("pg down")
	if _, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

// TestGenerateBindCode_CreateError 落库失败 → ErrInternal 且保留根因。
func TestGenerateBindCode_CreateError(t *testing.T) {
	f := newGameBindFixture()
	f.codes.createErr = errors.New("unique violation")
	_, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace")
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "unique violation") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}

// TestGenerateBindCode_RoleBindingStoreError 角色查询失败 → ErrInternal，不得误判为「未绑定」。
func TestGenerateBindCode_RoleBindingStoreError(t *testing.T) {
	f := newGameBindFixture()
	f.roles.err = errors.New("pg down")
	if _, err := f.svc.GenerateBindCode(context.Background(), 10001, "element", "trace"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if f.codes.createCall != 0 {
		t.Fatalf("角色查询失败不应发码")
	}
}

// ---- BIZ-010：游戏内绑定角色 ----

var testGamePlayer = protocol.QueqiaoPlayer{
	UUID:     "A3B91AE7-9EFD-3E2E-8907-3DB4A6C53509",
	Nickname: "PlantClover",
}

// TestBindGameRole_Success 正常路径：码合法、玩家身份合法 → 事务入参正确，结论 bound。
func TestBindGameRole_Success(t *testing.T) {
	f := newGameBindFixture()

	out, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, " abcd2345 ", "trace-1")
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if out.Kind != GameRoleBindOutcomeBound {
		t.Fatalf("期望绑定成功，实际 %+v", out)
	}
	if out.GameName != "PlantClover" || out.ServerCode != "element" || out.ServerName != "元素觉醒" {
		t.Fatalf("结论缺少角色/服务器信息: %+v", out)
	}
	// 码归一化为大写；UUID 归一化为小写（PostgreSQL uuid 列语义一致，日志/幂等查询更稳定）。
	if f.writer.lastInput.Code != "ABCD2345" {
		t.Fatalf("期望绑定码归一化为 ABCD2345，实际 %q", f.writer.lastInput.Code)
	}
	if f.writer.lastInput.GameUUID != "a3b91ae7-9efd-3e2e-8907-3db4a6c53509" {
		t.Fatalf("期望 UUID 归一化为小写，实际 %q", f.writer.lastInput.GameUUID)
	}
	if f.writer.lastInput.ServerID != 7 || f.writer.lastInput.GameName != "PlantClover" {
		t.Fatalf("事务入参不符: %+v", f.writer.lastInput)
	}
}

// TestBindGameRole_InvalidCodeFormatSkipsEverything 码格式非法时既不查库也不消耗限流配额。
func TestBindGameRole_InvalidCodeFormatSkipsEverything(t *testing.T) {
	for _, bad := range []string{"", "ABC", "ABCD23456", "ABCD2340", "ABCD234O"} {
		f := newGameBindFixture()
		out, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, bad, "trace")
		if err != nil {
			t.Fatalf("码 %q 应为业务结论而非错误，实际 %v", bad, err)
		}
		if out.Kind != GameRoleBindOutcomeCodeInvalid {
			t.Fatalf("码 %q 期望 code_invalid，实际 %+v", bad, out.Kind)
		}
		if f.writer.redeemCalls != 0 || f.rl.calls != 0 {
			t.Fatalf("码 %q 不应触达限流/数据库（rl=%d db=%d）", bad, f.rl.calls, f.writer.redeemCalls)
		}
	}
}

// TestBindGameRole_PlayerInvalid UUID 缺失/非法或昵称为空 → player_invalid。
func TestBindGameRole_PlayerInvalid(t *testing.T) {
	cases := []protocol.QueqiaoPlayer{
		{UUID: "", Nickname: "Steve"},
		{UUID: "not-a-uuid", Nickname: "Steve"},
		{UUID: testGamePlayer.UUID, Nickname: "   "},
		{UUID: testGamePlayer.UUID, Nickname: strings.Repeat("a", 65)},
	}
	for _, p := range cases {
		f := newGameBindFixture()
		out, err := f.svc.BindGameRole(context.Background(), 7, p, "ABCD2345", "trace")
		if err != nil {
			t.Fatalf("应为业务结论，实际 %v", err)
		}
		if out.Kind != GameRoleBindOutcomePlayerInvalid {
			t.Fatalf("玩家 %+v 期望 player_invalid，实际 %+v", p, out.Kind)
		}
		if f.writer.redeemCalls != 0 {
			t.Fatalf("玩家非法不应触达数据库")
		}
	}
}

// TestBindGameRole_RateLimitedBeforeDB 限流是前置的：命中后不得查/写数据库。
func TestBindGameRole_RateLimitedBeforeDB(t *testing.T) {
	f := newGameBindFixture()
	f.rl.allowed = false

	_, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, "ABCD2345", "trace")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
	if f.writer.redeemCalls != 0 {
		t.Fatalf("限流后不应触达数据库，实际 %d", f.writer.redeemCalls)
	}
}

// TestBindGameRole_RateLimiterFailOpen Redis 故障（限流器返回错误但放行）时继续绑定。
func TestBindGameRole_RateLimiterFailOpen(t *testing.T) {
	f := newGameBindFixture()
	f.rl.err = errors.New("redis down")
	f.rl.allowed = true

	if _, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, "ABCD2345", "trace"); err != nil {
		t.Fatalf("限流器故障应 fail-open，实际 %v", err)
	}
	if f.writer.redeemCalls != 1 {
		t.Fatalf("期望继续绑定，实际 %d", f.writer.redeemCalls)
	}
}

// TestBindGameRole_OutcomeMapping 仓储结论到业务结论的映射（业务分支不返回 error）。
func TestBindGameRole_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name string
		repo repository.GameRoleBindOutcome
		want GameRoleBindOutcomeKind
	}{
		{"绑定成功", repository.GameRoleBindOK, GameRoleBindOutcomeBound},
		{"幂等重放", repository.GameRoleBindAlreadyBound, GameRoleBindOutcomeIdempotent},
		{"码无效", repository.GameRoleBindCodeInvalid, GameRoleBindOutcomeCodeInvalid},
		{"账号已绑定", repository.GameRoleBindAccountBound, GameRoleBindOutcomeAccountBound},
		{"角色被占", repository.GameRoleBindRoleTaken, GameRoleBindOutcomeRoleTaken},
		{"服务器停用", repository.GameRoleBindServerUnavailable, GameRoleBindOutcomeServerUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGameBindFixture()
			f.writer.result = &repository.GameRoleBindResult{
				Outcome:    c.repo,
				GameName:   "Steve",
				ServerCode: "element",
				ServerName: "元素觉醒",
			}
			out, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, "ABCD2345", "trace")
			if err != nil {
				t.Fatalf("期望业务结论而非错误，实际 %v", err)
			}
			if out.Kind != c.want {
				t.Fatalf("期望 %v，实际 %v", c.want, out.Kind)
			}
		})
	}
}

// TestBindGameRole_AccountDisabled 码指向的账号不可用 → 复用 ErrAccountDisabled。
func TestBindGameRole_AccountDisabled(t *testing.T) {
	f := newGameBindFixture()
	f.writer.result = &repository.GameRoleBindResult{Outcome: repository.GameRoleBindAccountDisabled}

	if _, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, "ABCD2345", "trace"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("期望 ErrAccountDisabled，实际 %v", err)
	}
}

// TestBindGameRole_WriterError 事务失败 → ErrInternal 且保留根因。
func TestBindGameRole_WriterError(t *testing.T) {
	f := newGameBindFixture()
	f.writer.redeemErr = errors.New("tx aborted")

	_, err := f.svc.BindGameRole(context.Background(), 7, testGamePlayer, "ABCD2345", "trace")
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "tx aborted") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}

// ---- BIZ-011：QQ 解除角色绑定 ----

// TestUnbindGameRole_Success 解绑走 ResolveServer（不判 status），不调用可订阅校验。
func TestUnbindGameRole_Success(t *testing.T) {
	f := newGameBindFixture()
	f.writer.unbound = &repository.ServerGameBindingRecord{
		ID: 3, ServerID: 7, UserID: 42, GameName: "Steve", GameUUID: "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
	}

	out, err := f.svc.UnbindGameRole(context.Background(), 10001, " ELEMENT ", "trace")
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if out.Kind != GameRoleUnbindOutcomeDone || out.GameName != "Steve" || out.ServerName != "元素觉醒" {
		t.Fatalf("解绑结论不符: %+v", out)
	}
	if f.servers.resolveServerCalls != 1 || f.servers.resolveCalls != 0 {
		t.Fatalf("解绑应走 ResolveServer（calls=%d），不应走 ResolveSubscribableServer（calls=%d）",
			f.servers.resolveServerCalls, f.servers.resolveCalls)
	}
	if f.servers.lastResolveCode != "element" {
		t.Fatalf("期望 code 归一化为小写，实际 %q", f.servers.lastResolveCode)
	}
	if f.writer.lastUnbindSrvID != 7 || f.writer.lastUnbindUserID != 42 {
		t.Fatalf("解绑入参不符: srv=%d user=%d", f.writer.lastUnbindSrvID, f.writer.lastUnbindUserID)
	}
}

// TestUnbindGameRole_AllowsDisabledServer 服务器停用后仍允许解绑（ResolveServer 不校验 status）。
func TestUnbindGameRole_AllowsDisabledServer(t *testing.T) {
	f := newGameBindFixture()
	f.servers.byCode.Status = 0 // 已停用
	f.writer.unbound = &repository.ServerGameBindingRecord{ID: 3, GameName: "Steve"}

	out, err := f.svc.UnbindGameRole(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("停用服务器也应可解绑，实际 %v", err)
	}
	if out.Kind != GameRoleUnbindOutcomeDone {
		t.Fatalf("期望解绑成功，实际 %+v", out.Kind)
	}
}

// TestUnbindGameRole_NotBound 无有效绑定 → not_bound。
func TestUnbindGameRole_NotBound(t *testing.T) {
	f := newGameBindFixture()
	out, err := f.svc.UnbindGameRole(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功返回提示，实际 %v", err)
	}
	if out.Kind != GameRoleUnbindOutcomeNotBound {
		t.Fatalf("期望无需解绑，实际 %+v", out.Kind)
	}
}

// TestUnbindGameRole_QQNotBound 该 QQ 未绑定网站账号 → qq_not_bound（不查绑定表）。
func TestUnbindGameRole_QQNotBound(t *testing.T) {
	f := newGameBindFixture()
	f.qqBind.info = nil

	out, err := f.svc.UnbindGameRole(context.Background(), 10001, "element", "trace")
	if err != nil {
		t.Fatalf("期望成功返回提示，实际 %v", err)
	}
	if out.Kind != GameRoleUnbindOutcomeQQNotBound {
		t.Fatalf("期望未绑 QQ，实际 %+v", out.Kind)
	}
	if f.writer.unbindCalls != 0 {
		t.Fatalf("未绑 QQ 不应触达解绑 UPDATE")
	}
}

// TestUnbindGameRole_ServerNotFound 服务器不存在 / code 非法统一映射为 ErrServerNotFound。
func TestUnbindGameRole_ServerNotFound(t *testing.T) {
	for _, srcErr := range []error{ErrServerNotFound, ErrServerCodeInvalid} {
		f := newGameBindFixture()
		f.servers.resolveServerErr = Wrap(srcErr, errors.New("cause"), "x")

		_, err := f.svc.UnbindGameRole(context.Background(), 10001, "nope", "trace")
		if !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("源错误 %v 期望映射为 ErrServerNotFound，实际 %v", srcErr, err)
		}
		if f.writer.unbindCalls != 0 {
			t.Fatalf("服务器不可用不应解绑")
		}
	}
}

// TestUnbindGameRole_WriterError 解绑失败 → ErrInternal，不得误判为「无需解绑」。
func TestUnbindGameRole_WriterError(t *testing.T) {
	f := newGameBindFixture()
	f.writer.unbindErr = errors.New("pg down")

	_, err := f.svc.UnbindGameRole(context.Background(), 10001, "element", "trace")
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}
