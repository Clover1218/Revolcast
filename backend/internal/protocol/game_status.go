package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 本文件解析 MC 服务端对 `list uuids` 的命令回显（BIZ-012 服务器状态采集）。
//
// 回显格式以 docs/鹊桥与服务端WS连接格式详解.md「发送RCON命令」为准（英文原版）：
//
//	There are 3 of a max of 20 players online: Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058), Bot2 (...), Bot3 (...)
//
// 已知限制：命令回显由服务端语言包生成。若服务端 locale 不是英文，文本会被本地化
// （例如「当前有 3 名玩家，最多 20 名玩家在线：…」），本解析器会返回 error。
// 上层必须把「解析失败」当作**不写缓存**处理，不能让旧的有效快照被坏样本覆盖。

// StatusPlayer 是状态快照中的单个在线玩家（仅保留展示所需字段）。
type StatusPlayer struct {
	Nickname string
	UUID     string
}

// ServerStatusSnapshot 是一次 `list uuids` 的解析结果。
type ServerStatusSnapshot struct {
	// Online 取自回显头部的 N（`There are N of a max of M ...`），**不是** len(Players)。
	Online int
	// MaxPlayers 取自回显头部的 M。
	MaxPlayers int
	// Players 按回显顺序排列的在线玩家（可能因个别项格式异常而少于 Online）。
	Players []StatusPlayer
	// Skipped 是尾部无法按 `昵称 (uuid)` 解析而被跳过的条目数（>0 时上层应记 Warn）。
	Skipped int
}

// Consistent 表示解析出的玩家条目数与头部人数一致。
// 不一致通常意味着回显被截断或昵称含分隔符，人数仍以 Online 为准。
func (s *ServerStatusSnapshot) Consistent() bool {
	return s != nil && len(s.Players) == s.Online
}

// listUUIDsHead 匹配英文原版回显的头部；尾部（玩家列表）允许为空。
var listUUIDsHead = regexp.MustCompile(`^There are (\d+) of a max of (\d+) players online:?`)

// listUUIDsPlayer 匹配单个玩家条目 `昵称 (uuid)`。
// 昵称用贪婪 `.+` 以兼容空格与括号（UUID 的括号在末尾，贪婪匹配不会误吞）。
var listUUIDsPlayer = regexp.MustCompile(`^(.+?)\s+\(([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)$`)

// ErrListUUIDsUnsupported 表示回显不是受支持的英文原版 `list uuids` 格式。
var ErrListUUIDsUnsupported = errors.New("list uuids 回显格式不受支持")

// ParseListUUIDsEcho 把 `list uuids` 的命令回显解析为在线玩家快照。
//
// 返回值语义：
//   - 头部无法匹配（空串 / RCON 未启用 / 本地化文本）→ ErrListUUIDsUnsupported；
//   - 头部匹配但个别条目异常 → 成功返回，异常条目计入 Skipped（昵称含逗号等场景）；
//   - 头部匹配且尾部为空 → Players 为空切片（0 人在线的正常回显）。
func ParseListUUIDsEcho(raw string) (*ServerStatusSnapshot, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, fmt.Errorf("%w: 回显为空", ErrListUUIDsUnsupported)
	}

	loc := listUUIDsHead.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, fmt.Errorf("%w: %q", ErrListUUIDsUnsupported, truncateForLog(text, 120))
	}

	online, err := strconv.Atoi(text[loc[2]:loc[3]])
	if err != nil {
		return nil, fmt.Errorf("%w: 在线人数无法解析: %q", ErrListUUIDsUnsupported, text[loc[2]:loc[3]])
	}
	maxPlayers, err := strconv.Atoi(text[loc[4]:loc[5]])
	if err != nil {
		return nil, fmt.Errorf("%w: 最大人数无法解析: %q", ErrListUUIDsUnsupported, text[loc[4]:loc[5]])
	}

	snap := &ServerStatusSnapshot{Online: online, MaxPlayers: maxPlayers, Players: []StatusPlayer{}}
	// loc[1] 是整段头部匹配的结束位置，其后即玩家列表（半角逗号分隔）。
	for _, item := range strings.Split(text[loc[1]:], ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		m := listUUIDsPlayer.FindStringSubmatch(item)
		if m == nil {
			snap.Skipped++
			continue
		}
		snap.Players = append(snap.Players, StatusPlayer{
			Nickname: strings.TrimSpace(m[1]),
			UUID:     m[2],
		})
	}
	return snap, nil
}

// truncateForLog 截断过长文本，避免把整段回显写进日志。按字节截断可能切坏多字节字符，
// 但这里只用于日志可读性，不影响业务判断。
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
