package protocol

import (
	"errors"
	"testing"
)

const sampleListUUIDsEcho = "There are 3 of a max of 20 players online: " +
	"Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058), " +
	"Bot2 (16beaa94-ff1b-37a7-8afb-c22dcb8c16d1), " +
	"Bot3 (73f370b4-497b-347a-a6bb-50d07eb88947)"

// TestParseListUUIDsEcho_DocSample 文档原样示例必须解析出 3 个玩家与正确的人数/上限。
func TestParseListUUIDsEcho_DocSample(t *testing.T) {
	snap, err := ParseListUUIDsEcho(sampleListUUIDsEcho)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if snap.Online != 3 || snap.MaxPlayers != 20 {
		t.Fatalf("人数解析错误: online=%d max=%d", snap.Online, snap.MaxPlayers)
	}
	if len(snap.Players) != 3 {
		t.Fatalf("玩家数错误: %d", len(snap.Players))
	}
	want := StatusPlayer{Nickname: "Bot1", UUID: "51a4b33d-8b80-3bde-806f-de4086b03058"}
	if snap.Players[0] != want {
		t.Fatalf("首个玩家错误: %+v", snap.Players[0])
	}
	if snap.Skipped != 0 || !snap.Consistent() {
		t.Fatalf("不应有跳过项且应一致: skipped=%d", snap.Skipped)
	}
}

// TestParseListUUIDsEcho_EmptyServer 0 人在线是正常回显（尾部和冒号都可能出现/缺失）。
func TestParseListUUIDsEcho_EmptyServer(t *testing.T) {
	for _, raw := range []string{
		"There are 0 of a max of 20 players online:",
		"There are 0 of a max of 20 players online: ",
		"There are 0 of a max of 20 players online",
	} {
		snap, err := ParseListUUIDsEcho(raw)
		if err != nil {
			t.Fatalf("解析失败 %q: %v", raw, err)
		}
		if snap.Online != 0 || snap.MaxPlayers != 20 || len(snap.Players) != 0 {
			t.Fatalf("0 人回显解析错误 %q: %+v", raw, snap)
		}
		if !snap.Consistent() {
			t.Fatalf("0 人应一致 %q", raw)
		}
	}
}

// TestParseListUUIDsEcho_Nicknames 昵称含空格 / 括号 / 中文时必须完整保留。
func TestParseListUUIDsEcho_Nicknames(t *testing.T) {
	cases := []struct {
		raw      string
		nickname string
	}{
		{"There are 1 of a max of 20 players online: Notch Jr. (51a4b33d-8b80-3bde-806f-de4086b03058)", "Notch Jr."},
		{"There are 1 of a max of 20 players online: 小明 (51a4b33d-8b80-3bde-806f-de4086b03058)", "小明"},
		{"There are 1 of a max of 20 players online: Bot (op) (51a4b33d-8b80-3bde-806f-de4086b03058)", "Bot (op)"},
	}
	for _, c := range cases {
		snap, err := ParseListUUIDsEcho(c.raw)
		if err != nil {
			t.Fatalf("解析失败 %q: %v", c.raw, err)
		}
		if len(snap.Players) != 1 || snap.Players[0].Nickname != c.nickname {
			t.Fatalf("昵称解析错误 %q: %+v", c.raw, snap.Players)
		}
	}
}

// TestParseListUUIDsEcho_SkipBrokenItem 单个条目格式异常时跳过并计数，整体仍成功。
func TestParseListUUIDsEcho_SkipBrokenItem(t *testing.T) {
	// 中间一项没有 `(uuid)`（例如被服务端截断），应跳过并计数。
	raw := "There are 3 of a max of 20 players online: " +
		"Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058), BrokenItem, " +
		"Bot2 (16beaa94-ff1b-37a7-8afb-c22dcb8c16d1)"
	snap, err := ParseListUUIDsEcho(raw)
	if err != nil {
		t.Fatalf("整体解析不应失败: %v", err)
	}
	if snap.Online != 3 {
		t.Fatalf("人数应取头部数字，实际 %d", snap.Online)
	}
	if snap.Skipped != 1 || len(snap.Players) != 2 {
		t.Fatalf("应跳过 1 项并保留 2 项: skipped=%d players=%d", snap.Skipped, len(snap.Players))
	}
	if snap.Consistent() {
		t.Fatal("条数与人数不一致时 Consistent 应为 false")
	}
}

// TestParseListUUIDsEcho_CommaInNickname 昵称含半角逗号会被切成两项（已知限制）：
// 后半截可能被误认为一个合法条目，因此只要求「不报错、人数以头部为准」。
func TestParseListUUIDsEcho_CommaInNickname(t *testing.T) {
	raw := "There are 2 of a max of 20 players online: Bad,Name (51a4b33d-8b80-3bde-806f-de4086b03058), " +
		"Bot2 (16beaa94-ff1b-37a7-8afb-c22dcb8c16d1)"
	snap, err := ParseListUUIDsEcho(raw)
	if err != nil {
		t.Fatalf("不应整体失败: %v", err)
	}
	if snap.Online != 2 {
		t.Fatalf("人数必须取头部数字，实际 %d", snap.Online)
	}
	if snap.Skipped != 1 {
		t.Fatalf("被切断的片段应计入 Skipped，实际 %d", snap.Skipped)
	}
}

// TestParseListUUIDsEcho_Unsupported 非英文原版格式一律返回 ErrListUUIDsUnsupported。
func TestParseListUUIDsEcho_Unsupported(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"RCON is not enabled",
		"Unknown or incomplete command",
		// 中文 locale 回显：已知限制，必须明确失败而不是产出错误数据。
		"当前有 3 名玩家，最多 20 名玩家在线：Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058)",
	}
	for _, raw := range cases {
		if _, err := ParseListUUIDsEcho(raw); !errors.Is(err, ErrListUUIDsUnsupported) {
			t.Fatalf("%q 应返回 ErrListUUIDsUnsupported，实际 %v", raw, err)
		}
	}
}
