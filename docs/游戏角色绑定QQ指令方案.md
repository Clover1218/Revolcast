# 游戏角色绑定 QQ 指令方案

> 状态：**已实现**（2026-10-23；方案评审通过后按本方案落地，落地差异见 §14）
> 关联文档：《Revolcast业务流程设计文档》§2.8 / §2.9（BIZ-008 / BIZ-009）、《Revolcast_数据库表设计文档_v1.1》§3.4 / §3.6 / §3.7 / §5 / §6.2、《Websocket消息结构设计文档》§4
> 关联代码：`backend/internal/transport/handler.go`、`backend/internal/biz/game_bind_service.go`、`backend/internal/biz/game_server_service.go`、`backend/internal/repository/game_bind_code.go`、`backend/internal/repository/server_game_binding.go`

---

## 1. 目标

在 QQ 私聊中新增两条指令，打通「QQ 号 → 网站账号 → 游戏内角色」的绑定入口：

| 指令 | 作用 | 对应业务编号 |
| --- | --- | --- |
| `revolcast server list` | 列出当前**可用**服务器（名称 / 简介 / 代号） | BIZ-008 |
| `revolcast server <code> bind` | 为「当前 QQ 已绑定的网站账号」生成指定服务器的游戏角色绑定码 | BIZ-009 |

服务器列表查询走 Redis 缓存（TTL 5 分钟），避免每条指令都打到 PostgreSQL。

## 2. 范围

**本期实现**

1. 后端命令解析扩展（一个私聊消息 → 多条指令的路由）。
2. `GameServerService` 新增「可用服务器列表 + 5 分钟 Redis 缓存」，并在服务器变更时失效缓存。
3. 新增 `GameBindCodeRepo`（`game_bind_codes` 读写）与 `ServerGameBindingRepo`（`server_game_bindings` 只读）。
4. 新增 `GameBindService`（BIZ-009 全流程）与对应私聊文案。
5. 数据库补一条**部分唯一索引**（见 §8）。

**本期不做（明确边界）**

- **绑定码赎回（BIZ-010）**：游戏服务器如何把玩家在游戏内输入的 8 位码回传后端、写入 `server_game_bindings` 并回填 `game_name` / `game_uuid`，属于下一阶段（需要新增游戏服务器侧接口 + 事务）。本期只负责**发码**，码会处于 `status=0` 待使用状态。
- **网站侧角色解绑 / 更换角色**：本期对已绑定角色只做提示，不提供解绑入口。
- **群聊指令**：仅支持私聊（与现有 BIZ-001 一致）。群聊若要支持，可通过 `server_groups` 由群号反查服务器，另开方案。
- **adapter 端改动**：**零改动**（见 §3 决策 4）。命令识别全部在 backend。

## 3. 已确认决策

| # | 决策项 | 结论 | 说明 |
| --- | --- | --- | --- |
| 1 | 「可用服务器」的 status | **`status = 1`** | 数据库文档 §3.4 明确 `1=启用、0=停用`「可用」即启用。原始需求中写的 `status==0` 与表定义相反，按 `status=1` 实现。 |
| 2 | QQ 已绑定、但目标服务器已有有效角色 | **提示已绑定，不生成新码** | 回显已绑定的角色名，并说明「一个账号在一个服务器只能绑定一个角色」。不引入隐式解绑。 |
| 3 | 限流维度 | **按 (qq_uin + server_code) 分别限流** | 用户可为不同服务器分别申请，互不挤占额度。 |
| 4 | adapter 改动 | **adapter 不过滤，私聊消息全量转发后端** | 与现状一致。命令识别与「非命令消息忽略」全部由 backend 负责，符合 AGENTS.md「adapter 不做业务决策」。 |

### 3.1 决策 4 带来的硬性约束

既然 adapter 全量转发私聊，backend **必须**做到：

- 仅当消息首个词（忽略大小写、忽略首尾空白）为 `revolcast` 时才进入命令解析；
- 非 `revolcast` 开头的私聊消息 **静默丢弃**（不回复），否则机器人会回复用户的所有私聊内容；
- `revolcast` 开头但无法识别的指令 → 回一条固定帮助文案（不静默，便于用户自查）。

## 4. 指令协议

### 4.1 语法

```
revolcast server list
revolcast server <code> bind
revolcast user bind            （既有，BIZ-001，保持不变）
```

