# Revolcast —— 跨服/跨平台通信中枢

> 基于 Go 语言开发的 Minecraft 与 QQ 互通中枢系统。

## 1. 项目背景与目标

Revolcast 的核心痛点是解决 Minecraft 服务器（部署在简幻欢等受限环境）**端口数量受限、无法开放额外端口**（如 RCON、WebSocket Server）的问题。

通过 **"内部主动外连"** 的模式，实现 MC 服务器与外部 QQ 机器人的双向事件转发与命令执行。

## 2. 整体架构拓扑

```
[ QQ 用户 ]
    ↕ (腾讯私有协议)
[ NapCat ]                          —— 协议端：只做协议转换和收发
    ↕ (OneBot v11 协议 / WebSocket 或 HTTP)
[ Bot 框架 (NoneBot2 或 AniaBot) ]   —— 业务层：指令匹配、CQ码解析
    ↕ (HTTP API 或 WebSocket 长连接)
[ Go 中枢后端 (Revolcast Core) ]        —— 核心：WS Server、路由分发、跨服状态管理
    ↕ (WebSocket 客户端主动外连)
[ MCDR + QueQiao.MCDReforged 插件 ]  —— 每个 MC 服内部：零端口，依赖 STDIN/STDOUT
    ↕ (进程通信)
[ Minecraft 服务端 (Forge 1.20.1) ]
```

**关键设计**：MC 服内不监听任何端口，由 MCDR 插件**主动外连** Revolc 中枢，从而绕开受限环境的端口开放问题。

## 3. 数据链路与核心逻辑

### 3.1 上行链路：MC 事件 → QQ

```
MC → Revolcast → Bot → QQ
```

触发场景：MC 玩家在游戏内聊天、死亡、加入或退出。

1. **MCDR 捕获**：`QueQiao.MCDReforged` 插件捕获事件，打包成鹊桥/OneBot 标准 JSON 格式，通过已建立的 WS 长连接（由 MCDR 主动发起连接）发送给 Revolc 中枢。
2. **Revolcast 处理**：Go 后端接收 JSON，根据服务器标识符（如 `server_name`）清洗数据，转发给对应的 Bot 框架。
3. **Bot 转发**：Bot 框架将数据格式化为 QQ 消息，调用 NapCat 发送至指定 QQ 群。

### 3.2 下行链路：QQ 指令 → MC

```
QQ → Bot → Revolc → MC
```

触发场景：QQ 群内用户发送指令（如 `/list` 查询在线玩家）。

1. **Bot 解析**：Bot 框架匹配指令，通过 HTTP/WS 请求 Revolc 中枢。
2. **Revolcast 路由**：Go 后端查询在线 MC 服务器列表，向目标 MCDR 插件的 WS 连接发送命令执行请求（如 `send_rcon_command`）。
3. **MCDR 执行**：QueQiao 插件收到请求，调用 MCDR 的 `server.execute()` API，将命令写入 MC 的标准输入流（STDIN）。执行结果通过同一 WS 连接回传给 Revolc。

## 4. 关键数据结构定义（API 契约）

### 4.1 Revolc 核心路由数据（Bot ↔ Revolc）

**Bot 发给 Revolc 的指令请求：**

```json
{
  "server": "survival_1",
  "action": "execute_command",
  "params": {
    "command": "list"
  },
  "request_id": "uuid-1234"
}
```

**Revolc 返回给 Bot 的执行结果：**

```json
{
  "server": "survival_1",
  "request_id": "uuid-1234",
  "status": "success",
  "data": "There are 3 of a max of 20 players online: PlayerA, PlayerB, PlayerC"
}
```

### 4.2 鹊桥事件数据（MCDR ↔ Revolc）

**MCDR 发送给 Revolc 的玩家聊天事件：**

```json
{
  "post_type": "message",
  "server_name": "survival_1",
  "message_type": "group",
  "user_id": "player_uuid",
  "sender": {
    "nickname": "PlayerA"
  },
  "raw_message": "大家好！"
}
```

## 5. 技术栈约束与选型建议

| 模块 | 技术选型 | 约束 / 说明 |
| --- | --- | --- |
| **MC 端** | MCDR + `QueQiao.MCDReforged` 插件 | 必须配置为 **WebSocket Client（正向连接）** 模式，**拒绝监听任何新端口** |
| **Revolcast** | Go（推荐 `gorilla/websocket`） | 必须实现多实例连接管理（维护 `map[string]*websocket.Conn` 对应不同的服务器名称），并处理心跳保活与断线重连后的状态恢复 |
| **QQ 端** | Python (NoneBot2) 或 Go (AniaBot) | 追求快速开发与生态选 **NoneBot2**；追求纯 Go 技术栈统一选 **AniaBot** |
| **NapCat** | — | 保持默认配置，开启正向/反向 WS 供 Bot 框架连接 |

## 6. 当前开发任务

### 6.1 Go 模块：Revolc 服务端（WS 接入与连接管理）

基于 `gorilla/websocket` 编写 Revolc 服务端：

- 接收 MCDR 的 QueQiao 客户端连接；
- 按照 `server_name` 进行分组注册。

### 6.2 Go 模块：指令路由与异步回调

编写 HTTP/REST 接口或内部 WS 逻辑：

- 接收来自 Bot 框架的指令；
- 根据 `server_name` 将指令路由至对应的 MCDR 连接；
- 实现异步 `request_id` 的响应回调。

### 6.3 Bot 模块：QQ 机器人插件

编写一个简单的 QQ 机器人插件：

- 收到 QQ 消息 `/list` 时，调用 Revolc 中枢 API；
- 将返回的结果发送给 QQ 群。