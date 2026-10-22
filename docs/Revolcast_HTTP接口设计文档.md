# Revolcast HTTP 接口设计文档

**版本：** 1.0  
**整理日期：** 2026-10-21  
**适用范围：** Revolcast 网站前端（Frontend）及外部客户端访问 Go Backend 的 HTTP REST 接口  
**数据库：** PostgreSQL（`users`、`web_sessions` 表，详见《Revolcast 数据库表设计文档》§3.1、§3.9）、Redis（限流，详见 §9）

> 本文档描述 Backend 对外暴露的 HTTP 接口契约（路径、请求/响应字段、限流、错误码）。  
> 与《WebSocket 消息结构设计文档》不同，HTTP 接口是 Backend 与网站/外部客户端之间的请求-响应通道，不经由 Adapter，也不使用统一的 WS `Envelope`。  
> 相关业务流程（校验用户名、注册账号、账号登录、网站赎回 QQ 绑定码、网站解绑 QQ）同步登记于《Revolcast 业务流程设计文档》BIZ-002、BIZ-003、BIZ-004、BIZ-006、BIZ-007。

---

## 1. 通用约定

| 项 | 约定 |
| --- | --- |
| Base Path | `/api/v1` |
| 传输协议 | HTTPS（生产）/ HTTP（本地开发） |
| 请求/响应格式 | `application/json; charset=utf-8` |
| 时间 | 所有时间字段使用 UTC |
| 字段命名 | `snake_case`（与 WS 协议及数据库字段风格一致） |

### 1.1 标准响应结构

所有接口统一返回以下外层结构：

