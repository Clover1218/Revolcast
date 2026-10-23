package transport

import (
	"strings"
	"testing"
	"time"

	"revolcast-backend/internal/biz"
)

// ---- parseCommand ----

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name string
		text string
		kind commandKind
		arg  string
	}{
		{"用户绑定", "revolcast user bind", commandUserBind, ""},
		{"用户绑定-大写", "REVOLCAST User BIND", commandUserBind, ""},
		{"用户绑定-多余空格", "  revolcast   user    bind  ", commandUserBind, ""},
		{"服务器列表", "revolcast server list", commandServerList, ""},
		{"服务器列表-大写", "Revolcast Server List", commandServerList, ""},
		{"服务器绑定", "revolcast server element bind", commandServerBind, "element"},
		{"服务器绑定-代号大写", "revolcast server ELEMENT bind", commandServerBind, "element"},
		{"服务器绑定-代号含连字符", "revolcast server survival-01 bind", commandServerBind, "survival-01"},
		{"服务器状态", "revolcast server element status", commandServerStatus, "element"},
		{"服务器状态-大写", "Revolcast Server ELEMENT STATUS", commandServerStatus, "element"},
		{"服务器状态-多余空格", "  revolcast   server   element   status  ", commandServerStatus, "element"},
		// 前缀门禁：普通私聊必须被忽略（Adapter 会转发全部私聊）。
		{"普通聊天", "你好啊", commandNone, ""},
		{"空消息", "", commandNone, ""},
		{"仅空白", "   \t ", commandNone, ""},
		{"前缀不完整", "revolca server list", commandNone, ""},
		{"前缀不是首词", "say revolcast server list", commandNone, ""},
		// revolcast 开头但无法识别 → 帮助。
		{"仅前缀", "revolcast", commandHelp, ""},
		{"未知子命令", "revolcast foo", commandHelp, ""},
		{"缺 bind", "revolcast server element", commandHelp, ""},
		{"缺代号", "revolcast server bind", commandHelp, ""},
		{"多参数", "revolcast server element bind now", commandHelp, ""},
		{"list 带多余参数", "revolcast server list all", commandHelp, ""},
		{"status 缺代号", "revolcast server status", commandHelp, ""},
		{"status 带多余参数", "revolcast server element status now", commandHelp, ""},
		{"未知子命令（同层）", "revolcast server element stats", commandHelp, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, arg := parseCommand(c.text)
			if kind != c.kind || arg != c.arg {
				t.Fatalf("parseCommand(%q) = (%v, %q)，期望 (%v, %q)", c.text, kind, arg, c.kind, c.arg)
			}
		})
	}
}

// ---- renderServerList ----

func TestRenderServerList_Format(t *testing.T) {
	got := renderServerList([]biz.AvailableServer{
		{Code: "element", Name: "元素觉醒", Description: "转生成魔法师拯救世界"},
		{Code: "AFop", Name: "宁然一隅", Description: "Find A Piece Of Peace"},
	})
	want := "当前可用服务器：\n" +
		"元素觉醒：转生成魔法师拯救世界\n代号：element\n" +
		"\n" +
		"宁然一隅：Find A Piece Of Peace\n代号：AFop"
	if got != want {
		t.Fatalf("文案不符:\n--- 实际 ---\n%s\n--- 期望 ---\n%s", got, want)
	}
}

func TestRenderServerList_Empty(t *testing.T) {
	if got := renderServerList(nil); got != "当前没有可用的服务器。" {
		t.Fatalf("空列表文案不符: %q", got)
	}
	if got := renderServerList([]biz.AvailableServer{}); got != "当前没有可用的服务器。" {
		t.Fatalf("空切片文案不符: %q", got)
	}
}

func TestRenderServerList_EmptyDescription(t *testing.T) {
	got := renderServerList([]biz.AvailableServer{{Code: "afop", Name: "宁然一隅"}})
	want := "当前可用服务器：\n宁然一隅：暂无简介\n代号：afop"
	if got != want {
		t.Fatalf("无简介文案不符:\n实际: %s\n期望: %s", got, want)
	}
}

// TestRenderServerList_Truncation 超过 20 条时截断并提示总数。
func TestRenderServerList_Truncation(t *testing.T) {
	list := make([]biz.AvailableServer, 25)
	for i := range list {
		list[i] = biz.AvailableServer{Code: "s", Name: "服务器"}
	}
	got := renderServerList(list)
	if n := strings.Count(got, "代号：s"); n != serverListMaxItems {
		t.Fatalf("期望只展示 %d 条，实际 %d", serverListMaxItems, n)
	}
	if !strings.Contains(got, "（共 25 个可用服务器，仅显示前 20 个") {
		t.Fatalf("缺少截断提示: %s", got)
	}
}

// TestRenderServerList_NoTruncationHint 未超限时不应出现截断提示。
func TestRenderServerList_NoTruncationHint(t *testing.T) {
	list := make([]biz.AvailableServer, serverListMaxItems)
	for i := range list {
		list[i] = biz.AvailableServer{Code: "s", Name: "服务器"}
	}
	if got := renderServerList(list); strings.Contains(got, "仅显示前") {
		t.Fatalf("恰好 20 条不应提示截断: %s", got)
	}
}

// ---- renderGameBindOutcome ----

