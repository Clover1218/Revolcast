# Revolcast业务流程设计文档 v1.0

本文件是**业务逻辑清单**，供开发和 AI 阅读，
## 1. 业务清单总表

| 业务ID | 业务名称 | 触发方式 | 核心处理 |
| --- | --- | --- | --- | --- |
| BIZ-001 | 生成QQ绑定码 | QQ 私聊 `revolcast user bind` | 创建 QQ 绑定码并私聊发送给用户 |

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
| 05 | **幂等取/建绑定码** — 查是否存在未过期的待使用 `qq_bind_codes`；有则直接返回同一码（幂等），否则生成符合规则的码 |
| 06 | **持久化绑定码** — 写入 `qq_bind_codes`（bot_id、qq_uin、expires_at≈5 分钟、status=0、trace_id），确保唯一性与并发安全（见数据库设计文档 §3.3） |
| 07 | **构造回复指令** — Backend 将绑定码文本与目标 QQ 用户交给 Adapter，以 `command.qq.send_private_message`（含 `command_id`）的形式下发 |
| 08 | **发送私聊消息** — Adapter 调用发送接口，并回 `result.qq.send_private_message` |

**异常情况**

- `ErrQQAlreadyBound`：该 QQ 已有 active `qq_bindings`，拒绝发起（需先解绑）
- `ErrRateLimited`：命中 `revolcast:bindcode:attempt:{qq_uin}` 限流
- 码唯一性冲突：重试生成（8 位码排除 0O1IL）
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
