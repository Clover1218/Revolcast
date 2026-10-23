# Revolcast-开发文档-鹊桥与服务端WS连接格式详解

本文档旨在给出QQ（Queqiao）与连接端在1.20.1纯Forge模组端交互的消息格式，一切来源于原文档 https://queqiao-docs.pages.dev/

## 基本数据结构：

### Player
| 字段名 | 数据类型 | 可能的值 | 说明 |
| :--- | :--- | :--- | :--- |
| nickname | str | - | 玩家昵称 |
| uuid | UUID | - | 玩家 UUID |
| is_op | bool | - | 玩家是否为管理员 |
| address | str | - | 玩家 IP 地址 |
| health | float | - | 当前生命值 |
| max_health | float | - | 最大生命值 |
| experience_level | int | - | 玩家经验等级 |
| experience_progress | float | - | 当前经验进度（0.0–1.0） |
| total_experience | int | - | 玩家总经验值 |
| walk_speed | float | - | 行走速度 |
| x | float | - | 坐标 X |
| y | float | - | 坐标 Y |
| z | float | - | 坐标 Z |
示例：
```json
"player": {
  "nickname": "PlantClover",
  "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
  "address": "127.0.0.1",
  "health": 20,
  "max_health": 20,
  "experience_level": 0,
  "experience_progress": 0,
  "total_experience": 0,
  "is_op": true,
  "walk_speed": 0.10000000149011612,
  "x": 5.413974010706725,
  "y": 84,
  "z": -2.6006318850226258
},
```

## 鹊桥 → 连接端

### PlayerjoinEvent-玩家加入事件
#### 原始JSON
| 字段名 | 数据类型 | 可能的值 | 说明 |
| :--- | :--- | :--- | :--- |
| timestamp | int | - | 事件时间戳 |
| post_type | str | notice | 事件类型 |
| event_name | str | PlayerJoinEvent | 事件名 |
| server_name | str | - | 服务器名称 |
| server_version | str | - | 服务器版本 |
| server_type | str | - | 服务器类型 |
| sub_type | str | player_join | 事件子类型 |
| player | Player | - | 玩家对象 |
示例：
```json
{
  "player": {
    "nickname": "PlantClover",
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "address": "127.0.0.1",
    "health": 20,
    "max_health": 20,
    "experience_level": 0,
    "experience_progress": 0,
    "total_experience": 0,
    "is_op": true,
    "walk_speed": 0.10000000149011612,
    "x": 5.413974010706725,
    "y": 84,
    "z": -2.6006318850226258
  },
  "event_name": "PlayerJoinEvent",
  "post_type": "notice",
  "sub_type": "player_join",
  "timestamp": 1792395025,
  "server_name": "TestServer",
  "server_version": "1.20.1",
  "server_type": "forge"
}
```
#### 建议转换存入MongoDB格式

> **公共约定（适用于 PlayerJoin / PlayerQuit / PlayerChat / PlayerCommand 全部事件）**
> - **集合**：统一写入新建集合 `game_events`。⚠️ 注意：与《Revolcast_数据库表设计文档_v1.1.md》§4.1 的 `server_messages`（转发到 Web/QQ 的聊天消息）**不是同一集合**；本集合是原始游戏事件审计日志，不承载转发聊天。
> - **`event_type`**：取原始 JSON 的 `sub_type` 字符串值，取值 `player_join` / `player_quit` / `player_chat` / `player_command`（**不要用 int**）。
> - **`server_id`**：`int64`，来自本服务器 WebSocket 连接的**已鉴权身份**（`clientID = game_servers.id`），**不要**由 `server_name` 反查；`server_name` 仅作为展示/自检字段原样存储。
> - **`player`**：统一保存 `{ uuid, nickname, is_op }` 快照。
> - **`occurred_at`**：由原始 `timestamp`（秒级 Unix 时间戳）转为 UTC 时间；**`received_at`**：后端收到事件后写入的 UTC 时间，截断到**毫秒**。
> - **时间字段存储类型为 BSON Date**（不是字符串）。⚠️ 历史版本曾以 `time.RFC3339Nano` 字符串存储，尾部 0 被裁剪导致小数位长度不定（`...34.5Z` 字典序大于 `...34.522048Z`，但时间更早），无法用于游标分页；已由 `backend/cmd/migrate-game-events` 一次性迁移为 Date。**新写入代码必须用 `time.Time`，禁止再写字符串**。下方 JSON 示例中的时间以 ISO8601 展示仅为可读性，Extended JSON 形式为 `{"$date":"2026-10-19T07:30:25.125Z"}`。
> - **`_id`**：由 MongoDB 自动生成 `ObjectId`，应用层不赋值（下方示例省略）。它同时是**事件的统一身份**：`InsertOne` 回传的 `_id` 直接用于生成 HTTP / WebSocket 两侧的 `id`（24 位十六进制）与分页游标，不额外生成 UUID 作为事件 ID。