解析规则：

- **大小写不敏感**（命令词与 `code` 均先 `ToLower`）。
- **空白不敏感**：按 `strings.Fields` 切词，多个空格/制表符视为一个分隔。
- 参数字数必须完全匹配，多余或缺失均视为「无法识别」→ 帮助文案。

| 词序列 | 结果 |
| --- | --- |
| `revolcast` `user` `bind` | 走 BIZ-001（原逻辑） |
| `revolcast` `server` `list` | 走 BIZ-008 |
| `revolcast` `server` `<code>` `bind` | 走 BIZ-009 |
| 其它以 `revolcast` 开头 | 帮助文案 |
| 不以 `revolcast` 开头 | 忽略（无回复） |

### 4.2 文案模板

**A. 帮助（无法识别的指令）**

```
未知指令。当前可用指令：
revolcast server list —— 查看可用服务器列表
revolcast server <代号> bind —— 生成游戏角色绑定码
revolcast user bind —— 生成 QQ 绑定码
```

**B. 服务器列表（BIZ-008）**

```
当前可用服务器：
元素觉醒：转生成魔法师拯救世界
代号：element

宁然一隅：Find A Piece Of Peace
代号：AFop
```

- `description` 为 `NULL` 或空串 → 显示 `暂无简介`。
- 空列表 → `当前没有可用的服务器。`
- 条目上限 **20**，超出时末尾追加：`（共 N 个可用服务器，仅显示前 20 个，请访问网站查看完整列表。）`
- 每条之间用**一个空行**分隔（QQ 客户端富文本较窄，空行更易读）。

**C. 生成绑定码成功（BIZ-009）**

```
你的游戏角色绑定码为：ABCD2345
服务器：元素觉醒（element）
请在目标服务器内输入该绑定码完成角色绑定，10 分钟内有效。
```

**D. 该服务器已绑定角色**

```
你在服务器「元素觉醒」已绑定角色：Steve
一个账号在同一个服务器只能绑定一个角色。如需更换，请联系管理员。
```

**E. QQ 尚未绑定网站账号**

```
你的 QQ 尚未绑定 Revolcast 账号。
请先私聊发送「revolcast user bind」获取 QQ 绑定码，并在网站个人中心完成 QQ 绑定后再试。
```

**F. 服务器不存在 / 已停用**

```
未找到代号为「xxx」的可用服务器。
请发送「revolcast server list」查看当前可用服务器。
```
（`code` 格式非法、不存在、已停用 → **统一同一文案**，避免枚举出已停用服务器。）

**G. 失败（复用 `biz.ResolveErrorMeta` 的 WSMessage）**

- 限流 → `请求过于频繁，请 5 分钟后再试。`
- 账号被禁用 → `该账号已被禁用，请联系管理员。`
- 内部错误 → `服务内部错误，请稍后再试。`

## 5. 流程设计

### 5.1 BIZ-008：列出可用服务器

```
QQ 私聊 "revolcast server list"
        │
        ▼
transport.route() 命令解析
        │
        ▼
GameBindService.ListAvailableServers(ctx)
        │  ① 读缓存 revolcast:cache:server:list:available
        │     命中 → 反序列化 → 返回
        │     未命中 → ②
        ▼  ② 查库 ListGameServers(status=1, limit=100, offset=0)
        │  ③ 写缓存 SETEX 300s（失败仅记日志，不影响返回）
        ▼
transport 渲染文案 B → command.qq.send_private_message
```

步骤表：

| # | 步骤 | 失败策略 |
| --- | --- | --- |
| 01 | 读 Redis 缓存 | Redis 错误 → **fail-open**，直接查库，不写缓存 |
| 02 | 缓存未命中 → 查 `game_servers WHERE status=1 ORDER BY id LIMIT 100` | 返回 `ErrInternal` → 文案 G |
| 03 | 回填缓存（JSON 数组，TTL 300s） | 失败仅记日志 |
| 04 | 渲染文案（截断 20 条） | — |

**为什么 100 条**：可用服务器数量是运维级规模，100 条足够覆盖；单条 JSON 缓存值约 10KB 以内，不会撑爆 Redis。

**不做额外限流**：列表走缓存，成本≈一次 Redis GET；若后续出现刷屏，再补 `revolcast:ratelimit:qq:server_list:{qq_uin}`（5 分钟 / 30 次）。

