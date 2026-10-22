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

> 命令格式（`revolcast ...`）与上表优先级（P0/P1）均为草案示例，落地前需评审确认。

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