以下值必填:
| 字段名 | 数据类型 | 说明 |
| :--- | :--- | :--- |
| event_type | str | 事件类型，固定为 `player_join` |
| server_name | str | 服务器名字，原样提取自原始JSON的`server_name`字段（仅展示/自检，不参与 server_id 解析） |
| server_id | int64 | 此服务器在`game_servers.id`的值，来自 WS 连接已鉴权身份 |
| player | object | 玩家对象快照 |
| player.uuid | str | 玩家uuid，提取自原始JSON的`player.uuid` |
| player.nickname | str | 玩家昵称，提取自原始JSON的`player.nickname` |
| player.is_op | bool | 玩家是否为管理员，提取自原始JSON的`player.is_op` |
| occurred_at | Date(BSON, UTC) | 事件发生时间，由原始JSON的`timestamp`（秒）转换 |
| received_at | Date(BSON, UTC) | 后端收到该事件时的时间（毫秒精度） |
| data | object | 事件数据；PlayerJoinEvent 无额外字段，固定为 `{}` |

```json
{
  "event_type": "player_join",
  "server_name": "TestServer",
  "server_id": 1,
  "player": {
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "nickname": "PlantClover",
    "is_op": true
  },
  "occurred_at": "2026-10-19T07:30:25.000Z",
  "received_at": "2026-10-19T07:30:25.125Z",
  "data": {}
}
```
### PlayerQuitEvent-玩家离开事件

| 字段名 | 数据类型 | 可能的值 | 说明 |
| :--- | :--- | :--- | :--- |
| timestamp | int | - | 事件时间戳 |
| post_type | str | notice | 通知事件类型 |
| event_name | str | PlayerQuitEvent | 事件名 |
| server_name | str | - | 服务器名称 |
| server_version | str | - | 服务器版本 |
| server_type | str | - | 服务器类型 |
| sub_type | str | player_quit | 事件子类型 |
| player | Player | - | 玩家对象 |
示例：
```json
{
  "player": {
    "nickname": "PlantClover",
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "address": "127.0.0.1",
    "health": 20,
    "max_health": 20,
    "experience_level": 0,
    "experience_progress": 0,
    "total_experience": 0,
    "is_op": true,
    "walk_speed": 0.10000000149011612,
    "x": 5.413974010706725,
    "y": 84,
    "z": -2.6006318850226258
  },
  "event_name": "PlayerQuitEvent",
  "post_type": "notice",
  "sub_type": "player_quit",
  "timestamp": 1792395023,
  "server_name": "TestServer",
  "server_version": "1.20.1",
  "server_type": "forge"
}
```
#### 建议转换存入MongoDB格式
集合 `game_events`（公共约定同上）。必填字段与 PlayerJoinEvent 一致，仅 `event_type="player_quit"`，`data` 固定为 `{}`（离开事件原始 JSON 不携带消息内容）。

| 字段名 | 数据类型 | 说明 |
| :--- | :--- | :--- |
| event_type | str | 事件类型，固定为 `player_quit` |
| server_name | str | 原样提取自原始JSON的`server_name` |
| server_id | int64 | `game_servers.id`，来自 WS 连接已鉴权身份 |
| player | object | 玩家对象快照 |
| player.uuid | str | 提取自`player.uuid` |
| player.nickname | str | 提取自`player.nickname` |
| player.is_op | bool | 提取自`player.is_op` |
| occurred_at | Date(BSON, UTC) | 由`timestamp`（秒）转换 |
| received_at | Date(BSON, UTC) | 后端收到时间（毫秒精度） |
| data | object | 固定为 `{}` |

```json
{
  "event_type": "player_quit",
  "server_name": "TestServer",
  "server_id": 1,
  "player": {
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "nickname": "PlantClover",
    "is_op": true
  },
  "occurred_at": "2026-10-19T07:30:23.000Z",
  "received_at": "2026-10-19T07:30:23.125Z",
  "data": {}
}
```

