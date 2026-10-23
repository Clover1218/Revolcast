# Revolcast业务流程设计文档 v1.0

本文件是**业务逻辑清单**，供开发和 AI 阅读，
## 1. 业务清单总表

| 业务ID | 业务名称 | 触发方式 | 核心处理 |
| --- | --- | --- | --- | --- |
| BIZ-001 | 生成QQ绑定码 | QQ 私聊 `revolcast user bind` | 创建 QQ 绑定码并私聊发送给用户 |
| BIZ-002 | 校验用户名可用性 | 网站 HTTP `POST /api/v1/auth/check-name` | 查询 `users` 判断用户名是否已存在（只读） |
| BIZ-003 | 注册网站账号 | 网站 HTTP `POST /api/v1/auth/register` | 校验用户名可用性、bcrypt 哈希密码并写入 `users` |
| BIZ-004 | 网站账号登录 | 网站 HTTP `POST /api/v1/auth/login` | 校验用户名/密码（bcrypt 比对）、写入 `web_sessions` 并签发会话 Token |
| BIZ-005 | 网站账号登出 | 网站 HTTP `POST /api/v1/auth/logout` | 读取会话 Cookie、按 Token 撤销 `web_sessions` 当前会话（`revoked_at`）并清除本地 Cookie |
| BIZ-006 | 网站赎回 QQ 绑定码 | 网站 HTTP `POST /api/v1/user/bind-QQ` | 已登录用户提交绑定码，原子消费 `qq_bind_codes` 并写入 `qq_bindings`（与 BIZ-001 对接） |
| BIZ-007 | 网站解绑 QQ | 网站 HTTP `POST /api/v1/user/unbind-QQ` | 仅凭会话 Cookie 将 `qq_bindings` 中当前有效绑定软解绑（`status` 1→0、记录 `unbound_at`） |
| BIZ-008 | 列出可用服务器 | QQ 私聊 `revolcast server list` | 查询 `game_servers`（`status=1`）并渲染服务器列表，带 Redis 5 分钟缓存 |
| BIZ-009 | 生成游戏角色绑定码 | QQ 私聊 `revolcast server <code> bind` | 校验服务器可用 → QQ 已绑定账号 → 无角色绑定 → 生成 `game_bind_codes` 待使用码并私聊返回 |
| BIZ-010 | 游戏内绑定角色 | 游戏内 `/revolcast bind <code>`（鹊桥 `player_command` 事件） | 限流 → 原子消费绑定码 → 写入 `server_game_bindings`，游戏内私聊回执 |
| BIZ-011 | 解除服务器角色绑定 | QQ 私聊 `revolcast server <code> unbind` | 校验服务器与 QQ 账号 → 软解绑 `server_game_bindings`（`status` 1→0、记录 `unbound_at`），QQ 回执 + 尽力而为的游戏内通知 |
| BIZ-012 | 查询服务器在线状态 | QQ 私聊 `revolcast server <code> status`（数据由每台在线服务器每 60s 的 `list uuids` 采集） | 只读 Redis 快照 `revolcast:server:status:{server_code}`，返回在线人数与玩家昵称；**不查表、不触发即时采集** |

> 命令格式（`revolcast ...`）与上表优先级（P0/P1）均为草案示例，落地前需评审确认。
>
> **命令识别位置**：QQ Adapter **不**对私聊做任何过滤或业务判断，全部私聊原样上报 Backend（见 AGENTS.md §3.2）；命令前缀 / 参数解析、非命令消息的静默丢弃、回复文案全部由 Backend 负责。

---

## 2. 各业务详细流程

---

### 2.1 BIZ-001：生成 QQ 绑定码

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter

**业务目标**

用户向机器人发送 `revolcast user bind`，后端为用户生成一个用于绑定 QQ 与网站账号的 QQ 绑定码，并通过机器人私聊返回。该绑定码由用户在网站侧提交以完成 QQ 与网站账号的绑定（对应数据库文档 §6.1）。

> QQ 身份（qq_uin）是后端识别用户的依据，具体身份标识字段以协议文档为准。生成阶段仅需 qq_uin，不需要先解析网站 user_id；user_id 在网站侧赎回绑定码时填入。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **接收私聊事件** — Adapter 解析平台事件，转换为标准化 DTO，以 `event.qq.message.private` 的形式上报给 Backend |
| 02 | **识别业务命令** — Backend 检查消息来源（仅私聊）、命令格式 `revolcast user bind` 及发送者 QQ 身份 |
| 03 | **限流检查** — 命中 Redis `revolcast:bindcode:attempt:{qq_uin}` 限流（每 qq_uin 5 分钟最多 3 次）则返回 `ErrRateLimited` |
| 04 | **已绑定检查** — 查 `qq_bindings`，若该 qq_uin 已有 active 绑定则返回 `ErrQQAlreadyBound`（需先解绑） |
| 05 | **幂等取/建绑定码** — 查是否存在未过期的待使用 `qq_bind_codes`；有则直接返回同一码（幂等）。否则先把该 `qq_uin` 已过期但仍 `status=0` 的旧码置为 `status=2`（回收唯一槽位），再生成符合规则的码 |
| 06 | **持久化绑定码** — 写入 `qq_bind_codes`（bot_id、qq_uin、expires_at≈5 分钟、status=0、trace_id），确保唯一性与并发安全（见数据库设计文档 §3.3） |
| 07 | **构造回复指令** — Backend 将绑定码文本与目标 QQ 用户交给 Adapter，以 `command.qq.send_private_message`（含 `command_id`）的形式下发 |
| 08 | **发送私聊消息** — Adapter 调用发送接口，并回 `result.qq.send_private_message` |