### 5.2 BIZ-009：生成游戏角色绑定码

```
QQ 私聊 "revolcast server element bind"
        │
        ▼
① 解析 code（ToLower）→ ResolveSubscribableServer（DB 权威校验）
        │   格式非法 / 不存在 / 停用 → 文案 F（统一）
        ▼
② 查 qq_bindings(status=1) 取 user_id
        │   无 → 文案 E
        ▼
③ 查 users.status
        │   != 1 → ErrAccountDisabled（文案 G）
        ▼
④ 查 server_game_bindings(server_id, user_id, status=1)
        │   有 → 文案 D（不生成新码）
        ▼
⑤ 限流 revolcast:ratelimit:qq:game_bind:{qq_uin}:{code}  5min / 5 次
        │   超限 → ErrRateLimited（文案 G）
        ▼
⑥ 幂等取/建 game_bind_codes
        │   已有未过期待使用码 → 直接复用同一码
        │   否则：回收过期槽位 → 生成新码 → 写入
        ▼
⑦ 文案 C → command.qq.send_private_message
```

步骤表（含顺序理由）：

| # | 步骤 | 说明 |
| --- | --- | --- |
| 01 | 服务器校验 | **以 DB 为准**，不走缓存（见 §6.3）。用既有 `GameServerService.ResolveSubscribableServer`：格式校验 → 按 code 查 → `status != 1` 拒绝。 |
| 02 | QQ 绑定解析 | 新增 `QQBindRepo.GetActiveQQBinding(ctx, qqUIN)` → `(userID, botID, found, err)`。`user_id` 是 `game_bind_codes.user_id` 的唯一来源。 |
| 03 | 账号状态 | 复用 `bindUserStatusStore.GetByID`，禁用账号拒绝发码（与 BIZ-006 口径一致）；`ErrUserNotFound` 视为 QQ 未绑定（数据不一致兜底）。 |
| 04 | 角色绑定检查 | 新增 `ServerGameBindingRepo.FindActive(ctx, serverID, userID)`，命中则回显 `game_name`。 |
| 05 | 限流 | 键 `revolcast:ratelimit:qq:game_bind:{qq_uin}:{server_code}`，**5 分钟 / 5 次**，`fail-open`（Redis 故障不阻断）。放在 05 而非 01：①②③④ 均为只读幂等查询，先让用户拿到「已绑定」「未绑 QQ」等确定性答案；限流只拦截真正会**发码**的请求。 |
| 06 | 幂等取/建 | 与 `qq_bind_codes` 完全同构：先查未过期待使用码 → 有则复用；否则先把该 `(server_id,user_id)` 已过期仍 `status=0` 的旧码置 `status=2`（回收唯一槽位），再插入新码；并发冲突时回退查询现有码。 |
| 07 | 回复 | 私聊发送失败不影响业务成功（码已落库，用户重发指令会拿到同一码）。 |

**绑定码规格**（照搬 QQ 绑定码）

| 项 | 取值 |
| --- | --- |
| 长度 | 8 |
| 字符集 | `ABCDEFGHJKMNPQRSTUVWXYZ23456789`（排除 `0/O/1/I/L`） |
| 生成 | `crypto/rand` 取模（复用 `repository.generateBindCode`） |
| 有效期 | **10 分钟**（数据库文档 §3.6「建议创建后 10 分钟」；与 QQ 绑定码的 5 分钟不同，属有意差异） |
| 落库字段 | `code` / `server_id` / `user_id` / `qq_uin`（审计快照）/ `trace_id` / `status=0` / `expires_at` |
| 留空字段 | `game_name` / `game_uuid`（赎回时由游戏服务端验证后回填） |

### 5.3 返回值设计（业务分支不走 error）

`GenerateBindCode` 返回 `(*GameBindOutcome, error)`：

```go
// GameBindOutcome 描述 BIZ-009 的正向结论。三种「正常结束」不通过 error 表达，
// 以便 transport 层渲染各自文案（文案是业务产物，不是错误）。
type GameBindOutcome struct {
    Kind      GameBindOutcomeKind // code | already_bound | qq_not_bound
    Code      string              // Kind=code 时有效
    ExpiresAt time.Time           // Kind=code 时有效
    Server    AvailableServer     // 服务器名称/代号，用于回显
    GameName  string              // Kind=already_bound 时有效
}
```