### PlayerChatEvent-玩家聊天事件
| 字段名 | 数据类型 | 可能的值 | 说明 |
| :--- | :--- | :--- | :--- |
| timestamp | int | - | 事件时间戳 |
| post_type | str | message | 事件类型 |
| event_name | str | PlayerChatEvent | 事件名 |
| server_name | str | - | 服务器名称 |
| server_version | str | - | 服务器版本 |
| server_type | str | - | 服务器类型 |
| sub_type | str | player_chat | 事件子类型 |
| message_id | str | - | 消息 ID |
| raw_message | str | - | 原始消息内容 |
| player | Player | - | 玩家对象 |
| message | str | - | 消息对象 |
```json
{
  "message": "有人吗",
  "player": {
    "nickname": "PlantClover",
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "address": "127.0.0.1",
    "health": 20,
    "max_health": 20,
    "experience_level": 0,
    "experience_progress": 0,
    "total_experience": 0,
    "is_op": true,
    "walk_speed": 0.10000000149011612,
    "x": 2.049673932101723,
    "y": 86,
    "z": 12.133213577570155
  },
  "message_id": "",
  "raw_message": "{\"text\":\"有人吗\"}",
  "event_name": "PlayerChatEvent",
  "post_type": "message",
  "sub_type": "player_chat",
  "timestamp": 1792394977,
  "server_name": "TestServer",
  "server_version": "1.20.1",
  "server_type": "forge"
}
```
#### 建议转换存入MongoDB格式
集合 `game_events`（公共约定同上）。`event_type="player_chat"`。聊天事件在 `data` 中携带消息内容。

| 字段名 | 数据类型 | 说明 |
| :--- | :--- | :--- |
| event_type | str | 事件类型，固定为 `player_chat` |
| server_name | str | 原样提取自原始JSON的`server_name` |
| server_id | int64 | `game_servers.id`，来自 WS 连接已鉴权身份 |
| player | object | 玩家对象快照（uuid / nickname / is_op） |
| occurred_at | Date(BSON, UTC) | 由`timestamp`（秒）转换 |
| received_at | Date(BSON, UTC) | 后端收到时间（毫秒精度） |
| data | object | 聊天数据 |
| data.message | str | 纯文本消息，提取自原始JSON的`message` |
| data.raw_message | str | 原始消息（Minecraft 文本组件 JSON 字符串），提取自`raw_message` |
| data.message_id | str | 消息 ID，提取自`message_id`（样本为空字符串，允许为空） |

```json
{
  "event_type": "player_chat",
  "server_name": "TestServer",
  "server_id": 1,
  "player": {
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "nickname": "PlantClover",
    "is_op": true
  },
  "occurred_at": "2026-10-19T07:29:37.000Z",
  "received_at": "2026-10-19T07:29:37.125Z",
  "data": {
    "message": "有人吗",
    "raw_message": "{\"text\":\"有人吗\"}",
    "message_id": ""
  }
}
```

> 说明：`message` 为已提取的纯文本；`raw_message` 为 Minecraft 文本组件 JSON，便于需要还原颜色/格式时回溯。若后续需要把聊天转发到 Web/QQ，应按《数据库表设计文档》§4.1 另行写入 `server_messages`，本记录仅作原始事件留存。

### PlayerCommandEvent-玩家命令事件
| 字段名 | 数据类型 | 可能的值 | 说明 |
| :--- | :--- | :--- | :--- |
| timestamp | int | - | 事件时间戳 |
| post_type | str | message | 事件类型 |
| event_name | str | PlayerCommandEvent | 事件名 |
| server_name | str | - | 服务器名称 |
| server_version | str | - | 服务器版本 |
| server_type | str | - | 服务器类型 |
| sub_type | str | player_command | 事件子类型 |
| message_id | str | - | 消息 ID |
| raw_message | str | - | 原始消息内容 |
| player | Player | - | 玩家对象 |
| command | str | - | 玩家输入的命令内容 |
示例：
```json
{
  "command": "gamemode creative",
  "player": {
    "nickname": "PlantClover",
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "address": "127.0.0.1",
    "health": 20,
    "max_health": 20,
    "experience_level": 0,
    "experience_progress": 0,
    "total_experience": 0,
    "is_op": true,
    "walk_speed": 0.10000000149011612,
    "x": 81.04514412853894,
    "y": 59,
    "z": 26.30000001192093
  },
  "message_id": "",
  "raw_message": "com.mojang.brigadier.context.CommandContextBuilder@44f89862",
  "event_name": "PlayerCommandEvent",
  "post_type": "message",
  "sub_type": "player_command",
  "timestamp": 1792395239,
  "server_name": "TestServer",
  "server_version": "1.20.1",
  "server_type": "forge"
}
```

#### 建议转换存入MongoDB格式
集合 `game_events`（公共约定同上）。`event_type="player_command"`。命令事件在 `data` 中携带命令内容。