**异常情况**

- `ErrQQAlreadyBound`：该 QQ 已有 active `qq_bindings`，拒绝发起（需先解绑）
- `ErrRateLimited`：命中 `revolcast:bindcode:attempt:{qq_uin}` 限流
- 码唯一性冲突：重试生成（8 位码排除 0O1IL）
- 过期码占用唯一槽位：生成前将该 `qq_uin` 已过期但仍 `status=0` 的旧码置为 `status=2` 再插入（部分唯一索引 `ux_qq_bind_codes_pending_qq` 的谓词仅 `status=0`，不含过期判断，详见数据库文档 §3.3）
- 私聊发送失败：绑定码已持久化即视为业务成功；发送失败仅记录日志，用户可重新请求以获取同一码（不回滚、不失效）

**数据依赖**

- 写：`qq_bind_codes`（INSERT）
- 读：`qq_bindings`
- 缓存：Redis `revolcast:bindcode:attempt:{qq_uin}`

**协议依赖**

- WS inbound：`event.qq.message.private`
- WS outbound：`command.qq.send_private_message`
- WS result：`result.qq.send_private_message`

---

### 2.2 BIZ-002：校验用户名可用性

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

在注册表单填写阶段，前置查询目标用户名是否已被占用，避免用户提交注册后才发现冲突。仅做只读查询，不创建任何数据。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **限流检查** — 命中 Redis `revolcast:ratelimit:auth:check_name:{ip}` 限流（每 IP 5 分钟最多 10 次）则返回 `429` |
| 03 | **规范化** — 将 `username` 统一转为小写 |
| 04 | **查重** — 以 `lower(username)` 查询 `users` 表（走 `ux_users_username_lower` 索引） |
| 05 | **返回结果** — 存在返回 `data.exists=true`，否则 `false` |

**异常情况**

- `400`：缺少 `username` 或长度/格式不符合规范。
- `429`：命中 `revolcast:ratelimit:auth:check_name:{ip}` 限流。
- `500`：数据库查询失败等内部错误（不向客户端暴露明细）。

**数据依赖**

- 读：`users`（按 `lower(username)` 查询）
- 缓存：Redis `revolcast:ratelimit:auth:check_name:{ip}`

**协议依赖**

- HTTP：`POST /api/v1/auth/check-name`
- 接口契约详见《Revolcast HTTP 接口设计文档》§3

---

### 2.3 BIZ-003：注册网站账号

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

创建新的网站账号。服务端对密码使用 bcrypt（cost=12）哈希后入库，明文密码不落库、不记日志；用户名可用性由应用层预检 + 数据库唯一约束双重保证。