`error` 仅用于：`ErrServerNotFound` / `ErrAccountDisabled` / `ErrRateLimited` / `ErrInternal`。

> **为什么不为「已绑定」「未绑 QQ」新增错误码**：它们是用户可预期的正常分支，且文案需要携带角色名等上下文；若走 `errorMeta` 会丢失上下文，也会污染错误码表。现有 `errorMeta` 保持**零新增**。

## 6. Redis 设计

### 6.1 键与 TTL

| 用途 | 键 | 类型 | TTL | 说明 |
| --- | --- | --- | --- | --- |
| 可用服务器列表缓存 | `revolcast:cache:server:list:available` | String(JSON) | 300s | 可失效重建，**非权威**数据；沿用数据库文档 §5 `revolcast:cache:server:*` 命名族 |
| 绑定码生成限流 | `revolcast:ratelimit:qq:game_bind:{qq_uin}:{server_code}` | Counter | 300s | 与 §5 `revolcast:ratelimit:{scope}:{subject}` 一致 |

缓存值结构（`description` 拍平为字符串，`NULL → ""`，避免 `*string` 判空）：

```json
[{"code":"element","name":"元素觉醒","description":"转生成魔法师拯救世界"},
 {"code":"AFop","name":"宁然一隅","description":""}]
```

### 6.2 缓存一致性

- **Cache-Aside**：读时未命中才查库并回填；写时**先改库、后删键**（不做写回）。
- **失效点**：`GameServerService.Create` / `Update`（含 `status` 变更）/ `Disable` 成功后 `DEL` 缓存键。
  `RotateSecret` 不影响任何展示字段，**不**失效。
- **失败策略**：`DEL` 失败仅记 Warn 日志，由 300s TTL 兜底（最坏 5 分钟可见延迟）。
- **跨实例**：Redis 共享，任一实例 DEL 对全部实例生效。
- **不做防击穿**：单键 300s、数据量极小，不加 singleflight；若后续 QPS 上升再引入。

### 6.3 缓存不参与「发码」的权威判定

`revolcast server list` 与 `bind` 若都读缓存，存在窗口：管理员在缓存有效期内停用某服务器，用户仍能对已停用服务器发码。

因此：

- `list` → 读缓存（展示用途，滞后可接受）；
- `bind` 的服务器校验 → **直接查库**（`ResolveSubscribableServer`，单条 `code` 索引查询，成本可忽略）。

需求中「先去服务器列表里查存不存在这个服务器」在语义上等价满足：`status=1` 的服务器集合就是列表集合，只是判定源取权威库而非缓存。

## 7. 代码改动清单

### 7.1 backend

| 文件 | 改动 |
| --- | --- |
| `internal/repository/redis_cache.go` | **新增**。`GetCache(ctx, key, dest) (hit bool, err error)` / `SetCache(ctx, key, val, ttl)` / `DelCache(ctx, keys...)`；常量 `GameServerListCacheKey`；`GameBindRateLimitKey(qqUIN, serverCode)` |
| `internal/repository/game_bind_code.go` | **新增** `GameBindCodeRepo`：`GetPending(ctx, serverID, userID)`、`Create(ctx, serverID, userID, qqUIN, traceID, expiresAt)`（含 `reclaimExpiredPending` + 唯一冲突重试/回退查询）；复用同包 `generateBindCode` / `bindCodeAlphabet` / `uniqueViolationOn` |
| `internal/repository/server_game_binding.go` | **新增** `ServerGameBindingRepo.FindActive(ctx, serverID, userID) (*Record, error)`（`WHERE status=1`，无记录返回 `nil, nil`） |
| `internal/repository/qq_bind.go` | **新增** `GetActiveQQBinding(ctx, qqUIN) (userID int64, botID string, found bool, err error)`；`IsQQActiveBound` 改为委托它（保持既有调用不变） |
| `internal/biz/game_server_service.go` | 新增 `ServerCache` 依赖与 `GameServerConfig.AvailableCacheTTL`；新增 `ListAvailable(ctx) ([]AvailableServer, error)`；`Create/Update/Disable` 成功后 `DelCache` |
| `internal/biz/game_bind_service.go` | **新增** `GameBindService`：`ListAvailableServers`、`GenerateBindCode`，含 `GameBindOutcome` / `GameBindConfig`（TTL、限流窗口与上限） |
| `internal/transport/handler.go` | 新增 `parseCommand(text) (cmd, arg, ok)`；`route` 改为按命令分发；非 `revolcast` 消息静默丢弃；`NewBizHandler` 增加 `gameBindSvc` 参数 |
| `cmd/server/main.go` | 装配 `GameBindCodeRepo` / `ServerGameBindingRepo` / `GameBindService`；`NewGameServerService` 传入缓存依赖；`NewBizHandler` 参数同步 |
| `internal/transport/handler_command_test.go` | **新增**。命令解析表驱动测试（大小写、多空格、缺参、多参、非命令） |
| `internal/biz/game_bind_service_test.go` | **新增**。未绑 QQ / 已绑角色 / 限流 / 幂等复用 / 服务器停用 / 账号禁用 |
| `internal/biz/game_server_service_test.go` | 补充缓存命中、缓存未命中回填、`Disable` 后失效 |