| 字段名 | 数据类型 | 说明 |
| :--- | :--- | :--- |
| event_type | str | 事件类型，固定为 `player_command` |
| server_name | str | 原样提取自原始JSON的`server_name` |
| server_id | int64 | `game_servers.id`，来自 WS 连接已鉴权身份 |
| player | object | 玩家对象快照（uuid / nickname / is_op） |
| occurred_at | Date(BSON, UTC) | 由`timestamp`（秒）转换 |
| received_at | Date(BSON, UTC) | 后端收到时间（毫秒精度） |
| data | object | 命令数据 |
| data.command | str | 玩家输入的命令（不含前导 `/`），提取自`command` |
| data.raw_message | str | 原始消息；⚠️ 本事件中 `raw_message` 为 Java 对象 `toString`（如 `com.mojang.brigadier.context.CommandContextBuilder@...`），通常无业务价值，原样留存以便排查 |
| data.message_id | str | 消息 ID，提取自`message_id`（样本为空字符串，允许为空） |

```json
{
  "event_type": "player_command",
  "server_name": "TestServer",
  "server_id": 1,
  "player": {
    "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509",
    "nickname": "PlantClover",
    "is_op": true
  },
  "occurred_at": "2026-10-19T07:33:59.000Z",
  "received_at": "2026-10-19T07:33:59.125Z",
  "data": {
    "command": "gamemode creative",
    "raw_message": "com.mojang.brigadier.context.CommandContextBuilder@44f89862",
    "message_id": ""
  }
}
```

> 说明：`command` 是命令审计的核心字段；`raw_message` 在本事件中价值极低，仅作原始留存。建议对 `is_op=true` 玩家的命令做重点审计/检索（结合 `player.is_op`）。

## 连接端 → 鹊桥

### 广播消息
| 字段路径 | 类型 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| message | json | - | 消息内容。参考 Minecraft 文本组件。 |

附链接：https://zh.minecraft.wiki/w/%E6%96%87%E6%9C%AC%E7%BB%84%E4%BB%B6

#### 请求体
```json
{
    "api": "broadcast",
    "data": {
        "message": [
            {
                "text": " [群名1]",
                "color": "aqua"
            },
            {
                "text": " 用户",
                "color": "green"
            },
            {
                "text": "说:",
                "color": "white"
            },
            {
                "text": "聊天内容",
                "color": "white"
            }
        ]
    },
    "echo": "1"
}
```
#### 响应体
```json
{
    "code": 200,
    "api": "broadcast",
    "post_type": "response",
    "status": "SUCCESS",
    "message": "success",
    "echo": "1"
}
```

### 私聊消息
| 字段路径 | 类型 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| uuid | string | - | 接收者 UUID（优先使用），可选，如果为空则使用 nickname。 |
| nickname | string | - | 接收者昵称，当 uuid 为空时使用。 |
| message | json | - | 私聊消息内容。参考 Minecraft 文本组件。 |

+ 发送的聊天消息会在前方添加前缀，默认为：[鹊桥]
+ uuid 与 nickname 至少传递一个 若都传递，则优先使用 uuid
#### 请求体
示例：
```json
{
    "api": "send_private_msg",
    "data": {
        "uuid": null,
        "nickname": "17TheWord",
        "message": [
            {
                "text": "[私聊消息]",
                "color": "aqua"
            },
            {
                "text": "说:",
                "color": "white"
            },
            {
                "text": "聊天内容",
                "color": "white"
            }
        ]
    },
    "echo": "1"
}
```
#### 响应体
示例：
```json
{
    "code": 200,
    "api": "send_private_msg",
    "post_type": "response",
    "status": "SUCCESS",
    "message": "success",
    "data": {
        "target_player": {
            "nickname": "string",
            "uuid": "string",
            "is_op": true,
            "address": "string",
            "health": 0,
            "max_health": 0,
            "experience_level": 0,
            "experience_progress": 0,
            "total_experience": 0,
            "walk_speed": 0,
            "x": 0,
            "y": 0,
            "z": 0
        },
        "message": "Send private message success."
    },
    "echo": "1"
}
```

### 发送RCON命令

#### 请求体
| 字段路径 | 类型 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| data.command | string | - | 将要远程执行的命令 |
示例：
```json
{
    "api": "send_rcon_command",
    "data": {
        "command": "list uuids"
    },
    "echo": "1"
}
```
#### 响应体
| 字段路径 | 类型 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| data | string | - | 命令回显 |
```json
{
    "code": 200,
    "api": "send_rcon_command",
    "post_type": "response",
    "status": "SUCCESS",
    "message": "success",
    "data": "There are 3 of a max of 20 players online: Bot1 (51a4b33d-8b80-3bde-806f-de4086b03058), Bot2 (16beaa94-ff1b-37a7-8afb-c22dcb8c16d1), Bot3 (73f370b4-497b-347a-a6bb-50d07eb88947)",
    "echo": "1"
}
```