> 用户名冲突判定不单靠入库时的 unique key：应用层预检用于返回友好的 `409` 提示，数据库 `ux_users_username_lower` 唯一约束用于在并发/竞态下作为权威兜底（呼应《数据库表设计文档》§7.1）。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username`、`password` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **限流检查** — 命中 Redis `revolcast:ratelimit:auth:register:{ip}` 限流（每 IP 5 分钟最多 5 次）则返回 `429` |
| 03 | **规范化** — 将 `username` 统一转为小写 |
| 04 | **用户名预检** — 以 `lower(username)` 查询 `users`；若已存在返回 `409` |
| 05 | **密码哈希** — 使用 bcrypt（cost=12）对明文密码计算哈希 |
| 06 | **持久化** — 写入 `users`（`username` 小写、 `password_hash`、 `status=1`、 `created_at`/`updated_at=now()`） |
| 07 | **唯一约束兜底** — 若因并发插入触发 `ux_users_username_lower` 唯一冲突，回滚并返回 `409`，不暴露原始数据库错误 |

**异常情况**

- `400`：缺少字段或 `username`/`password` 长度/格式不符合规范。
- `409`：用户名已被占用（应用层预检命中，或数据库唯一约束兜底命中）。
- `429`：命中 `revolcast:ratelimit:auth:register:{ip}` 限流。
- `500`：数据库写入失败等内部错误。

**数据依赖**

- 写：`users`（INSERT）
- 读：`users`（用户名预检）
- 缓存：Redis `revolcast:ratelimit:auth:register:{ip}`

**协议依赖**

- HTTP：`POST /api/v1/auth/register`
- 接口契约详见《Revolcast HTTP 接口设计文档》§4

---

### 2.4 BIZ-004：网站账号登录

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

验证网站账号的用户名与密码，校验通过后创建一条 `web_sessions` 会话记录，并向客户端签发会话凭证（原始 Token）。密码比对使用 bcrypt；数据库只保存 Token 的 SHA-256 哈希（`web_sessions.token_hash`，见《数据库表设计文档》§3.9），原始 Token 仅一次性返回，服务端不再持有明文。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username`、`password` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **双重限流检查** — 分别检查 `revolcast:ratelimit:auth:login:{ip}` 与 `revolcast:ratelimit:auth:login:{username}`（各 5 分钟最多 5 次）；任一命中即返回 `429` |
| 03 | **规范化** — 将 `username` 裁剪首尾空白（保留原大小写；查重/比对大小写不敏感，与 HTTP 文档 §1.2 一致） |
| 04 | **查用户** — 以 `lower(username)` 查询 `users`；不存在则进入"凭证错误"分支返回 `401`（不暴露账号是否存在） |
| 05 | **密码比对** — 使用 bcrypt 比对 `password` 与 `users.password_hash`；不匹配返回 `401`（与"用户不存在"统一，不区分） |
| 06 | **账号状态校验** — 若 `users.status != 1`（非活跃，例如已禁用）返回 `403`（`account disabled`，明确告知账号已禁用） |
| 07 | **生成会话** — `crypto/rand` 生成原始 Token；计算 SHA-256；写入 `web_sessions`（`user_id`、`token_hash`、`device_name`、`expires_at=now()+30天`、`created_at`/`last_seen_at=now()`、`revoked_at=NULL`） |
| 08 | **下发凭证** — 在响应头 `Set-Cookie` 写入 HttpOnly 会话 Cookie（`revolcast_session`=原始 Token，有效期 30 天）；响应体返回 `expires_at`（原始 Token 仅经 Cookie 下发，不进 body） |

**异常情况**

- `400`：缺少字段或 `username`/`password` 长度/格式不符合规范。
- `401`：凭证错误（用户名不存在或密码错误，统一处理，防枚举）。
- `403`：账号不可用（`users.status != 1`，例如已禁用）。
- `429`：命中登录限流（IP 或 username 任一维度）。
- `500`：数据库查询/写入失败等内部错误。

**数据依赖**

- 读：`users`（按 `lower(username)` 查询，取 `password_hash`、`status`）
- 写：`web_sessions`（INSERT 会话记录）
- 缓存：Redis 登录限流键（双重：`revolcast:ratelimit:auth:login:{ip}` 与 `:login:{username}`）

**协议依赖**

- HTTP：`POST /api/v1/auth/login`
- 接口契约详见《Revolcast HTTP 接口设计文档》§5

---

### 2.5 BIZ-005：网站账号登出

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