### 7.2 adapter

**零改动。** `revolcast-qq-adapter/main.go` 中已有注释说明「业务命令识别与绑定码生成由 Backend 统一负责」，私聊全量转发逻辑保持不变。

## 8. 数据库改动

`game_bind_codes` / `server_game_bindings` 已按《数据库表设计文档》附录 DDL 建表，字段无需变更。

**唯一需要补的是一条部分唯一索引**：文档 §3.6 给的是普通索引

```sql
-- 现状（非唯一）
CREATE INDEX ix_game_bind_codes_pending_user
    ON game_bind_codes (server_id, user_id, expires_at) WHERE status = 0;
```

非唯一索引**无法**约束「同一 (服务器, 账号) 同时只有一条待使用码」，并发两次请求会落两条码，`GetPending` 只能返回其一。改为唯一索引（与 `ux_qq_bind_codes_pending_qq` 对齐），让并发下的幂等由数据库兜底：

```sql
-- 1) 预检：确认无重复待使用码（有输出则先人工清理）
SELECT server_id, user_id, count(*)
FROM game_bind_codes
WHERE status = 0
GROUP BY 1, 2
HAVING count(*) > 1;

-- 2) 回收已过期但仍占槽位的旧码
UPDATE game_bind_codes SET status = 2
WHERE status = 0 AND expires_at <= now();

-- 3) 替换索引
DROP INDEX IF EXISTS ix_game_bind_codes_pending_user;
CREATE UNIQUE INDEX ux_game_bind_codes_pending_user
    ON game_bind_codes (server_id, user_id) WHERE status = 0;
```

> 谓词只能是 `status = 0`（`now()` 不是 immutable，无法进索引谓词），因此 §7.1 的 `Create` 必须先执行「回收过期槽位」，否则旧码会永久占位。这与 `qq_bind_codes` 的已知坑完全一致。

**索引名注意事项**：`game_bind_codes.code` 的唯一索引名为 `ux_game_bind_codes_code`（附录中为独立 `CREATE UNIQUE INDEX`，非内联约束），`UniqueViolationOn` 判断需用该名。

## 9. 错误码

**零新增。** 复用既有映射：

| 场景 | 哨兵 | WS code | 用户文案 |
| --- | --- | --- | --- |
| 服务器非法/不存在/停用 | `ErrServerNotFound` | `SERVER_NOT_FOUND` | 未找到代号为「xxx」的可用服务器。请发送…（transport 追加指引） |
| 账号被禁用 | `ErrAccountDisabled` | `ACCOUNT_DISABLED` | 该账号已被禁用，请联系管理员。 |
| 限流 | `ErrRateLimited` | `RATE_LIMITED` | 请求过于频繁，请 5 分钟后再试。 |
| 其它 | `ErrInternal` | `INTERNAL_ERROR` | 服务内部错误，请稍后再试。 |

transport 层日志统一带 `qq_uin` / `server_code` / `error_code` / `zap.Error(err)`，不向用户泄露根因。

## 10. 边界与异常