```json
{
  "code": 200,
  "msg": "success",
  "data": null
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `code` | `int` | 是 | 业务状态码；与 HTTP 状态码语义对齐，见 §10 错误码表 |
| `msg` | `string` | 是 | 结果描述；成功固定为 `success`，失败为简要说明，**不作为业务分支判断依据** |
| `data` | `object` / `null` | 否 | 业务数据体；无业务数据时省略或置 `null` |

> 说明：外层 `code` 为业务状态码，HTTP 状态码建议与之一致（见 §10 的 http 列）。`msg` 仅用于辅助排查，调用方不应仅凭 `msg` 文本判断分支。

### 1.2 用户名大小写规则

沿用《数据库表设计文档》§3.1 的约定，并明确区分「入库」与「查重」两件事：

- **入库保留原始大小写**：用户填写的 `username` 经裁剪首尾空白后，**原样写入** `users.username`，不做强制小写转换（避免用户自定义的大小写被擅自改写）。
- **查重大小写不敏感**：判断用户名是否重复时，应用层（`repository.ExistsByUsername`）与数据库都将其**统一归一化为小写**比较——查询为 `lower(username) = lower($1)`，并命中表达式唯一索引 `ux_users_username_lower`。因此 `Clover` 与 `clover` 视为同一用户名，后者注册会返回 409。
- 长度上限 64（`VARCHAR(64)`）。
- 应用层校验（已落地于 `internal/biz/auth.go`）：裁剪空白后须匹配 `^[a-zA-Z0-9_]{3,64}$`（大小写字母 / 数字 / 下划线，长度 3–64）；`password` 最小长度 8（仅做下限防弱口令，bcrypt 无长度上限）。
- 唯一性由数据库表达式唯一索引 `ux_users_username_lower` 兜底，应用层预检仅用于给出友好提示，不能替代唯一约束。

---

## 2. 接口总览

| 接口 | 方法 | 路径 | 核心处理 |
| --- | --- | --- | --- |
| 校验用户名可用性 | `POST` | `/api/v1/auth/check-name` | 查询 `users` 表判断用户名是否已存在，并受 Redis 限流保护 |
| 注册账号 | `POST` | `/api/v1/auth/register` | 校验用户名可用性、bcrypt 哈希密码、写入 `users`，并受 Redis 限流保护 |
| 账号登录 | `POST` | `/api/v1/auth/login` | 校验用户名/密码（bcrypt 比对）、写入 `web_sessions` 并签发会话 Token，并受 Redis 限流保护 |
| 账号登出 | `POST` | `/api/v1/auth/logout` | 读取会话 Cookie、按 Token 撤销 `web_sessions` 当前会话（`revoked_at`）并清除本地 Cookie |
| 网站赎回 QQ 绑定码 | `POST` | `/api/v1/user/bind-QQ` | 已登录用户提交 QQ 机器人下发的绑定码，原子消费 `qq_bind_codes` 并写入 `qq_bindings`（BIZ-006） |
| 网站解绑 QQ | `POST` | `/api/v1/user/unbind-QQ` | 已登录用户解绑当前 QQ（`qq_bindings.status` 1→0、记录 `unbound_at`），仅校验会话（BIZ-007） |
| 创建游戏服务器 | `POST` | `/api/v1/game-servers` | 创建游戏服务器并签发一次性 API Secret（管理接口，当前暂不鉴权） |
| 游戏服务器列表 | `GET` | `/api/v1/game-servers` | 公开分页列出游戏服务器（可按 status 过滤） |
| 游戏服务器详情 | `GET` | `/api/v1/game-servers/{code}` | 公开按 code 查询单个服务器（**不含** `api_secret_hash`） |
| 更新游戏服务器 | `PATCH` | `/api/v1/game-servers/{code}` | 部分更新 name/description/status（code 与密钥不可改；管理接口，当前暂不鉴权） |
| 禁用游戏服务器 | `DELETE` | `/api/v1/game-servers/{code}` | 软禁用（status 1→0），保留审计与 FK 引用（管理接口，当前暂不鉴权） |
| 轮换服务器密钥 | `POST` | `/api/v1/game-servers/{code}/rotate-secret` | 轮换 API Secret，返回一次性新 secret（管理接口，当前暂不鉴权） |

> `check-name`、`register`、`login` 为**未认证**接口（注册/查重发生在登录之前），限流 `subject` 取客户端 IP，而非用户身份；`bind-QQ`、`unbind-QQ` 为**已认证**接口，需携带 `revolcast_session` Cookie，限流 `subject` 取登录用户 `user_id`。

---

## 3. POST `/api/v1/auth/check-name`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 3.1 业务目标

在用户填写注册表单时，前置校验目标用户名是否已被占用，避免用户提交注册后才发现冲突。仅做只读查询，不创建任何数据。

### 3.2 请求

请求体（JSON）：

```json
{
  "username": "clover"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `username` | `string` | 是 | 待校验用户名；服务端先按 §1.2 规则规范化为小写，再比对 `users.lower(username)` 唯一索引 |

### 3.3 响应

成功响应：

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "exists": false
  }
}
```

`data` 字段定义：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `exists` | `boolean` | 是 | 用户名是否已存在。`true` 表示该用户名已被占用（对应 `users` 表中已存在记录），`false` 表示可用 |

### 3.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **限流检查** — 命中 Redis `revolcast:ratelimit:auth:check_name:{ip}` 限流（每 IP 5 分钟最多 10 次）则返回 `429` |
| 03 | **规范化** — 将 `username` 统一转为小写 |
| 04 | **查重** — 以 `lower(username)` 查询 `users` 表（走 `ux_users_username_lower` 索引） |
| 05 | **返回结果** — 存在返回 `data.exists=true`，否则 `false` |

### 3.5 异常情况

- `400`：缺少 `username` 或长度/格式不符合规范。
- `429`：命中 `revolcast:ratelimit:auth:check_name:{ip}` 限流。
- `500`：数据库查询失败等内部错误（不向客户端暴露明细）。

### 3.6 数据依赖

- 读：`users`（按 `lower(username)` 查询）
- 缓存：Redis `revolcast:ratelimit:auth:check_name:{ip}`

---

## 4. POST `/api/v1/auth/register`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 4.1 业务目标

创建新的网站账号。服务端对密码使用 bcrypt（cost=12）哈希后入库，明文密码不落库、不记日志；用户名可用性由应用层预检 + 数据库唯一约束双重保证。

> 关于“是否仅靠入库时的 unique key 触发冲突”：**不推荐只依赖唯一约束**。本接口采用「应用层预检 + 数据库唯一约束兜底」双重策略——预检用于返回友好的 `409` 提示，唯一约束用于在并发/竞态下作为权威兜底（详见 §4.4）。这与《数据库表设计文档》§7.1「唯一性由数据库兜底」一致。

### 4.2 请求

请求体（JSON）：

```json
{
  "username": "clover",
  "password": "a-strong-password"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `username` | `string` | 是 | 用户名；规范化规则同 §1.2 |
| `password` | `string` | 是 | 账户明文密码；服务端使用 bcrypt（cost=12）计算哈希，结果存入 `users.password_hash`。明文不落库、不写入日志 |

### 4.3 响应

成功响应（`data` 为 `null`）：

```json
{
  "code": 200,
  "msg": "success",
  "data": null
}
```

> 注册成功不返回任何敏感信息；`data` 置 `null` 或省略。是否需要返回新账号 `id` 由后续产品需求决定（本期不返回）。

### 4.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username`、`password` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **限流检查** — 命中 Redis `revolcast:ratelimit:auth:register:{ip}` 限流（每 IP 5 分钟最多 5 次）则返回 `429` |
| 03 | **规范化** — 将 `username` 统一转为小写 |
| 04 | **用户名预检** — 以 `lower(username)` 查询 `users`；若已存在返回 `409`（`USERNAME_TAKEN`） |
| 05 | **密码哈希** — 使用 bcrypt（cost=12）对明文密码计算哈希 |
| 06 | **持久化** — 写入 `users`（`username` 小写、 `password_hash`、 `status=1`、 `created_at`/`updated_at=now()`） |
| 07 | **唯一约束兜底** — 若因并发插入触发 `ux_users_username_lower` 唯一冲突，回滚并返回 `409`（`USERNAME_TAKEN`），不暴露原始数据库错误 |

### 4.5 异常情况

- `400`：缺少字段或 `username`/`password` 长度/格式不符合规范。
- `409`：用户名已被占用（应用层预检命中，或数据库唯一约束兜底命中）。
- `429`：命中 `revolcast:ratelimit:auth:register:{ip}` 限流。
- `500`：数据库写入失败等内部错误。

### 4.6 数据依赖

- 写：`users`（INSERT）
- 读：`users`（用户名预检）
- 缓存：Redis `revolcast:ratelimit:auth:register:{ip}`

---

## 5. POST `/api/v1/auth/login`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 5.1 业务目标

验证网站账号的用户名与密码，校验通过后创建一条 `web_sessions` 会话记录，并向客户端签发会话凭证（原始 Token）。密码比对使用 bcrypt；数据库只保存 Token 的 SHA-256 哈希（`web_sessions.token_hash`，见《数据库表设计文档》§3.9），原始 Token 仅一次性返回给客户端，服务端不再持有明文。

> 登录为**未认证**接口（发生在拿到会话之前），限流 `subject` 采用**双重维度**：客户端 IP 与 username 各计一份（详见 §9 限流约定表格），任一维度命中即拒绝，以同时防分布式撞库与针对单账号的爆破。

### 5.2 请求

请求体（JSON）：

```json
{
  "username": "clover",
  "password": "a-strong-password"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `username` | `string` | 是 | 用户名；规范化规则同 §1.2（裁剪首尾空白、保留原大小写；查重/比对大小写不敏感） |
| `password` | `string` | 是 | 账户明文密码；仅用于本次 bcrypt 比对，不落库、不写入日志 |

> 入参严格为 `username` + `password`。`device_name`（写入 `web_sessions.device_name`）建议由请求头 `User-Agent` 推导，不强制客户端传参；若后续产品需要，可升级为可选字段。

### 5.3 响应

成功响应：

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "expires_at": "2026-11-20T22:00:00Z"
  }
}
```

> **会话凭证通过 HttpOnly Cookie 下发**（而非响应体）：服务端在响应头写入 `Set-Cookie: revolcast_session=<原始Token>; HttpOnly; Path=/; SameSite=Lax; Max-Age=2592000`（生产环境追加 `Secure`）。原始 Token 仅此一次经 Cookie 返回，服务端不再持有明文，数据库仅存其 SHA-256 哈希（`web_sessions.token_hash`）。后续请求由浏览器自动携带该 Cookie 完成鉴权（"当前用户"等受保护接口后续补充）。前端**切勿**在 JS 中读取或持久化该 Token。

`data` 字段定义：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `expires_at` | `string` (UTC) | 是 | 会话过期时间（RFC3339 / ISO8601 UTC），与 `web_sessions.expires_at` 一致 |

失败响应（凭证错误 / 账号不可用 / 限流）见 §10 错误码表。**为防用户名枚举**，用户名不存在与密码错误统一返回 `401`（`msg` 文案中性，如 `invalid credentials`），不在响应中区分成因。

### 5.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — 校验 `username`、`password` 存在且符合格式/长度规则，否则返回 `400` |
| 02 | **双重限流检查** — 分别检查 Redis 登录限流键 `revolcast:ratelimit:auth:login:{ip}` 与 `revolcast:ratelimit:auth:login:{username}`（各 5 分钟最多 5 次，见 §9）；任一命中即返回 `429` |
| 03 | **规范化** — 将 `username` 按 §1.2 裁剪首尾空白（保留原大小写） |
| 04 | **查用户** — 以 `lower(username)` 查询 `users`；不存在则进入"凭证错误"分支返回 `401`（不暴露账号是否存在） |
| 05 | **密码比对** — 使用 bcrypt 比对 `password` 与 `users.password_hash`；不匹配返回 `401`（与"用户不存在"统一，不区分） |
| 06 | **账号状态校验** — 若 `users.status != 1`（非活跃，例如已禁用）返回 `403`（`msg=account disabled`，明确告知账号已禁用） |
| 07 | **生成会话** — `crypto/rand` 生成原始 Token；计算 SHA-256；写入 `web_sessions`（`user_id`、`token_hash`、`device_name`、`expires_at=now()+30天`、`created_at`/`last_seen_at=now()`、`revoked_at=NULL`） |
| 08 | **下发凭证** — 在响应头 `Set-Cookie` 写入 HttpOnly 会话 Cookie（`revolcast_session`=原始 Token，有效期 30 天）；响应体返回 `data.expires_at`（原始 Token 仅经 Cookie 下发，不进 body） |

> 限流计数包含**全部**登录请求（成功与失败均计入），达到上限即返回 `429`，以统一约束登录频率。

### 5.5 异常情况

- `400`：缺少字段或 `username`/`password` 长度/格式不符合规范。
- `401`：凭证错误（用户名不存在或密码错误，统一处理，防枚举）。
- `403`：账号不可用（`users.status != 1`，例如已禁用）。
- `429`：命中登录限流（IP 或 username 任一维度）。
- `500`：数据库查询/写入失败等内部错误（不向客户端暴露明细）。

### 5.6 数据依赖

- 读：`users`（按 `lower(username)` 查询，取 `password_hash`、`status`）
- 写：`web_sessions`（INSERT 会话记录）
- 缓存：Redis 登录限流键（双重：`revolcast:ratelimit:auth:login:{ip}` 与 `:login:{username}`，见 §9）

---

## 6. POST `/api/v1/auth/logout`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 6.1 业务目标

撤销当前登录会话，使客户端持有的会话凭证失效。服务端从 HttpOnly Cookie `revolcast_session` 读取原始 Token，重算 SHA-256 后在 `web_sessions` 中将该会话标记为已撤销（`revoked_at`），并清除客户端本地 Cookie。

> 登出为**已认证**接口：服务端仅依据 Cookie 中的会话 Token 撤销「当前这一条」会话，不影响同一用户在其他设备/浏览器上的有效会话（"登出全部设备"见 §6.6 备注）。

### 6.2 请求

无需请求体。需携带登录下发的 `revolcast_session` Cookie（浏览器同源请求自动附带）。

| 凭证 | 类型 | 说明 |
| --- | --- | --- |
| `Cookie: revolcast_session=<原始Token>` | `string` | 登录下发的会话 Token；HttpOnly，前端 JS 不可读、不应持久化 |

### 6.3 响应

成功响应（`data` 为 `null`）：

```json
{
  "code": 200,
  "msg": "success",
  "data": null
}
```

响应头同时下发 `Set-Cookie: revolcast_session=; HttpOnly; Path=/; SameSite=Lax; Max-Age=-1`（生产环境追加 `Secure`），令浏览器删除本地会话 Cookie。

### 6.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **校验方法** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **撤销会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`；未命中（令牌不存在 / 已撤销 / 已过期）返回 `401` |
| 04 | **清除 Cookie** — 下发同名空 Cookie（`Max-Age=-1`）删除本地凭证；成功返回 `200` |

### 6.5 异常情况

- `401`：未携带 Cookie / Cookie 为空，或会话已失效（已撤销、已过期或令牌不存在，服务端不区分具体成因）。
- `405`：非 `POST` 方法。
- `500`：数据库撤销失败等内部错误（不向客户端暴露明细）。

### 6.6 数据依赖与备注

- 写：`web_sessions`（UPDATE `revoked_at`）。
- 软撤销策略：仅置 `revoked_at`，不硬删行，保留会话记录（与 §3.9 设计一致；活跃会话判定为 `revoked_at IS NULL AND expires_at > now()`）。
- 仅撤销当前会话；如需「登出全部设备」，需先由 `token_hash` 查出 `user_id`，再批量撤销该用户全部 `revoked_at IS NULL` 的会话——本期未实现，作为后续扩展。
- 撤销动作不更新 `last_seen_at`（属登出而非活跃心跳）。
- CSRF 缓解：`revolcast_session` 为 `SameSite=Lax` 的会话 Cookie，跨站 `POST` 不携带该 Cookie，天然缓解跨站登出/操作用例；本期不引入额外 CSRF Token。
- 登出为已认证接口、风险低，**不施加额外限流**（与 login 双重限流不同，见 §9）。

---

## 7. POST `/api/v1/user/bind-QQ`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 7.1 业务目标

登录用户提交 QQ 机器人在私聊中下发的绑定码（BIZ-001 生成于 `qq_bind_codes`），完成「网站账号 ↔ QQ 号」的绑定（写入 `qq_bindings`）。本接口是《数据库表设计文档》§6.1「QQ 绑定」步骤 4–6 的「网站侧赎回」环节，与 QQ 机器人侧生成绑定码对接（对应业务流程 BIZ-006）。

### 7.2 请求

请求体（JSON）：

```json
{
  "bind_code": "ABCDEFGH"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `bind_code` | `string` | 是 | QQ 绑定码；8 位，字符集排除 0/O/1/I/L（与 BIZ-001 生成规则一致），`char_length=8`（见《数据库表设计文档》§3.3） |

> 鉴权：需携带登录下发的 `revolcast_session` Cookie（浏览器同源请求自动附带），服务端据此解析会话并取 `user_id`。

### 7.3 响应

成功响应（`data` 为 `null`）：

```json
{
  "code": 200,
  "msg": "success",
  "data": null
}
```

> 出于最小信息暴露原则，成功响应不返回绑定后的 `qq_uin`；前端可随后刷新「已绑定」状态。

### 7.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **校验方法** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **参数校验** — `bind_code` 非空且长度为 8，否则返回 `400`（`invalid param`） |
| 04 | **解析会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET last_seen_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING user_id`；未命中（令牌不存在 / 已撤销 / 已过期）返回 `401`（`not authenticated`） |
| 05 | **账号状态校验** — 查 `users.status`；若 `!= 1`（已禁用）返回 `403`（`account disabled`） |
| 06 | **限流检查** — 命中 Redis `revolcast:ratelimit:user:bind_qq:{user_id}` 限流（每用户 5 分钟最多 5 次，见 §9）则返回 `429` |
| 07 | **原子消费并绑定（同一事务）** — ① `UPDATE qq_bind_codes SET status = 1, used_by_user_id = $uid, used_at = now() WHERE code = $code AND status = 0 AND expires_at > now() RETURNING qq_uin, bot_id`；0 行（码不存在 / 已使用 / 已过期）→ 返回 `400`（`bind code invalid`）；② 预检该 `user_id` 已有 active 绑定 → `409`（`user already bound`），该 `qq_uin` 已被其他账号 active 绑定 → `409`（`qq already bound`）；③ `INSERT qq_bindings(user_id, qq_uin, bot_id, status=1, bound_at, created_at, updated_at)`；并发唯一索引 `ux_qq_bindings_active_user` / `ux_qq_bindings_active_qq` 冲突回滚并映射为对应 `409`；④ 提交事务 |
| 08 | **返回结果** — 绑定成功返回 `200` + `data=null` |

> 绑定码消费后 `qq_bind_codes.status` 由 0（待使用）置为 1（已使用），并记录 `used_by_user_id` 与 `used_at`；该状态为权威消费标记（`status=2 已过期` 仅由后台任务用于清理/展示，不可作为安全依赖）。

### 7.5 异常情况

- `400`：缺少 `bind_code`、长度不为 8，或绑定码无效（不存在 / 已使用 / 已过期）。
- `401`：未携带 `revolcast_session` Cookie，或会话已失效（已撤销、已过期或令牌不存在）。
- `403`：账号不可用（`users.status != 1`，例如已禁用）。
- `405`：非 `POST` 方法。
- `409`：该账号已绑定其他 QQ（`user already bound`），或该 QQ 已被其他账号绑定（`qq already bound`）。
- `429`：命中 `revolcast:ratelimit:user:bind_qq:{user_id}` 限流。
- `500`：数据库查询/写入/事务失败等内部错误（不向客户端暴露明细）。

### 7.6 数据依赖与备注

- 读：`web_sessions`（解析会话 + 刷新 `last_seen_at`）、`users`（账号状态）、`qq_bindings`（冲突预检）。
- 写：`qq_bind_codes`（消费，status→1）、`qq_bindings`（INSERT 绑定关系）；两者在同一事务内完成，保证原子性。
- 限流 `subject` 取登录用户 `user_id`（区别于未认证接口的 IP 维度），键名 `revolcast:ratelimit:user:bind_qq:{user_id}`（见 §9）。
- 与 BIZ-001 的对接关系：用户在 QQ 私聊发送 `revolcast user bind` → 机器人生成并下发绑定码；本接口为网站侧赎回。绑定码 5 分钟有效期由 `qq_bind_codes.expires_at` 权威判定。

---

## 8. POST `/api/v1/user/unbind-QQ`

**触发端：** 网站/外部客户端 · **执行端：** Go Backend · **交付端：** HTTP 响应

### 8.1 业务目标

已登录用户解除「网站账号 ↔ QQ 号」的绑定关系。服务端只依据会话 Cookie 识别用户，**不做任何入参校验（无请求体）**：将 `qq_bindings` 中该账号当前有效绑定的 `status` 由 1（有效）置为 0（已解绑）并记录 `unbound_at`。解绑为软解绑，保留历史行用于审计（《数据库表设计文档》§3.2「解绑时更新当前行状态并设置 `unbound_at`，保留历史记录」），解绑后该账号可再次走 BIZ-001 + BIZ-006 重新绑定。

> 本接口是 §7（`bind-QQ`）的对称操作，对应业务流程 BIZ-007。解绑后**不**联动清理 `server_game_bindings` / `server_message_subscriptions`，也**不**发送 QQ 私聊通知。

### 8.2 请求

无需请求体。需携带登录下发的 `revolcast_session` Cookie（浏览器同源请求自动附带）。

| 凭证 | 类型 | 说明 |
| --- | --- | --- |
| `Cookie: revolcast_session=<原始Token>` | `string` | 登录下发的会话 Token；HttpOnly，前端 JS 不可读、不应持久化 |

> 本接口**不接受**任何入参：解绑目标由会话解析出的 `user_id` 唯一确定，客户端无法指定要解绑的 QQ 号。

### 8.3 响应

成功响应（`data` 为 `null`）：

```json
{
  "code": 200,
  "msg": "success",
  "data": null
}
```

> 出于最小信息暴露原则，成功响应不返回被解绑的 `qq_uin`；前端可随后刷新「已绑定」状态。

### 8.4 主流程

| 步骤 | 处理 |
| --- | --- |
| 01 | **校验方法** — 仅接受 `POST`，否则返回 `405` |
| 02 | **读取凭证** — 取 `revolcast_session` Cookie；缺失或值为空直接返回 `401`（未认证） |
| 03 | **解析会话** — 对原始 Token 重算 SHA-256，执行 `UPDATE web_sessions SET last_seen_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING user_id`；未命中（令牌不存在 / 已撤销 / 已过期）返回 `401`（`not authenticated`） |
| 04 | **限流检查** — 命中 Redis `revolcast:ratelimit:user:unbind_qq:{user_id}` 限流（每用户 5 分钟最多 5 次，见 §9）则返回 `429` |
| 05 | **软解绑（单条原子 UPDATE）** — `UPDATE qq_bindings SET status = 0, unbound_at = now(), updated_at = now() WHERE user_id = $1 AND status = 1`；0 行（该账号当前无有效绑定）→ 返回 `409`（`not bound`）；成功 → `200` + `data=null` |

> **不校验 `users.status`**：解绑是释放资源、降低风险的操作，被禁用的账号也应能解除 QQ 绑定（与 §7 的 bind-QQ 相反）。因此本接口不额外查询 `users` 表。
>
> **并发语义**：单条条件 `UPDATE ... WHERE status = 1` 本身即原子操作，无需显式事务。同一账号并发发起两次解绑时，只有一次命中并返回 `200`，另一次因 0 行影响返回 `409`。本接口**非幂等**（以 `409` 明确告知「当前无绑定」），与登出（会话失效即 `401`）的设计取向一致。

### 8.5 异常情况

- `401`：未携带 `revolcast_session` Cookie，或会话已失效（已撤销、已过期或令牌不存在）。
- `405`：非 `POST` 方法。
- `409`：该账号当前没有有效 QQ 绑定（`not bound`）。
- `429`：命中 `revolcast:ratelimit:user:unbind_qq:{user_id}` 限流。
- `500`：数据库更新失败等内部错误（不向客户端暴露明细）。

### 8.6 数据依赖与备注

- 读：`web_sessions`（解析会话 + 刷新 `last_seen_at`）。
- 写：`qq_bindings`（`status` 1→0、`unbound_at`、`updated_at`）。
- **不读** `users`（不禁用校验）、**不读写** `qq_bind_codes`（绑定码在 BIZ-006 赎回时已消费为 `status=1`，不存在残留待用码）。
- 解绑保留历史行；部分唯一索引 `ux_qq_bindings_active_user` / `ux_qq_bindings_active_qq` 仅约束 `status=1`，因此解绑后同一账号或同一 QQ 均可重新绑定。
- 不联动 `server_game_bindings` / `server_message_subscriptions`：游戏角色绑定与私聊订阅不随 QQ 解绑自动清理；后续推送侧按「是否存在 active QQ 绑定」判定投递目标。
- 不发 QQ 私聊通知：本期不引入 WS 出站指令，也不依赖 Adapter 在线。
- 限流 `subject` 取登录用户 `user_id`，键名 `revolcast:ratelimit:user:unbind_qq:{user_id}`（见 §9）。

---

## 9. 限流约定

check-name、register、login 均为未认证接口，限流 `subject` 默认取**客户端 IP**（来源于 `X-Forwarded-For` 最左可信值或连接远端地址；**取信策略由部署拓扑确认**）。键名遵循《数据库表设计文档》§5 的 `revolcast:ratelimit:{scope}:{subject}` 模式。登录接口采用双重 `subject` 维度：客户端 IP 与 username 各计一份（见上方表格）。bind-QQ、unbind-QQ 为**已认证**接口，限流 `subject` 取登录用户 `user_id`（键前缀 `revolcast:ratelimit:user:`）。

| 接口 | Redis Key 模式 | 窗口 | 上限 | subject |
| --- | --- | --- | --- | --- |
| check-name | `revolcast:ratelimit:auth:check_name:{ip}` | 5 分钟 | 10 次 | 客户端 IP |
| register | `revolcast:ratelimit:auth:register:{ip}` | 5 分钟 | 5 次 | 客户端 IP |
| login | `revolcast:ratelimit:auth:login:{ip}`、`revolcast:ratelimit:auth:login:{username}` | 5 分钟 | 5 次 | 客户端 IP + username（双重，任一命中即限流） |
| bind-QQ | `revolcast:ratelimit:user:bind_qq:{user_id}` | 5 分钟 | 5 次 | 登录用户 `user_id`（已认证接口，见 §7） |
| unbind-QQ | `revolcast:ratelimit:user:unbind_qq:{user_id}` | 5 分钟 | 5 次 | 登录用户 `user_id`（已认证接口，见 §8） |
| game-servers 管理 | `revolcast:ratelimit:server:manage:{ip}` | 5 分钟 | 20 次 | 客户端 IP（创建/更新/禁用/轮换，当前暂不鉴权，见 §12） |
| game-servers 读 | `revolcast:ratelimit:server:read:{ip}` | 5 分钟 | 60 次 | 客户端 IP（列表/详情，公开，见 §12） |

> 限流为短期防刷保护，Redis 数据可重建；业务唯一性/正确性仍由 PostgreSQL 约束保证（见 §4.4）。

---

## 10. 错误码约定

HTTP 接口的 `code` 与 HTTP 状态码对齐，错误类别与《WebSocket 消息结构设计文档》§4 的 `error_code`（如 `RATE_LIMITED`、`INTERNAL_ERROR`）同源，此处以数值形式暴露。

> **单一映射源（single source of truth）**：所有 `code` / `msg` / HTTP 状态均定义在 `backend/internal/biz/errors.go` 的 `errorMeta` 表中，由 `transport/http_auth.go` 的 `writeBizError` 通过 `biz.ResolveErrorMeta(err)` 统一推导。**禁止在 transport 层散落硬编码文案或状态码**；新增业务错误只需在 `errorMeta` 登记，HTTP 与 WS 自动同步。
>
> **错误链约定**：biz 层任何返回业务错误处都必须用 `biz.Wrap(sentinel, cause, "上下文")` 携带底层根因，**绝不允许裸 `return sentinel` 吞掉原因**。根因虽不对外暴露，但会通过 `zap.Error` 完整输出到内部日志（详见《AGENTS.md》§7）。

| code | http | 含义 | 典型触发 |
| --- | --- | --- | --- |
| `200` | 200 | 成功 | 正常返回 |
| `400` | 400 | 参数校验失败 | 缺字段、`username`/`password` 长度或格式不符；bind-QQ 的 `bind_code` 缺失/长度不为 8 或绑定码无效（不存在 / 已使用 / 已过期）亦返回 400 |
| `401` | 401 | 未授权 / 凭证错误 | 登录用户名不存在、密码错误或账号不可用（统一返回，防枚举）；登出/bind-QQ/unbind-QQ 未携带有效会话亦返回 401 |
| `409` | 409 | 资源冲突（唯一约束 / 状态冲突） | 注册时用户名已存在（应用层预检命中或数据库唯一约束兜底）；bind-QQ 时该账号已绑定其他 QQ（`user already bound`），或该 QQ 已被其他账号绑定（`qq already bound`）；unbind-QQ 时该账号当前无有效 QQ 绑定（`not bound`）；创建游戏服务器时 code 已被占用（`server code taken`，见 §12） |
| `404` | 404 | 资源不存在 | 按 code 查询/操作游戏服务器但不存在（`server not found`，见 §12） |
| `429` | 429 | 触发限流 | 命中 Redis 限流计数（`RATE_LIMITED` 类别） |
| `500` | 500 | 服务端内部错误 | 数据库写入/查询失败等（`INTERNAL_ERROR` 类别） |

> 后续新增业务接口时，应在 `backend/internal/biz/errors.go` 的 `errorMeta` 登记对应条目（新增 `code` 请保持与《WebSocket 消息结构设计文档》§4 的 `error_code` 同源），此处表格仅作对外契约展示。

---

## 11. 安全与合规要求

1. **密码存储** — 仅存 bcrypt 哈希（cost=12），绝不存明文；不记录原始密码到日志（呼应《数据库表设计文档》§7.3）。
2. **日志最小化** — 不记录 `password` 明文；对 `username` 等标识控制日志暴露。
3. **限流保护** — 未认证接口必须对客户端 IP 限流，防止用户名枚举与批量注册。
4. **规范化一致性** — 入参 `username` 与数据库存储/比对使用同一套小写规范化规则，避免大小写绕过唯一性。
5. **唯一性兜底** — 用户名校验与注册都不能只靠应用层先查再写；必须以数据库唯一索引为权威。
6. **会话凭证安全** — 原始 Token 仅经 HttpOnly Cookie（`revolcast_session`）一次性下发，数据库只存其 SHA-256 哈希（`web_sessions.token_hash`）；Cookie 设 `HttpOnly`（防 XSS 窃取）、`SameSite=Lax`，生产环境追加 `Secure`（仅 HTTPS）；前端不可在 JS 中读取或持久化 Token。
7. **防用户名枚举** — 登录失败统一返回 `401`，不在响应中区分"用户不存在"与"密码错误"，也不暴露账号是否被禁用。

---

## 12. game_servers 接口（游戏服务器管理）

**基础前缀：** `/api/v1/game-servers`  
**资源标识：** 路径统一使用 `code`（数据库稳定标识，对应表设计文档 §3.4），不使用可变的 `id` 或 `name`。  
**绝不返回字段：** 任何响应都不包含 `api_secret_hash`。  
**鉴权现状：** 读接口（列表 / 详情）**完全公开**；管理接口（创建 / 更新 / 禁用 / 轮换密钥）**当前暂不鉴权**（角色系统待引入，代码中以 `TODO(role-system)` 标注）。上线前必须由角色系统接入管理员判定，否则不应在公网暴露。  
**数据依赖：** PostgreSQL `game_servers` 表（详见《Revolcast 数据库表设计文档 v1.1》§3.4）；Redis 限流键见 §9。

### 12.1 `GameServerView` 结构

所有成功响应中服务器的统一视图（无 secret）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | `int64` | 服务器 ID |
| `code` | `string` | 服务器标识（路径使用此值） |
| `name` | `string` | 展示名称 |
| `description` | `string` \| `null` | 描述，无则 `null` |
| `status` | `int` | `1`=启用，`0`=停用 |
| `created_at` | `string`（UTC） | RFC3339 |
| `updated_at` | `string`（UTC） | RFC3339 |

### 12.2 `POST /api/v1/game-servers` — 创建

**业务目标：** 创建一条游戏服务器记录，并生成一次性 API Secret。

**请求体（JSON）：**

```json
{ "code": "survival-01", "name": "生存服", "description": "第一生存服" }
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `code` | `string` | 是 | 服务器标识；须匹配 `^[a-z0-9-]+$`，长度 1..64（与数据库 CHECK 约束一致） |
| `name` | `string` | 是 | 展示名称；非空，长度 1..128 |
| `description` | `string` | 否 | 描述；空字符串等价于无描述（存 NULL） |

**主流程：**

| 步骤 | 处理 |
| --- | --- |
| 01 | **参数校验** — `code` 格式/长度、`name` 非空/长度；否则 `400`（`server code invalid` / `server name invalid`） |
| 02 | **限流检查** — 命中 `revolcast:ratelimit:server:manage:{ip}`（5 分钟 20 次）则返回 `429` |
| 03 | **生成 Secret** — `crypto/rand` 32 字节随机 → hex 64 字符原始串；计算 `SHA256(原始串)` 作为 `api_secret_hash` |
| 04 | **持久化** — `INSERT INTO game_servers (code, name, description, api_secret_hash, status=1)`；唯一约束 `game_servers_code_key` 冲突回滚并返回 `409`（`server code taken`） |
| 05 | **返回结果** — `200`，`data` 含 `GameServerView` + 一次性原始 `secret` |

**响应（成功 `200`）：**

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "id": 1, "code": "survival-01", "name": "生存服",
    "description": "第一生存服", "status": 1,
    "created_at": "2026-10-22T07:00:00Z", "updated_at": "2026-10-22T07:00:00Z",
    "secret": "<一次性原始密钥>"
  }
}
```

> 原始 `secret` 仅在此响应出现一次；库只存其 `SHA256` 哈希，不写日志、不回传哈希以外信息。

**异常情况：** `400`（参数非法）/ `409`（code 冲突）/ `429`（限流）/ `500`（内部错误）。

### 12.3 `GET /api/v1/game-servers` — 列表（公开）

**业务目标：** 分页列出游戏服务器，无需登录。

**查询参数（均可选）：**

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `status` | `int` | `0` 或 `1`，过滤状态；缺省返回全部 |
| `limit` | `int` | 每页条数，默认 20，封顶 100 |
| `offset` | `int` | 偏移，默认 0 |

**响应（成功 `200`）：** `data` 为 `GameServerView` 数组（不含 `secret`）。  
**异常情况：** `400`（参数非法）/ `429`（限流）/ `500`。

### 12.4 `GET /api/v1/game-servers/{code}` — 详情（公开）

**业务目标：** 按 `code` 查询单个服务器（公开）。

**响应（成功 `200`）：** 单个 `GameServerView`。  
**异常情况：** `400`（code 格式非法，直接拦截不查库）/ `404`（不存在，`server not found`）/ `429` / `500`。

### 12.5 `PATCH /api/v1/game-servers/{code}` — 更新（暂不鉴权）

**业务目标：** 部分更新 `name` / `description` / `status`；`code` 与密钥不可经此修改。

**请求体（JSON，字段均可选；指针区分「未提供」与「显式置空」）：**

```json
{ "name": "新名字", "description": "", "status": 0 }
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | `string` | 否 | 非空，长度 1..128 |
| `description` | `string` \| `null` | 否 | 非 null 即更新（空字符串 → NULL） |
| `status` | `int` | 否 | 仅 `0` / `1` |

**主流程：** 参数校验 → 限流（manage）→ 动态拼装 `UPDATE SET`（`updated_at=now()`）→ 目标不存在返回 `404`。全部字段均未提供时视为空更新，幂等返回当前记录。  
**响应（成功 `200`）：** 更新后的 `GameServerView`。  
**异常情况：** `400`（参数/`status` 非法）/ `404`（不存在）/ `429` / `500`。

### 12.6 `DELETE /api/v1/game-servers/{code}` — 禁用（暂不鉴权）

**业务目标：** 软禁用服务器（`status 1→0`），保留历史行与所有外键引用（`server_game_bindings` / `server_groups` / `game_bind_codes`），符合数据库文档 §7.10「优先禁用、保留审计」。

**主流程：** 限流（manage）→ `UPDATE ... SET status=0, updated_at=now() WHERE code=$1 AND status=1`；0 行时回查区分「不存在（404）」与「已禁用（幂等成功，返回当前记录）」。  
**响应（成功 `200`）：** 当前 `GameServerView`（已禁用）。  
**异常情况：** `404`（不存在）/ `429` / `500`。

### 12.7 `POST /api/v1/game-servers/{code}/rotate-secret` — 轮换密钥（暂不鉴权）

**业务目标：** 生成新的一次性 API Secret 并更新 `api_secret_hash`；旧 secret 立即失效。

**响应（成功 `200`）：** `data` 含 `GameServerView` + 一次性新 `secret`。  
**异常情况：** `404`（不存在）/ `429` / `500`。

### 12.8 Secret 安全约束

- 库仅存 `SHA256(secret)`；原始 secret 仅在创建/轮换响应中出现一次，**不写日志、不回传哈希以外信息**。
- 校验方式（与数据库文档 §3.4 一致）：对端在 TLS 通道出示 secret，服务端比较 `SHA256(presented) == api_secret_hash`（满足「Secret 仅用于校验可保存安全哈希」）。
- 后续若需 HMAC 签名（`timestamp + nonce`）防重放，服务端须以原始密钥计算签名，则需改为可安全取回的密钥管理（KMS / 信封加密）并新增密文列——属独立改造，不在本接口范围。
- 在线人数、MOTD、心跳等动态状态不在此表，由 Redis / WS 承载，不属本 CRUD。

### 12.9 后续依赖（不在本接口范围）

- **WS 鉴权升级（已落地）：** `/api/v1/game-servers/ws` 不再使用全局 `cfg.WS.Token`，改为「对 `query.token`（原始 API Secret）取 `SHA256` 后按 `api_secret_hash` 查 `game_servers` + 要求 `status=1`」，命中则 `clientID = game_servers.id`；不再接受自声明的 `client_id`。详见《Websocket消息结构设计文档》「WS 连接与接入端身份」与《WS游戏服务器鉴权改造方案》。
- **角色系统：** 管理接口接入管理员判定后，移除 `TODO(role-system)` 并在 transport 层强制鉴权。