撤销当前登录会话，使客户端持有的会话凭证失效。服务端从 HttpOnly Cookie `revolcast_session` 读取原始 Token，重算 SHA-256 后在 `web_sessions` 中将该会话标记为已撤销（`revoked_at`），并清除客户端本地 Cookie。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **方法校验** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **撤销会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`；未命中（令牌不存在 / 已撤销 / 已过期）返回 `401` |
| 04 | **清除 Cookie** — 下发同名空 Cookie（`Max-Age=-1`）删除本地凭证；成功返回 `200` |

**异常情况**

- `401`：未携带 Cookie / Cookie 为空，或会话已失效（已撤销、已过期或令牌不存在，不区分具体成因）。
- `405`：非 `POST` 方法。
- `500`：数据库撤销失败等内部错误。

**数据依赖**

- 写：`web_sessions`（UPDATE `revoked_at`）
- 不依赖 `users` 查询（仅按 `token_hash` 定位会话，与登录写入时的哈希逻辑同源）

**关键设计决策**

- 软撤销：`revoked_at` 置位而非硬删行，保留会话记录（与《数据库表设计文档》§3.9 一致；活跃会话判定为 `revoked_at IS NULL AND expires_at > now()`）。
- 仅撤销当前会话：服务端仅凭 Cookie 中的 Token 定位单条会话，不影响同一用户在其他设备/浏览器的有效会话。"登出全部设备"需先由 `token_hash` 查出 `user_id` 再批量撤销，本期未实现，列为后续扩展。
- 撤销动作不更新 `last_seen_at`（属登出而非活跃心跳）。
- 不施加额外限流：登出为已认证接口、风险低。
- CSRF 缓解：`revolcast_session` 为 `SameSite=Lax` 会话 Cookie，跨站 `POST` 不携带该 Cookie，天然缓解跨站登出/操作用例；本期不引入额外 CSRF Token。

**协议依赖**

- HTTP：`POST /api/v1/auth/logout`
- 接口契约详见《Revolcast HTTP 接口设计文档》§6

---

### 2.6 BIZ-006：网站赎回 QQ 绑定码

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

已登录用户提交 QQ 机器人在私聊中下发的绑定码（BIZ-001 生成于 `qq_bind_codes`），完成「网站账号 ↔ QQ 号」的绑定（写入 `qq_bindings`）。本业务是《数据库表设计文档》§6.1「QQ 绑定」步骤 4–6 的「网站侧赎回」环节，与 BIZ-001 对接：用户在 QQ 私聊发送 `revolcast user bind` → 机器人生成并下发绑定码 → 用户回到网站提交该码完成绑定。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **方法校验** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **参数校验** — `bind_code` 非空且长度为 8，否则返回 `400`（`invalid param`） |
| 04 | **解析会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET last_seen_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING user_id`；未命中返回 `401`（`not authenticated`） |
| 05 | **账号状态校验** — 查 `users.status`；若 `!= 1`（已禁用）返回 `403`（`account disabled`） |
| 06 | **限流检查** — 命中 Redis `revolcast:ratelimit:user:bind_qq:{user_id}` 限流（每用户 5 分钟最多 5 次）则返回 `429` |
| 07 | **原子消费并绑定（同一事务）** — ① `UPDATE qq_bind_codes SET status = 1, used_by_user_id = $uid, used_at = now() WHERE code = $code AND status = 0 AND expires_at > now() RETURNING qq_uin, bot_id`；0 行（码不存在 / 已使用 / 已过期）→ 返回 `400`（`bind code invalid`）；② 预检该 `user_id` 已有 active 绑定 → `409`（`user already bound`），该 `qq_uin` 已被其他账号 active 绑定 → `409`（`qq already bound`）；③ `INSERT qq_bindings(user_id, qq_uin, bot_id, status=1, bound_at, created_at, updated_at)`；并发唯一索引 `ux_qq_bindings_active_user` / `ux_qq_bindings_active_qq` 冲突回滚并映射为对应 `409`；④ 提交事务 |
| 08 | **返回结果** — 绑定成功返回 `200` + `data=null` |

**异常情况**

- `400`：缺少 `bind_code`、长度不为 8，或绑定码无效（不存在 / 已使用 / 已过期）。
- `401`：未携带 `revolcast_session` Cookie，或会话已失效（已撤销、已过期或令牌不存在）。
- `403`：账号不可用（`users.status != 1`，例如已禁用）。
- `405`：非 `POST` 方法。
- `409`：该账号已绑定其他 QQ（`user already bound`），或该 QQ 已被其他账号绑定（`qq already bound`）。
- `429`：命中 `revolcast:ratelimit:user:bind_qq:{user_id}` 限流。
- `500`：数据库查询/写入/事务失败等内部错误。

**数据依赖**

- 读：`web_sessions`（解析会话 + 刷新 `last_seen_at`）、`users`（账号状态）、`qq_bindings`（冲突预检）。
- 写：`qq_bind_codes`（消费，status 0→1）、`qq_bindings`（INSERT 绑定关系）；两者在同一事务内完成，保证原子性。

**关键设计决策**

- 绑定码消费后 `qq_bind_codes.status` 由 0（待使用）置为 1（已使用），并记录 `used_by_user_id` 与 `used_at`；该状态为权威消费标记（`status=2 已过期` 仅由后台任务用于清理/展示，不可作为安全依赖）。
- 限流维度取登录用户 `user_id`（区别于未认证接口的 IP 维度），精准约束单个已登录账号频繁试码。
- 成功响应不返回 `qq_uin`（最小信息暴露），前端可随后刷新「已绑定」状态。
- 与 BIZ-001 解耦：生成侧只需 `qq_uin`，赎回侧才填入 `user_id`，绑定码有效期由 `qq_bind_codes.expires_at` 权威判定（5 分钟）。

**协议依赖**

- HTTP：`POST /api/v1/user/bind-QQ`
- 接口契约详见《Revolcast HTTP 接口设计文档》§7

---

### 2.7 BIZ-007：网站解绑 QQ

**触发端：** 网站/外部客户端（HTTP） · **执行端：** Go Backend · **交付端：** HTTP 响应

**业务目标**

已登录用户解除「网站账号 ↔ QQ 号」的绑定。服务端**只校验会话**：从 HttpOnly Cookie `revolcast_session` 解析出 `user_id`，将 `qq_bindings` 中该账号当前有效绑定的 `status` 由 1（有效）置为 0（已解绑）并记录 `unbound_at`。本业务是 BIZ-006 的对称操作。