| 场景 | 处理 |
| --- | --- |
| 用户在缓存有效期内对刚停用的服务器发码 | `bind` 查库拒绝（§6.3） |
| 用户连点两次 `bind` | `ux_game_bind_codes_pending_user` 兜底；命中冲突后回退查现有码，返回同一码 |
| 用户 5 分钟内重复 `bind` 同一服务器 | 幂等返回同一码（正常路径，仍消耗 1 次配额） |
| QQ 已绑定，但 `qq_bindings` 指向的 `users` 行不存在 | 视为「QQ 未绑定」（文案 E）+ Warn 日志 |
| 待使用码过期后再次 `bind` | 先 `status 0→2` 回收槽位，再发新码 |
| 用户在游戏内使用过期码 | 赎回侧按 `status=0 AND expires_at>now()` 拒绝（BIZ-010 实现时保证） |
| 私聊发送失败 | 码已落库即视为业务成功；仅 Warn 日志，用户重发可拿回同一码 |
| 可用服务器超过 20 个 | 列表截断，末尾附总数提示 |
| 非 `revolcast` 私聊 | 静默丢弃（不回复、不写业务日志，仅 debug 级） |

## 11. 待评审确认点

| # | 事项 | 本方案取值 | 备注 |
| --- | --- | --- | --- |
| P1 | 游戏角色绑定码有效期 | **10 分钟** | 依据数据库文档 §3.6；若希望与 QQ 绑定码统一为 5 分钟，改 `GameBindConfig.BindCodeTTL` 即可 |
| P2 | 限流窗口内次数 | **5 次 / 5 分钟** | 需求只给了「5 分钟」；窗口对齐 QQ 发码，次数放宽（同一码会幂等复用） |
| P3 | 列表条目上限 | **20** | 防止 QQ 消息过长被折叠 |
| P4 | 列表是否限流 | **不限流** | 走缓存，成本极低 |
| P5 | 是否为 `game_bind_codes` 补唯一索引 | **补**（§8） | 不补则并发可能产生多条待使用码 |
| P6 | 已绑定角色时的提示是否给出网站解绑指引 | **联系管理员** | 网站侧角色解绑能力未实现 |

## 12. 验收标准

| # | 用例 | 预期 |
| --- | --- | --- |
| 1 | 私聊 `REVOLCAST   SERVER   LIST` | 返回列表，格式与 §4.2 B 一致 |
| 2 | 无启用服务器 | `当前没有可用的服务器。` |
| 3 | 连续两次 `server list` | 第二次命中缓存（日志中无 DB 查询；`game_servers` 查询计数不增加） |
| 4 | 停用某服务器后立即 `server list`（缓存有效期内） | 仍显示旧列表（≤5 分钟） |
| 5 | 对刚停用的服务器 `bind` | 文案 F（不生成码） |
| 6 | 未绑 QQ 的号 `bind` | 文案 E |
| 7 | 已绑 QQ、无角色绑定 `bind` | 文案 C，`game_bind_codes` 新增一行 `status=0`，`expires_at≈now+10m` |
| 8 | 5 分钟内重复 `bind` 同一服务器 | 返回**同一** code，表内仍只有一行待使用码 |
| 9 | 不同服务器分别 `bind` | 各生成一码，互不挤占限流额度 |
| 10 | 同一服务器 6 次 `bind`（每次走幂等前先删除待使用码） | 第 6 次返回限流文案 |
| 11 | 已有 `server_game_bindings(status=1)` | 文案 D，回显角色名，不新增码 |
| 12 | 禁用账号 `bind` | 文案 G（ACCOUNT_DISABLED） |
| 13 | 私聊任意非 `revolcast` 文本 | 无任何回复 |
| 14 | `revolcast foo` | 帮助文案 |
| 15 | 手工构造 `revolcast server AAAA bind`（格式非法） | 文案 F |

## 13. 实施顺序

1. 数据库索引变更（§8，含预检）。
2. repository 层：`redis_cache.go`、`game_bind_code.go`、`server_game_binding.go`、`qq_bind.go` 扩展。
3. biz 层：`GameServerService.ListAvailable` + 缓存失效；`GameBindService`。
4. transport 层：命令解析与分发；文案渲染。
5. `cmd/server/main.go` 装配。
6. 单元测试（§7.1 三个测试文件）。
7. 联调：QQ 私聊实测 §12 全用例。

## 14. 实施记录（2026-10-23）

### 14.1 落地文件

