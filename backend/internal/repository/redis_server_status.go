package repository

// 本文件定义「服务器在线状态快照」在 Redis 中的键与值结构（BIZ-012）。
//
// 设计要点（详见 docs/服务器状态查询闭环方案.md §4）：
//   - 每台服务器一个 String 键，值为整份 JSON 快照，写入是整体覆盖（无读改写）；
//   - 键后缀用 **server_code** 而不是 server_id：WS 连接侧与 QQ 指令侧都只持有 code，
//     两侧都不需要 code→id 的反查，做到零查表；
//   - 过期完全交给 Redis 原生 TTL，断连时主动 DEL。

// ServerStatusKeyPrefix 是状态快照键前缀：revolcast:server:status:{server_code}。
// 沿用数据库设计文档 §5 的 revolcast:server:* 命名族。
const ServerStatusKeyPrefix = "revolcast:server:status:"

// ServerStatusKey 拼装状态快照键。
func ServerStatusKey(serverCode string) string {
	return ServerStatusKeyPrefix + serverCode
}

// StatusPlayer 是快照中的单个在线玩家。
type StatusPlayer struct {
	Nickname string `json:"nickname"`
	UUID     string `json:"uuid"`
}

// ServerStatusRecord 是写入 Redis 的状态快照。
//
// 该数据**可随时重建**（下一次采集即覆盖），不是权威配置：
// 权威数据始终是 MC 服务端此刻的真实在线列表。
type ServerStatusRecord struct {
	// ServerID 是连接已鉴权的 game_servers.id，仅作冗余标识，不参与键定位。
	ServerID int64 `json:"server_id"`
	// ServerCode 与键后缀一致，便于排查时值自解释。
	ServerCode string `json:"server_code"`
	// Online 取自回显头部人数（不是 len(Players)）。
	Online int `json:"online"`
	// MaxPlayers 取自回显头部的最大人数。
	MaxPlayers int `json:"max_players"`
	// Players 在线玩家列表（可能因个别条目异常而少于 Online）。
	Players []StatusPlayer `json:"players"`
	// UpdatedAt 是采集成功时刻，UTC 固定毫秒（protocol.FormatEventTime）；
	// 只用于展示与排查，不参与过期判断（过期由 TTL 保证）。
	UpdatedAt string `json:"updated_at"`
	// Source 标识采集方式，当前固定 "rcon"。
	Source string `json:"source"`
}