func TestRenderGameBindOutcome_Code(t *testing.T) {
	got := renderGameBindOutcome(&biz.GameBindOutcome{
		Kind:      biz.GameBindOutcomeCode,
		Code:      "ABCD2345",
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Server:    biz.AvailableServer{Code: "element", Name: "元素觉醒"},
	})
	if !strings.Contains(got, "你的游戏角色绑定码为：ABCD2345") {
		t.Fatalf("缺少绑定码: %s", got)
	}
	if !strings.Contains(got, "服务器：元素觉醒（element）") {
		t.Fatalf("缺少服务器信息: %s", got)
	}
	if !strings.Contains(got, "10 分钟内有效") {
		t.Fatalf("有效期文案不符: %s", got)
	}
}

func TestRenderGameBindOutcome_AlreadyBound(t *testing.T) {
	got := renderGameBindOutcome(&biz.GameBindOutcome{
		Kind:     biz.GameBindOutcomeAlreadyBound,
		GameName: "Steve",
		Server:   biz.AvailableServer{Code: "element", Name: "元素觉醒"},
	})
	if !strings.Contains(got, "你在服务器「元素觉醒」已绑定角色：Steve") {
		t.Fatalf("已绑定文案不符: %s", got)
	}
}

func TestRenderGameBindOutcome_QQNotBound(t *testing.T) {
	got := renderGameBindOutcome(&biz.GameBindOutcome{Kind: biz.GameBindOutcomeQQNotBound})
	if !strings.Contains(got, "尚未绑定 Revolcast 账号") || !strings.Contains(got, "revolcast user bind") {
		t.Fatalf("未绑 QQ 文案不符: %s", got)
	}
}

func TestRenderGameBindOutcome_Nil(t *testing.T) {
	if got := renderGameBindOutcome(nil); got != biz.ResolveErrorMeta(biz.ErrInternal).WSMessage {
		t.Fatalf("nil 结论应回退内部错误文案，实际 %q", got)
	}
}

// TestRemainingMinutes_MinimumOne 已过期/临近过期时最少显示 1 分钟，避免「0 分钟内有效」。
func TestRemainingMinutes_MinimumOne(t *testing.T) {
	if got := remainingMinutes(time.Now().Add(-time.Minute)); got != 1 {
		t.Fatalf("过期应回退为 1，实际 %d", got)
	}
	if got := remainingMinutes(time.Now().Add(30 * time.Second)); got != 1 {
		t.Fatalf("不足 1 分钟应回退为 1，实际 %d", got)
	}
	if got := remainingMinutes(time.Now().Add(10 * time.Minute)); got != 9 && got != 10 {
		t.Fatalf("10 分钟有效期应显示 9~10，实际 %d", got)
	}
}

// ---- renderServerStatus（BIZ-012） ----

// TestRenderServerStatus_Players 有人在线时按「当前服务器有 N 人：A，B」渲染。
func TestRenderServerStatus_Players(t *testing.T) {
	got := renderServerStatus("element", &biz.ServerStatusView{
		ServerCode: "element",
		Online:     3,
		MaxPlayers: 20,
		Players: []biz.StatusPlayer{
			{Nickname: "Bot1"}, {Nickname: "Bot2"}, {Nickname: "Bot3"},
		},
	})
	want := "当前服务器有 3 人：Bot1，Bot2，Bot3"
	if got != want {
		t.Fatalf("文案不符:\n实际: %s\n期望: %s", got, want)
	}
}

// TestRenderServerStatus_Empty 有快照但 0 人：与「未找到」是两种结论。
func TestRenderServerStatus_Empty(t *testing.T) {
	got := renderServerStatus("element", &biz.ServerStatusView{ServerCode: "element", Online: 0})
	if got != "当前服务器没有玩家在线。" {
		t.Fatalf("0 人文案不符: %s", got)
	}
}

// TestRenderServerStatus_NotFound 没有快照时给出可执行指引，不暴露内部细节。
func TestRenderServerStatus_NotFound(t *testing.T) {
	got := renderServerStatus("element", nil)
	if !strings.Contains(got, "未找到「element」的服务器状态") || !strings.Contains(got, "联系管理员") {
		t.Fatalf("未找到文案不符: %s", got)
	}
}

// TestRenderServerStatus_Truncation 超过 30 人只列前 30 个昵称并提示。
func TestRenderServerStatus_Truncation(t *testing.T) {
	players := make([]biz.StatusPlayer, serverStatusMaxNames+5)
	for i := range players {
		players[i] = biz.StatusPlayer{Nickname: "p"}
	}
	got := renderServerStatus("element", &biz.ServerStatusView{
		Online: len(players), Players: players,
	})
	if n := strings.Count(got, "p"); n != serverStatusMaxNames {
		t.Fatalf("应只展示 %d 个昵称，实际 %d", serverStatusMaxNames, n)
	}
	if !strings.Contains(got, "仅显示前 30 人") {
		t.Fatalf("缺少截断提示: %s", got)
	}
}

// TestRenderServerStatus_NoNicknames 人数 > 0 但昵称全部不可用（解析时被跳过）只报人数。
func TestRenderServerStatus_NoNicknames(t *testing.T) {
	got := renderServerStatus("element", &biz.ServerStatusView{Online: 4})
	if got != "当前服务器有 4 人在线。" {
		t.Fatalf("无昵称文案不符: %s", got)
	}
}