| 文件 | 说明 |
| --- | --- |
| `backend/internal/repository/redis_cache.go` | **新增**。`GetCache` / `SetCache` / `DelCache`、`GameServerListCacheKey`、`GameBindRateLimitKey` |
| `backend/internal/repository/game_bind_code.go` | **新增** `GameBindCodeRepo`（`GetPendingGameBindCode` / `CreateGameBindCode` / `reclaimExpiredPending`） |
| `backend/internal/repository/server_game_binding.go` | **新增** `ServerGameBindingRepo.FindActive`（`game_uuid::text` 扫描，避免 UUID 类型转换问题） |
| `backend/internal/repository/qq_bind.go` | 新增 `QQBindingInfo` + `GetActiveQQBinding`；`IsQQActiveBound` 改为委托它 |
| `backend/internal/biz/game_server_service.go` | 新增 `ServerCache` 依赖、`AvailableServer`、`GameServerConfig.AvailableCacheTTL/AvailableListLimit`、`ListAvailable`、`invalidateAvailableCache`（Create/Update/Disable 后调用） |
| `backend/internal/biz/game_bind_service.go` | **新增** `GameBindService`（BIZ-008 / BIZ-009）、`GameBindOutcome` / `GameBindOutcomeKind`、`GameBindConfig` |
| `backend/internal/transport/handler.go` | `commandPrefix` 前缀门禁、`parseCommand`（大小写/空白不敏感）、`renderServerList`、`renderGameBindOutcome`、`remainingMinutes`、`logBizError`；`NewBizHandler` 增加 `gameBindSvc` |
| `backend/cmd/server/main.go` | 装配 `GameBindCodeRepo` / `ServerGameBindingRepo` / `GameBindService`；`GameServerService` 注入缓存；`gameServerSvc` 提前到业务队列之前创建 |
| `backend/internal/biz/game_server_service_test.go` | 新增 `fakeServerCache`、`listCalls` 计数；补 9 个缓存相关用例 |
| `backend/internal/biz/game_bind_service_test.go` | **新增** 16 个用例（成功 / 幂等 / 已绑定 / 未绑 QQ / 服务器不可用 / 禁用账号 / 限流 / fail-open / 各类内部错误） |
| `backend/internal/transport/handler_command_test.go` | **新增** 命令解析表驱动（20 例）+ 文案渲染 + 截断 + 有效期换算 |
| `backend/internal/transport/http_game_server_test.go` | 构造函数补 `nil` 缓存参数（HTTP 路径不涉及列表缓存） |

### 14.2 与方案的差异

1. **码唯一约束名**：实测库中 `game_bind_codes.code` 的唯一性由**内联约束 `game_bind_codes_code_key`** 提供（不是文档附录写的 `ux_game_bind_codes_code`）。仓储层同时匹配两个名字，避免不同环境建法不一致。
2. **`botID` 未参与发码**：`game_bind_codes` 没有 `bot_id` 列，因此 `GenerateBindCode` 签名不含 botID（QQ 绑定的 `bot_id` 仅用于 `qq_bindings`）。
3. **未新增任何错误码**：与 §5.3 设计一致，「已绑定 / 未绑 QQ」通过 `GameBindOutcome` 返回，`errorMeta` 零改动。
4. **数据库索引已实际执行**（经 `revolcast-postgres` MCP，库 `revolcast`）：
   - 预检：`game_bind_codes` 共 **0 行**，`status=0` 的重复 `(server_id, user_id)` 组数 **0**；
   - `CREATE UNIQUE INDEX ux_game_bind_codes_pending_user ON game_bind_codes (server_id, user_id) WHERE status = 0;`
   - `DROP INDEX ix_game_bind_codes_pending_user;`
   - 执行后 `pg_indexes` 核验：仅剩 `game_bind_codes_pkey` / `game_bind_codes_code_key` / `ux_game_bind_codes_pending_user`。
5. **文档同步**：业务流程文档新增 §2.8 / §2.9 与总表两行；数据库文档更新 §3.6 索引、§5 Redis 键表、§6.2 流程第 1–2 步、附录 DDL 与执行记录（批次 3）。

### 14.3 验证

- `go build ./...` 通过；`go vet ./...` 干净；`gofmt -l` 无差异。
- `go test ./...` 全绿（biz / repository / transport / protocol / queue）。
- 新增 SQL 语句已在库上以只读方式验证语法与类型（`game_uuid::text` 可用）。
- **未做**：真实 QQ 端到端联调（需 Adapter + NapCat 在线），以及 BIZ-010 赎回链路（未实现）。
