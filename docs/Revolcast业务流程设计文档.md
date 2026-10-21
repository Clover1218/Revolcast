# Revolcast业务流程设计文档 v1.0

本文件是**业务逻辑清单**，供开发和 AI 阅读，
## 1. 业务清单总表

| 业务ID | 业务名称 | 触发方式 | 核心处理 |
| --- | --- | --- | --- | --- |
| BIZ-001 | 生成游戏绑定码 | QQ 私聊 `revolcast user bind` | 创建绑定码并发送给用户 |

> 命令格式（`revolcast ...`）与上表优先级（P0/P1）均为草案示例，落地前需评审确认。

---

## 2. 各业务详细流程

---

### 2.1 BIZ-001：生成游戏绑定码

**触发端：** QQ 私聊 · **执行端：** Go Backend · **交付端：** QQ Adapter

**业务目标**

用户向机器人发送 `revolcast user bind`，后端为用户生成一个可用于绑定 Minecraft 游戏账号的绑定码，并通过机器人私聊返回。

> 假设 QQ 身份是后端识别用户的依据，具体身份标识字段以协议文档为准。

**主流程**

| 步骤 | 处理 |
| --- | --- |
| 01 | **接收私聊事件** — Adapter 解析平台事件，转换为标准化 DTO ，以`event.qq.message.private`的形式上报给 Backend |
| 02 | **识别业务命令** — Backend 检查消息来源、命令格式及用户身份 |
| 03 | **执行绑定码服务** — 检查现有绑定状态（未绑 QQ / 已重复绑定 / 限流），生成符合规则的绑定码 |
| 04 | **持久化绑定码** — 写入 `game_bind_codes`，确保有效期与唯一性规则成立 |
| 05 | **构造回复指令** — Backend 将回复内容和目标 QQ 用户交给 Adapter，以`command.qq.send_private_message`的形式交给 Adapter |
| 06 | **发送私聊消息** — Adapter 调用发送接口，并处理发送结果 |

**异常情况**

- `ErrQQUnowned`：该 QQ 无 active `qq_binding`，拒绝发起
- `ErrServerNotFound` / `ErrServerDisabled`：目标 server 不存在或已禁用
- `ErrAlreadyBound`：该用户在该 server 已 active 绑定（需先解绑）
- `ErrRateLimited`：命中 `game_bind:rate:user:{user_id}` 限流
- 码唯一性冲突：重试生成（8 位码排除 0O1IL）

**数据依赖**

- 写：`game_bind_codes`（INSERT）
- 读：`qq_bindings`、`game_servers`、`server_game_bindings`
- 缓存：Redis `game_bind:rate:user:{user_id}`

**协议依赖**

- WS inbound：`private_msg`
- WS outbound：`send_private_msg`

---