> 解绑为**软解绑**：保留历史行用于审计（《数据库表设计文档》§3.2「解绑时更新当前行状态并设置 `unbound_at`，保留历史记录」）；解绑后该账号可重新走 BIZ-001 + BIZ-006 绑定新的 QQ。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **方法校验** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **解析会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET last_seen_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING user_id`；未命中返回 `401`（`not authenticated`） |
| 04 | **限流检查** — 命中 Redis `revolcast:ratelimit:user:unbind_qq:{user_id}` 限流（每用户 5 分钟最多 5 次）则返回 `429` |
| 05 | **软解绑（单条原子 UPDATE）** — `UPDATE qq_bindings SET status = 0, unbound_at = now(), updated_at = now() WHERE user_id = $1 AND status = 1`；0 行（该账号当前无有效绑定）→ 返回 `409`（`not bound`）；成功 → `200` + `data=null` |

**异常情况**

- `401`：未携带 `revolcast_session` Cookie，或会话已失效（已撤销、已过期或令牌不存在）。
- `405`：非 `POST` 方法。
- `409`：该账号当前无有效 QQ 绑定（`not bound`）。
- `429`：命中 `revolcast:ratelimit:user:unbind_qq:{user_id}` 限流。
- `500`：数据库更新失败等内部错误。

**数据依赖**

- 读：`web_sessions`（解析会话 + 刷新 `last_seen_at`）。
- 写：`qq_bindings`（`status` 1→0、`unbound_at`、`updated_at`）。
- 不读 `users`、不读写 `qq_bind_codes`。

**关键设计决策**

- **只校验会话**：解绑目标由会话解析出的 `user_id` 唯一确定，接口无请求体、无绑定码入参，客户端无法指定要解绑的 QQ 号。
- **不校验 `users.status`**：解绑是释放资源、降低风险的操作，被禁用的账号也应能解除绑定（与 BIZ-006 的禁用拒绝相反）。
- **非幂等 409**：以 `409 not bound` 明确告知「当前无绑定」，与登出（会话失效即 `401`）的取向一致；相比静默返回 `200`，用户能获得真实状态反馈。
- **无需显式事务**：单条条件 `UPDATE ... WHERE status = 1` 即原子操作；并发两次解绑只有一次命中，另一次 0 行 → `409`。
- **不联动清理**：不修改 `server_game_bindings` / `server_message_subscriptions`（游戏角色绑定与私聊订阅不随 QQ 解绑自动失效）；后续推送侧按「是否存在 active QQ 绑定」判定投递目标。
- **不发 QQ 通知**：本期不引入 WS 出站指令，解绑不依赖 Adapter 在线；QQ 侧感知由用户后续操作（如再次 `revolcast user bind`）自然触发。
- **限流维度**取登录用户 `user_id`（5 分钟 5 次，`fail-open`），与 BIZ-006 对称。

**协议依赖**

- HTTP：`POST /api/v1/user/unbind-QQ`
- 接口契约详见《Revolcast HTTP 接口设计文档》§8

---

### 2.8 BIZ-008：列出可用服务器

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter

**业务目标**

用户发送 `revolcast server list`，后端返回当前**可用**（`game_servers.status = 1`，启用）的服务器列表，包含名称、简介与代号，供用户后续用代号发起角色绑定（BIZ-009）。

> 列表查询带 Redis 缓存（5 分钟），避免高频指令直接打库；该缓存是**可重建的展示数据**，不是权威配置。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **接收私聊事件** — Adapter 全量转发私聊，以 `event.qq.message.private` 上报（不做过滤） |
| 02 | **识别业务命令** — Backend 校验首词为 `revolcast`（大小写不敏感）、词序列为 `server list`；非 `revolcast` 开头的消息静默丢弃 |
| 03 | **读缓存** — `revolcast:cache:server:list:available` 命中则直接渲染，不查库 |
| 04 | **回源查库** — 未命中 / Redis 故障时查 `game_servers WHERE status = 1`（`ORDER BY created_at DESC LIMIT 100`） |
| 05 | **回填缓存** — 以 JSON 写入上述键，TTL 300s；写入失败仅记日志，不影响本次返回 |
| 06 | **渲染文案** — 每条服务器渲染「`名称：简介`」+「`代号：code`」，条目间空行分隔；简介为空显示「暂无简介」；最多展示 20 条，超出追加总数提示 |
| 07 | **下发私聊** — `command.qq.send_private_message` |

**异常情况**

- 无启用服务器：回复「当前没有可用的服务器。」
- Redis 不可用：**fail-open**，退化为直接查库（不阻断）
- 查询失败：`ErrInternal` → 通用错误文案，根因仅进日志
- 缓存与库短暂不一致：创建/更新/停用服务器成功后主动 `DEL` 缓存键；极端情况下最长 5 分钟（TTL）后自愈

**数据依赖**

- 读：`game_servers`（`status = 1`）
- 缓存：Redis `revolcast:cache:server:list:available`（TTL 300s）

**协议依赖**

- WS inbound：`event.qq.message.private`
- WS outbound：`command.qq.send_private_message`

---

### 2.9 BIZ-009：生成游戏角色绑定码

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter

**业务目标**

用户发送 `revolcast server <code> bind`，后端为该 QQ 已绑定的网站账号生成一个**指定服务器**的游戏角色绑定码，用户再到游戏内输入该码完成「网站账号 ↔ 游戏角色」绑定（对应数据库文档 §6.2，赎回侧 BIZ-010 待实现）。

> 与 BIZ-001 的关系：两者生成逻辑同构（8 位、排除 `0O1IL`、幂等取/建、回收过期槽位），差异在于 `game_bind_codes` 绑定 `server_id` + `user_id`，且有效期 10 分钟。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **识别业务命令** — 词序列 `server <code> bind`；`code` 统一转小写 |
| 02 | **服务器校验** — 走 `ResolveSubscribableServer` **直查数据库**（不吃列表缓存），格式非法 / 不存在 / 已停用统一回复「未找到该可用服务器」并给出 `server list` 指引 |
| 03 | **QQ → 网站账号** — 查 `qq_bindings`（`status=1`）取 `user_id`；无记录则提示先完成 QQ 绑定（BIZ-001 + BIZ-006） |
| 04 | **账号状态** — `users.status != 1` 拒绝发码（与 BIZ-006 口径一致） |
| 05 | **角色绑定检查** — 查 `server_game_bindings`（`server_id + user_id + status=1`）；已存在则回显角色名并说明「一个账号在一个服务器只能绑定一个角色」，**不生成新码** |
| 06 | **限流** — Redis `revolcast:ratelimit:qq:game_bind:{qq_uin}:{server_code}`，5 分钟 5 次，`fail-open` |
| 07 | **幂等取/建绑定码** — 已有未过期待使用码直接复用；否则先把该 `(server_id, user_id)` 已过期仍 `status=0` 的旧码置 `status=2`（回收唯一槽位），再生成新码 |
| 08 | **持久化** — 写 `game_bind_codes`（`code`、`server_id`、`user_id`、`qq_uin` 审计快照、`trace_id`、`status=0`、`expires_at≈10 分钟`）；`game_name` / `game_uuid` 留空待赎回收填 |
| 09 | **下发私聊** — `command.qq.send_private_message`，文案含绑定码、服务器名称/代号、有效期 |

**异常情况**

- 服务器不可用：`SERVER_NOT_FOUND` 口径文案 + `server list` 指引（不区分停用与不存在，避免枚举）
- QQ 未绑定账号：`GameBindOutcomeQQNotBound` → 引导先 `revolcast user bind`
- 已有角色绑定：`GameBindOutcomeAlreadyBound` → 回显角色名，不发码
- 账号被禁用：`ErrAccountDisabled`
- 限流：`ErrRateLimited`
- 码唯一冲突：重试生成（最多 8 次）
- 并发重复请求：部分唯一索引 `ux_game_bind_codes_pending_user` 兜底，冲突后回退查询并返回同一码
- 私聊发送失败：码已落库即视为业务成功，用户重发可拿回同一码

**关键规则**

- **限流位置在只读校验之后**：①服务器、②QQ 绑定、③账号状态、⑤角色绑定都是幂等只读查询，先给出确定性答案；限流只拦截真正会发码的请求。
- **发码前的服务器判定必须查库**：列表缓存最长滞后 5 分钟，若用缓存判定，管理员停用服务器后仍会发出无法兑换的码。
- **一个账号在一个服务器只有一个有效角色**（数据库文档 §3.7 部分唯一索引），因此不提供隐式解绑/换绑；更换角色需另行实现解绑能力。

**数据依赖**

- 读：`game_servers`、`qq_bindings`、`users`、`server_game_bindings`
- 写：`game_bind_codes`（INSERT / 过期回收 UPDATE status 2）
- 缓存：Redis `revolcast:ratelimit:qq:game_bind:{qq_uin}:{server_code}`

**协议依赖**

- WS inbound：`event.qq.message.private`
- WS outbound：`command.qq.send_private_message`

---

### 2.10 BIZ-010：游戏内绑定角色

**触发端：** 游戏内命令 · **执行端：** Go Backend · **交付端：** 鹊桥（游戏内私聊）

**业务目标**

玩家在游戏内输入 `/revolcast bind <code>`，用 QQ 侧（BIZ-009）拿到的绑定码，把自己的游戏角色绑定到码所属的网站账号，形成 `server_game_bindings(status=1)`。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **接收事件** — 鹊桥上报 `sub_type=player_command`，Backend 派生 `event.game.player_command`；`server_id` 来自已鉴权 WS 连接 |
| 02 | **落库审计** — 写入 `game_events`（保留 `command` 原文；该类型事件**不对外**，见「关键规则 5」） |
| 03 | **命令识别** — `strings.Fields` 切词；首词 `revolcast`（兼容 `revolcast:bind` 命名空间形式）→ 子命令 `bind` → 取 1 个参数；其它命令静默丢弃 |
| 04 | **码格式校验** — 去空白 + 转大写，长度 8 且属于字符集 `ABCDEFGHJKMNPQRSTUVWXYZ23456789`；不合法直接回「绑定码无效」（不查库、不计数） |
| 05 | **玩家身份校验** — `player.uuid` 必须是合法 UUID、`player.nickname` 非空且 ≤ 64 字符 |
| 06 | **限流（前置）** — Redis `revolcast:ratelimit:game:redeem:{server_id}:{player_uuid}`，5 分钟 5 次，`fail-open`；命中回「操作过于频繁」 |
| 07 | **赎回事务** — 单事务内：服务器启用校验 → `SELECT ... FOR UPDATE` 锁码（含幂等重放判定）→ 账号状态 → 账号是否已绑定 → 角色是否被占用 → 消费码并回填 `game_name`/`game_uuid` → 写入 `server_game_bindings` |
| 08 | **游戏内回执** — 通过鹊桥 `send_private_msg` 向该玩家下发结果文案 |

**异常情况**

- 码不存在 / 已过期 / 已被他人使用 / 不属于本服务器 / 格式非法 → 统一「绑定码无效或已过期」，不泄露码状态
- 同一码 + 同一角色重复提交（消息重投 / 玩家重复输入）→ 幂等返回「绑定成功」，不重复写库
- 账号在该服务器已有有效角色 → 回显角色名并说明「只能绑定一个角色」
- 角色（UUID 或角色名，名字大小写不敏感）已被其他账号绑定 → 拒绝，不顶号
- 服务器停用 → 「该服务器当前不可用」
- 账号被禁用 → `ACCOUNT_DISABLED` 文案
- 并发消费同一码 → 行锁串行化，后到者走幂等或「码无效」
- 唯一索引冲突 → 整个事务回滚（**码状态恢复为待使用**），用户可换角色重试同一码

**关键规则**

1. **限流前置**：与 BIZ-009 的「限流后置」相反。发码是产生资源，只读幂等答案不该被限流挡住；兑换是**猜码**，每次尝试都必须计数。
2. **先落库后处理业务**：游戏内命令属于审计对象，绑定逻辑失败也不能丢审计记录。
3. **角色身份以事件为准**：`game_uuid` 取 `player.uuid`、`game_name` 取 `player.nickname`，由已鉴权服务器提供，不接受客户端自报。
4. **绑定码是持有者凭证**：谁输入就把「谁的角色」绑到「码所属账号」。
5. **`player_command` 不对外**：只落库供内部审计，不推 WS、不被 HTTP 事件查询返回（命令原文可能含绑定码）。

**数据依赖**

- 读：`game_servers`、`users`、`game_bind_codes`、`server_game_bindings`
- 写：`game_bind_codes`（status 0→1 + `used_at`/`game_name`/`game_uuid`）、`server_game_bindings`（INSERT status=1）
- 缓存：Redis `revolcast:ratelimit:game:redeem:{server_id}:{player_uuid}`

**协议依赖**

- WS inbound：`event.game.player_command`（裸 JSON，非 Envelope）
- WS outbound：鹊桥 `send_private_msg`（裸 JSON，非 Envelope）

---

### 2.11 BIZ-011：解除服务器角色绑定

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter（+ 尽力而为的游戏内通知）

**业务目标**

用户发送 `revolcast server <code> unbind`，解除「该 QQ 对应网站账号」在指定服务器的有效角色绑定，使其可重新绑定新的角色。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **命令识别** — 词序列 `server <code> unbind`（大小写/空白不敏感），`code` 转小写 |
| 02 | **服务器解析** — `ResolveServer`（**不校验 `status`**）：格式非法 / 不存在统一回复「未找到该服务器」+ `server list` 指引 |
| 03 | **QQ → 网站账号** — 查 `qq_bindings(status=1)`；无记录则引导先完成 QQ 绑定 |
| 04 | **软解绑** — `UPDATE server_game_bindings SET status=0, unbound_at=now(), updated_at=now() WHERE server_id AND user_id AND status=1 RETURNING ...` |
| 05 | **QQ 回执** — 成功时回显服务器名与角色名；无有效绑定回「无需解绑」 |
| 06 | **游戏内通知（尽力而为）** — 若该服务器在线，向该角色发私聊「绑定已解除」；离线/发送失败只记日志 |

**异常情况**

- 服务器不存在 / code 格式非法 → `SERVER_NOT_FOUND` 口径文案（不区分，避免枚举）
- QQ 未绑定账号 → `GameRoleUnbindOutcomeQQNotBound`，引导 `revolcast user bind`
- 无有效绑定（含并发重复解绑）→ 「没有已绑定的角色，无需解绑」
- 解绑 UPDATE 失败 → `ErrInternal`，不得误判为「无需解绑」

**关键规则**

1. **停用的服务器仍允许解绑**：解绑对已存在关系生效，若因服务器停用而禁止，用户的历史数据会被锁死；因此使用不判 `status` 的 `ResolveServer`，而订阅/发码仍走 `ResolveSubscribableServer`。
2. **软解绑、保留历史**：仅 `status` 1→0 + `unbound_at`，不删除行（数据库文档 §3.7）。
3. **不联动 `game_bind_codes`**：已使用的码保持 `status=1` 作为审计。
4. **解绑不限流**：幂等、单行 UPDATE，且必须由已绑定该账号的 QQ 发起，风险低。
5. **通知是附属效果**：解绑结果由数据库 UPDATE 决定，通知失败不回滚、不改文案。

**数据依赖**

- 读：`game_servers`、`qq_bindings`、`server_game_bindings`
- 写：`server_game_bindings`（status 1→0）

**协议依赖**

- WS inbound：`event.qq.message.private`
- WS outbound：`command.qq.send_private_message`
- 鹊桥下行（可选）：`send_private_msg`

---

### 2.12 BIZ-012：查询服务器在线状态

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter
（数据来源：每台在线 MC 服务器的 WS 连接每 60s 主动采集一次，与用户查询解耦）

**业务目标**

用户发送 `revolcast server <code> status`，查看该服务器当前在线人数与玩家昵称。查询只读 Redis 快照，不查表、不触发即时采集。

**主流程（查询）**

| 步骤 | 处理 |
| --- | --- |
| 01 | **命令识别** — 词序列 `server <code> status`（大小写/空白不敏感），`code` 转小写 |
| 02 | **读快照** — 1 次 Redis GET `revolcast:server:status:{server_code}`（0 次查表） |
| 03 | **渲染回执** — 命中：`当前服务器有 3 人：Bot1，Bot2，Bot3`；0 人：`当前服务器没有玩家在线。`；未命中：`未找到「code」的服务器状态…` |
| 04 | **Redis 故障** — 回「状态查询暂时不可用」，**与「未找到」严格区分**（否则用户会误判服务器掉线） |

**采集流程（无用户触发，每连接一条协程）**

| 步骤 | 处理 |
| --- | --- |
| 01 | WS 连接建立（per-server secret 鉴权通过）后**立即**采集一次，此后每 60s 一次 |
| 02 | 下行 `send_rcon_command`，`data.command = list uuids`，`echo = rcon:<唯一ID>` |
| 03 | 回执按 `echo` 关联（**不进业务队列**），解析头部人数 + `昵称 (uuid)` 列表 |
| 04 | 写 Redis 快照，TTL = 3 × 采集周期；解析失败 / 超时 / 回执异常一律**不覆盖**旧值 |
| 05 | 连接注销 → 删除快照（失败由 TTL 兜底） |

**异常情况**

- 快照不存在（未连接 / 刚掉线 / 从未采集成功）→ 「未找到…请联系管理员」
- 回显格式不符（服务端 locale 非英文、RCON 未启用、`data` 为空）→ 不写，旧值自然过期
- 个别条目无法解析（昵称含半角逗号）→ 跳过该条，人数仍以头部数字为准
- 回执超时（默认 5s）→ 放弃本次，等下一周期，不重试（避免超时放大）
- Redis 读写失败 → 采集侧记日志丢弃；查询侧回「暂时不可用」

**关键规则**

1. **零查表**：键后缀用 `server_code` 而非 `server_id` —— WS 侧与 QQ 指令侧都只持有 code，读写两侧都不需要 code→id 映射。
2. **失败不覆盖**：宁可让旧快照 TTL 过期，也不用坏样本清空玩家列表。
3. **断连即删**：只有当前在线的服务器才有快照，「查不到」与「未连接」语义一致。
4. **不加限流**：查询成本恒为一次 GET（与 BIZ-008 `server list` 同级）。
5. **不落库**：状态是实时态数据，不写 PostgreSQL / MongoDB，也不进 `game_events` 审计流。

**数据依赖**

- 读 / 写：Redis `revolcast:server:status:{server_code}`（**无表操作**）

**协议依赖**

- 鹊桥下行：`send_rcon_command`（裸 JSON）+ `post_type=response` 回执（`data` 为字符串回显）
- WS inbound / outbound（QQ）：`event.qq.message.private` / `command.qq.send_private_message`

**详细设计**：`docs/服务器状态查询闭环方案.md`

---
