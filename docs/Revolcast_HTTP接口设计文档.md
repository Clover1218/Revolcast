# Revolcast HTTP 接口设计文档

**版本：** 1.0  
**整理日期：** 2026-10-21  
**适用范围：** Revolcast 网站前端（Frontend）及外部客户端访问 Go Backend 的 HTTP REST 接口  
**数据库：** PostgreSQL（`users` 表，详见《Revolcast 数据库表设计文档》§3.1）、Redis（限流，详见 §5）

> 本文档描述 Backend 对外暴露的 HTTP 接口契约（路径、请求/响应字段、限流、错误码）。  
> 与《WebSocket 消息结构设计文档》不同，HTTP 接口是 Backend 与网站/外部客户端之间的请求-响应通道，不经由 Adapter，也不使用统一的 WS `Envelope`。  
> 相关业务流程（校验用户名、注册账号）同步登记于《Revolcast 业务流程设计文档》BIZ-002、BIZ-003。

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
| `code` | `int` | 是 | 业务状态码；与 HTTP 状态码语义对齐，见 §5 错误码表 |
| `msg` | `string` | 是 | 结果描述；成功固定为 `success`，失败为简要说明，**不作为业务分支判断依据** |
| `data` | `object` / `null` | 否 | 业务数据体；无业务数据时省略或置 `null` |

> 说明：外层 `code` 为业务状态码，HTTP 状态码建议与之一致（见 §5 的 http 列）。`msg` 仅用于辅助排查，调用方不应仅凭 `msg` 文本判断分支。

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

> 两个接口均为**未认证**接口（注册/查重发生在登录之前），因此限流 `subject` 取客户端 IP，而非用户身份。

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

## 5. 限流约定

两个接口均为未认证接口，限流 `subject` 取**客户端 IP**（来源于 `X-Forwarded-For` 最左可信值或连接远端地址；**取信策略由部署拓扑确认**）。键名遵循《数据库表设计文档》§5 的 `revolcast:ratelimit:{scope}:{subject}` 模式。

| 接口 | Redis Key 模式 | 窗口 | 上限 | subject |
| --- | --- | --- | --- | --- |
| check-name | `revolcast:ratelimit:auth:check_name:{ip}` | 5 分钟 | 10 次 | 客户端 IP |
| register | `revolcast:ratelimit:auth:register:{ip}` | 5 分钟 | 5 次 | 客户端 IP |

> 限流为短期防刷保护，Redis 数据可重建；业务唯一性/正确性仍由 PostgreSQL 约束保证（见 §4.4）。

---

## 6. 错误码约定

HTTP 接口的 `code` 与 HTTP 状态码对齐，错误类别与《WebSocket 消息结构设计文档》§4 的 `error_code`（如 `RATE_LIMITED`、`INTERNAL_ERROR`）同源，此处以数值形式暴露。

| code | http | 含义 | 典型触发 |
| --- | --- | --- | --- |
| `200` | 200 | 成功 | 正常返回 |
| `400` | 400 | 参数校验失败 | 缺字段、`username`/`password` 长度或格式不符 |
| `409` | 409 | 用户名已存在 | 注册时应用层预检命中或数据库唯一约束兜底 |
| `429` | 429 | 触发限流 | 命中 Redis 限流计数（`RATE_LIMITED` 类别） |
| `500` | 500 | 服务端内部错误 | 数据库写入/查询失败等（`INTERNAL_ERROR` 类别） |

> 后续新增业务接口时，应在此处补充对应 `code`，保持单一来源。

---

## 7. 安全与合规要求

1. **密码存储** — 仅存 bcrypt 哈希（cost=12），绝不存明文；不记录原始密码到日志（呼应《数据库表设计文档》§7.3）。
2. **日志最小化** — 不记录 `password` 明文；对 `username` 等标识控制日志暴露。
3. **限流保护** — 未认证接口必须对客户端 IP 限流，防止用户名枚举与批量注册。
4. **规范化一致性** — 入参 `username` 与数据库存储/比对使用同一套小写规范化规则，避免大小写绕过唯一性。
5. **唯一性兜底** — 用户名校验与注册都不能只靠应用层先查再写；必须以数据库唯一索引为权威。
