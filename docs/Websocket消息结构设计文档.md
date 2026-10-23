# Revolcast-开发文档-WebSocket消息结构设计文档

**版本：** 1.1  
**整理日期：** 2026-10-20  
**适用范围：** Revolcast WebSocket消息结构设计
**数据库：** 主后端(Backend)、QQ机器人适配器(Revolcast-qq-adapter，以下简称Adpater)、前端(Frontend)之间WS连接通信格式

## 1. 设计思路

所有消息都以统一的`Envelop`结构体为基底，Payload如何解析由接收端以及`Type`字段自行判断决定
```go
type Envelope struct {
	Version string          `json:"version"`  // 版本，默认v1
	Type    string          `json:"type"`     // 种类
	TraceID string          `json:"trace_id"` // 链路追踪 ID
	Payload json.RawMessage `json:"payload"`  // 实际的数据体
}
```

Type字段基本格式如下：
```text
方向.来源.对象[.动作]
 │    │    │     └── 子类型/动作/属性，仅在需要细分时出现
 │    │    └────── 消息/玩家/服务器/绑定……
 │    └─────────── qq / mc / fe
 └──────────────── event / command / result / request / response / hello / ping / pong
```

## WS 连接与接入端身份（补充约定）

除统一的消息协议外，接入端与服务端的 WS 连接本身也约定如下（代码见 `backend/internal/transport/ws_server.go`）。

### 接入路由

| 路由 | client_type | 鉴权方式 | 适用接入端 |
| :--- | :--- | :--- | :--- |
| `/api/v1/adapter/ws` | `adapter` | 共享密钥（配置 `ws.token`） | QQ 机器人适配器（Revolcast-qq-adapter） |
| `/api/v1/game-servers/ws` | `mc_server` | per-server API Secret（`game_servers`） | Minecraft 服务器及未来服务端类接入端 |
| `/api/v1/game-servers/{server_code}/ws` | `web` | Session Cookie（`revolcast_session`） | 网站前端（已登录用户订阅单个服务器的游戏事件） |

- **client_type 由路由决定，客户端不可自行声明**，从而避免冒用其他接入端类型。
- `wss`（TLS）由部署层 / 后续迭代处理（如 nginx 统一 TLS 终止），不在应用层强制。
- 三类连接共用同一套连接抽象（`transport/ws_conn.go`）与 Hub（`transport/ws_hub.go`），
  但索引、协议解析与业务处理彼此独立：`web` 连接的上行消息不会进入 Adapter / GameServer 的处理链路。

### 连接查询参数

| 参数 | 适用路由 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `token` | 两者 | 是 | adapter：共享密钥 `ws.token`；game-servers：创建/轮换时一次性下发的**原始 API Secret** |
| `client_id` | 仅 `/api/v1/adapter/ws` | 是 | 全局唯一身份 ID（如 `revolcast-qq-bot`），用于回传路由与去重。`/api/v1/game-servers/ws` **不接受**该参数，身份由 secret 派生 |

### `/api/v1/game-servers/ws` 接入鉴权

1. 取 `query.token`（原始 API Secret），缺失即 `401`。
2. 服务端计算 `SHA256(token)`，按 `api_secret_hash` 查 `game_servers`（**要求 `status = 1`**）。
   - 命中 → `clientID = game_servers.id`（十进制字符串），`client_type = mc_server`。
   - secret 错误 / 服务器不存在 / 已停用（`status = 0`）/ DB 异常 → 一律 `401`，不向客户端区分成因（避免枚举与信息泄露），根因仅进内部日志。
3. 身份由 secret 派生，客户端无法自报或冒用其他服务器；`client_id` 参数被忽略。
4. 服务器被停用或轮换 secret 后，旧连接**不会被立即断开**（下次重连才校验），如需强制下线见后续迭代。

### `/api/v1/game-servers/{server_code}/ws` 前端订阅（Web → Backend）

前端通过该路由订阅**单个**服务器的游戏事件，服务端只推送该 `server_code` 的事件，不做全局广播。

**握手流程**（任一步失败都不升级连接，错误以 HTTP 统一响应体返回，见《HTTP 接口设计文档》§1.1）：

1. **Origin 校验**：按 `ws.allowed_origins` 白名单精确匹配（大小写与末尾斜杠已归一化）。
   白名单为空时：开发环境放行，生产环境拒绝所有浏览器来源（fail-closed）。不通过返回 `403`。
2. **会话鉴权**：从 Cookie `revolcast_session` 取原始 Token，经 `biz.AuthService.ResolveUserID`
   （与登录/登出同源哈希）解析出 `user_id`；缺失 / 会话失效返回 `401`，**不升级连接**。
3. **握手限流**：Redis 共享计数，5 分钟窗口，按 `user_id`（20 次）与客户端 IP（60 次）双维度，
   任一命中返回 `429`。连接建立后的消息与心跳不计数；前端断线重连因会重新握手而纳入限流。
4. **订阅目标校验**：`server_code` 格式非法 `400`、不存在 `404`、已停用 `403`（`SERVER_DISABLED`）。
5. 升级连接 → 创建 Web 连接 → 注册 Hub（仅接收该 `server_code` 的事件）。

**连接标识**：

| 项 | 说明 |
| :--- | :--- |
| `connection_id` | 服务端为每条连接生成的 32 字符随机串，是 Hub 的唯一索引键 |
| `user_id` | 握手解析得到的登录用户，用于「按用户推送」 |
| `server_code` / `server_id` | 订阅目标；事件推送按 `server_code` 分桶，事件归属由 `server_id` 定位 |

- **Session Token 不作为连接标识**：Token 是可撤销凭证，只用于握手鉴权，绝不作为 Map 键。
- 同一用户可同时建立多条 Web 连接（多标签页 / 多设备），彼此不互斥、不踢旧。

### 推送消息：`event.game.<sub_type>`

游戏事件**落库成功后**，推送给订阅该服务器的全部 Web 连接（不广播给其他服务器）。

```json
{
  "version": "v1",
  "type": "event.game.player_chat",
  "trace_id": "与产生该事件的链路一致",
  "payload": {
    "id": "6ada272e0315fd31cf53187c",
    "cursor": "eyJ0IjoxNzkyMzk1MDI1MTI1LCJpZCI6IjZhZGEyNzJlMDMxNWZkMzFjZjUzMTg3YyJ9",
    "server_code": "survival",
    "server_id": 10,
    "event_type": "player_chat",
    "player": { "uuid": "a3b91ae7", "nickname": "Clover", "is_op": false },
    "occurred_at": "2026-10-19T07:30:25.000Z",
    "received_at": "2026-10-19T07:30:25.125Z",
    "data": { "message": "hello", "raw_message": "{\"text\":\"hello\"}", "message_id": "m1" }
  }
}
```

| 字段 | 说明 |
| :--- | :--- |
| `type` | `event.game.player_join` / `player_quit` / `player_chat`（`player_command` **不推送**，见下） |
| `id` | **事件统一身份**：持久化文档 `_id` 的 24 位十六进制串。前端**唯一去重键**，与 HTTP 事件查询 `items[].id` 完全相同 |
| `cursor` | 分页游标，由该事件的 `(received_at, _id)` 确定性生成，与 HTTP `items[].cursor` 完全相同；前端只保存与回传，**不解析**内部结构 |
| `server_code` / `server_id` | 事件所属服务器（`server_id` 由服务端已鉴权身份派生，不可由 `server_name` 反查） |
| `occurred_at` / `received_at` | 事件发生时间与服务端收到时间，UTC RFC3339 **固定毫秒**格式（`2006-01-02T15:04:05.000Z`）；`received_at` 在落库前已截断到毫秒，与 MongoDB 存储、HTTP 事件查询（《HTTP 接口设计文档》§13）指向同一时刻 |
| `data` | join/quit 为 `{}`；chat 为 `{message, raw_message, message_id}` |

**`player_command` 不进入公开事件流。** 玩家命令事件仅作为**内部审计**写入 `game_events`（保留 `command` 原文），既不通过本推送下发，也不被 HTTP 事件查询返回（《HTTP 接口设计文档》§13.4）。原因：`/revolcast bind <code>` 的原文包含游戏角色绑定码，属于一次性持有者凭据。业务处理见《Revolcast业务流程设计文档》§2.10；注意「不推送」不等于「不处理」——命令事件仍会触发绑定业务，并通过**鹊桥下行**给玩家回执。

### 下行指令：`/api/v1/game-servers/ws`（Backend → 鹊桥）

`/api/v1/game-servers/ws` 的对端是运行在 MC 服务端的鹊桥 mod，它**不认识统一 Envelope**：上行事件是裸 JSON（按 `sub_type` 派生 `event.game.<sub_type>`），下行指令同样是鹊桥原生裸 JSON。Envelope 只适用于 Adapter 与 Web 两个接入端。

当前使用的下行 API：

| API | 用途 | 触发业务 |
| :--- | :--- | :--- |
| `send_private_msg` | 向游戏内单个玩家发私聊（Minecraft 文本组件） | BIZ-010 绑定结果回执、BIZ-011 解绑通知 |
| `send_rcon_command` | 在服务器内执行一条 RCON 命令（本期固定 `list uuids`） | BIZ-012 在线状态采集（每连接每 60s） |
| `broadcast` | 服务器内广播 | 暂未使用 |

```json
{ "api": "send_private_msg",
  "data": { "uuid": "a3b91ae7-9efd-3e2e-8907-3db4a6c53509", "nickname": "Clover",
            "message": [{ "text": "✔ 绑定成功", "color": "green" }] },
  "echo": "c0ffee..." }
```

```json
{ "api": "send_rcon_command", "data": { "command": "list uuids" }, "echo": "rcon:..." }
```

- 发送方**只保证投递到该服务器的 WS 连接**（按已鉴权的 `server_id` 定位），不保证玩家收到；
- 回执统一为 `post_type=response`，`echo` 原样返回，用于关联请求与回执。**等待策略按 API 区分**：
  - `send_private_msg` / `broadcast`：**不等待**回执，仅按 `code`/`status` 记日志；通知失败不改变业务结论（绑定/解绑以数据库事务结果为准）。
  - `send_rcon_command`：**等待**同 `echo` 的回执（默认 ≤5s），用于取命令回显（`data` 为字符串）。超时/失败只放弃本次采集，不重试。回执**不进入业务队列**，由发送方（状态采集协程）消费。
- 注意 `data` 的类型随 API 变化：`send_private_msg` 为对象，`send_rcon_command` 为**字符串**，因此回执结构体以 `json.RawMessage` 承载 `data`，按 `api` 分别解码。

详细协议见《鹊桥与服务端WS连接格式详解》「连接端 → 鹊桥」。

- **数据来源单一**：`id` / `cursor` / `received_at` 全部取自同一条持久化记录（`InsertOne` 回传的 `_id` + 落库前已截断的 `received_at`），不在推送侧重新生成时间戳、ID 或游标。故同一条事件在 HTTP 与 WS 两侧身份完全一致。
- **`trace_id` 不得代替事件 ID**：它只用于串联链路（例如把一次落库与一次推送日志关联），既不稳定也不唯一（同一条链路的多条消息共用），不能作为去重键。
- `data.raw_message` 仅 chat 下发（Minecraft 文本组件 JSON）；command 的 `raw_message` 是 Java 对象 `toString`，无业务价值，不下发。
- 推送是**尽力而为**：单个连接发送队列满时只丢弃该连接的消息，不影响落库结果与其他连接。推送丢失的事件不会被补发，前端需在重连后用 HTTP `after` + `cursor` 补齐（重叠部分按 `id` 去重）。
- 消息外层仍是统一 Envelope，与 Adapter / GameServer 共用协议封装，但**消息方向不同**：这是 `event.*`（服务端 → 前端），不涉及 `command.*`。

### 身份与连接约束

- **单身份单连接**：同一接入端身份同时只允许一个有效连接。出现重复连接时，服务端**踢掉旧连接**由新连接接管；旧连接随后自动注销，不会误删新映射。
  - `adapter` 的身份键是 `client_id`（如 `revolcast-qq-bot`）；
  - `mc_server` 的身份键是 `server_code`（同一台服务器重连即触发踢旧，天然避免重复在线）；
  - `web` 每条连接独立（`connection_id`），不做互斥替换。
- **索引隔离**：Hub 为三类接入端各自维护连接索引，标识相同的字符串在不同接入端之间互不冲突（见 `transport/ws_hub.go`）。
- **回传路由**：业务层通过 `SendToClient(clientID)` 回传指令（先查 adapter 的 `client_id`，再查 `server_code`）；客户端不在线或发送队列满时返回失败，调用方（Worker）不因此阻塞。
- **业务归属**：`mc_server` 上报事件时，业务层使用的服务器身份是 `game_servers.id`（十进制字符串，来自已鉴权身份），连接索引键仍是 `server_code`，二者不可混用。
- **凭证与身份分离**：`adapter` 仍为共享 Token + 自声明 `client_id`；`mc_server` 为 per-server 独立凭证（secret 派生身份）；`web` 为登录会话 Cookie（Token 仅用于鉴权，不参与连接标识）。后续引入权限模型时，应在连接建立处补充服务器级权限校验（例如 `server_game_bindings` 归属校验与隐身角色过滤）。

## 2. Adapter → Backend

### event.qq.message.group/event.qq.message.private 收到 QQ 群聊/私聊 消息 
	
#### 消息结构定义
两者消息结构相同，只做业务逻辑上的区分，两者均使用统一的 Envelope：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `version` | `string` | 是 | 协议版本，当前为 `v1` |
| `type` | `string` | 是 | 事件类型，区分群聊和私聊 |
| `trace_id` | `string` | 是 | 本次消息处理链路的追踪标识 |
| `payload` | `object` | 是 | 标准化后的消息事件数据 |

`payload`字段定义如下。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `platform` | `string` | 是 | 平台标识，当前固定为 `qq` |
| `message_id` | `string` | 是 | QQ 平台原生消息 ID |
| `self_id` | `string` | 是 | 当前接收消息的 QQ 机器人 ID |
| `conversation` | `object` | 是 | 会话信息 |
| `conversation.type` | `string` | 是 | 会话类型，取值为 `group` 或 `private` |
| `conversation.id` | `string` | 是 | 会话标识；群聊为群号，私聊为对方 QQ 号 |
| `sender` | `object` | 是 | 发送者信息 |
| `sender.id` | `string` | 是 | 发送者 QQ 号 |
| `sender.nickname` | `string` | 否 | 发送者昵称 |
| `sender.card` | `string` | 否 | 群名片，仅群聊通常具有此字段 |
| `message` | `object` | 是 | 消息内容 |
| `message.text` | `string` | 是 | 消息的文本表示，供普通文本处理和意图识别使用 |
| `message.segments` | `array` | 是 | 按原始顺序排列的消息段 |
| `is_to_me` | `boolean` | 是 | ZeroBot 是否判定该消息与机器人有关，例如被直接 @ |
| `platform_data` | `object` | 否 | 经过筛选的平台扩展信息 |
| `event_time`	| `int64` |	是 | 事件发生时间，UTC 毫秒。由 Adapter 将 QQ 平台给出的秒级时间戳，都归一化为毫秒后填入；0 表示平台未提供时间 |
| `received_at` | `int64` | 是 | Adapter 收到事件的时间，UTC 毫秒。用于链路延迟监控，不作为消息排序依据 |

`message.segment`用于保留消息中的文本和非文本内容，避免仅传输纯文本导致图片、@ 等信息丢失。

每个`message.segment`包含以下字段：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | `string` | 是 | 消息段类型 |
| `data` | `object` | 是 | 该消息段的数据，具体字段由类型决定 |

当前建议支持以下常见类型：

| 类型 | 主要数据字段 | 说明 |
|---|---|---|
| `text` | `text` | 文本内容 |
| `at` | `qq` | 被 @ 的 QQ 号 |
| `image` | `file`、`url` | 图片标识或可访问地址，具体字段按 Adapter 实际能力提供 |
| `face` | `id` | 表情 ID |
| `file` | `file`、`name` | 文件标识及文件名 |

`platform_data` 用于存放少量对业务或问题排查有价值、但尚未纳入统一字段的 QQ/OneBot 信息，例如：

- `post_type`
- `detail_type`
- `sub_type`

这些字段应经过筛选，不直接复制整个 ZeroBot 事件对象。若后续某个平台字段被多个业务模块稳定依赖，应评估是否将其提升为统一字段。

#### 示例

收到群号为 `200001` 的群聊消息，发送者为 `300001`，消息内容为“服务器几人？”。
```json
{
  "version": "v1",
  "type": "event.qq.message.group",
  "trace_id": "019a1234-5678-7abc-8def-0123456789ab",
  "payload": {
    "platform": "qq",
    "message_id": "123456",
    "self_id": "100001",
    "conversation": {
      "type": "group",
      "id": "200001"
    },
    "sender": {
      "id": "300001",
      "nickname": "Clover",
      "card": "群成员"
    },
    "message": {
      "text": "服务器几人？",
      "segments": [
        {
          "type": "text",
          "data": {
            "text": "服务器几人？"
          }
        }
      ]
    },
    "is_to_me": false,
    "event_time": 1734567890123,
    "received_at": 1734567890156,
    "platform_data": {
      "post_type": "message",
      "detail_type": "group",
      "sub_type": "normal"
    }
  }
}
```
说明：
- `trace_id` 标识本次消息的处理链路，由协议发送方按照统一的追踪规则生成。
- `message_id` 是 QQ 平台的原生消息 ID，与 `trace_id` 用途不同。
- `conversation.id` 为群号，而不是发送者 QQ 号。
- `message.segments` 保留消息段结构，即使当前消息只有纯文本，也使用统一的数据格式。
- `is_to_me` 为 `false` 表示该消息未被 ZeroBot 判定为直接发给机器人；它不代表消息一定与机器人业务无关。


收到 QQ 号为 `300001` 的用户发送的私聊消息，内容为“你好”。

```json
{
  "version": "v1",
  "type": "event.qq.message.private",
  "trace_id": "019a1234-5678-7abc-8def-1123456789ab",
  "payload": {
    "platform": "qq",
    "message_id": "123457",
    "self_id": "100001",
    "conversation": {
      "type": "private",
      "id": "300001"
    },
    "sender": {
      "id": "300001",
      "nickname": "Clover"
    },
    "message": {
      "text": "你好",
      "segments": [
        {
          "type": "text",
          "data": {
            "text": "你好"
          }
        }
      ]
    },
    "is_to_me": true,
    "event_time": 1734567890123,
    "received_at": 1734567890156,
    "platform_data": {
      "post_type": "message",
      "detail_type": "private",
      "sub_type": "friend"
    }
  }
}
```

说明：
- 私聊中的 `conversation.id` 为对方 QQ 号。
- `sender.id` 表示实际发送者，因此在普通私聊场景下通常与 `conversation.id` 相同。
- `sender.card` 等不适用的字段直接省略，不必填入空字符串或 `null`。
- 示例中的 `sub_type` 仅用于说明可能存在的平台扩展信息，实际值应由 Adapter 根据收到的事件填写，不得写死。

#### Go 数据结构建议

QQ Adapter 可以使用以下 DTO 表示标准化后的消息。该 DTO 仅用于协议数据，不依赖 ZeroBot 的运行时 `Ctx`。

```go
package protocol

import "encoding/json"

type QQMessagePayload struct {
    Platform     string            `json:"platform"`
    MessageID    string            `json:"message_id"`
    SelfID       string            `json:"self_id"`
    Conversation QQConversation    `json:"conversation"`
    Sender       QQSender          `json:"sender"`
    Message      QQMessage         `json:"message"`
    IsToMe       bool              `json:"is_to_me"`
    PlatformData map[string]string `json:"platform_data,omitempty"`
    IsToMe       bool              `json:"is_to_me"`
    EventTime    int64             `json:"event_time"`    // UTC 毫秒；0 表示平台未提供
    ReceivedAt   int64             `json:"received_at"`   // UTC 毫秒；Adapter 收到时间
}

type QQConversation struct {
    Type string `json:"type"`
    ID   string `json:"id"`
}

type QQSender struct {
    ID       string `json:"id"`
    Nickname string `json:"nickname,omitempty"`
    Card     string `json:"card,omitempty"`
}

type QQMessage struct {
    Text     string        `json:"text"`
    Segments []QQSegment   `json:"segments"`
}

type QQSegment struct {
    Type string                     `json:"type"`
    Data map[string]string    `json:"data"`
}
```

这里的 `QQSegment.Data` 使用 `json.RawMessage`，是为了容纳不同消息段的不同数据结构，并避免把所有数据强制限制为字符串。Adapter 可以根据具体消息段写入对应 JSON 值。


## 3. Backend → Adapter

### command.qq.send_private_message

#### 消息结构定义
该消息使用统一的 Envelope：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `version` | `string` | 是 | 协议版本，当前为 `v1` |
| `type` | `string` | 是 | 事件类型，区分群聊和私聊 |
| `trace_id` | `string` | 是 | 本次消息处理链路的追踪标识 |
| `payload` | `object` | 是 | 标准化后的消息事件数据 |

`payload` 字段定义如下：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `receiver_id` | `string` | 是 | 接收者 QQ 号 |
| `message_segments` | `array` | 是 | 消息段数组，见下 |

`message_segments`为`message_segment`类型的数组，用于保留消息中的文本和非文本内容，避免仅传输纯文本导致图片、@ 等信息丢失。

每个`message_segment`包含以下字段：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | `string` | 是 | 消息段类型 |
| `data` | `object` | 是 | 该消息段的数据，具体字段由类型决定 |

当前建议支持以下常见类型：

| 类型 | 主要数据字段 | 说明 |
|---|---|---|
| `text` | `text` | 文本内容 |
| `at` | `qq` | 被 @ 的 QQ 号 |
| `image` | `file`、`url` | 图片标识或可访问地址，具体字段按 Adapter 实际能力提供 |
| `face` | `id` | 表情 ID |
| `file` | `file`、`name` | 文件标识及文件名 |


#### 示例

向QQ号为`66666666`发送 服务器2人 的消息
```json
{
  "version": "v1",
  "type": "command.qq.send_private_message",
  "trace_id": "019a1234-5678-7abc-8def-0123456789ab",
  "payload": {
    "receiver_id": "123456",
    "message_segments": {
      "text": "服务器几人？",
      "segments": [
        {
          "type": "text",
          "data": {
            "text": "服务器2人"
          }
        }
      ]
    }
  }
}
```
补充说明（建议加到示例下面）：
+ `type` 是 Backend → Adapter 的指令方向，与入站 `event.qq.message.*` 严格区分。
+ `trace_id` 由 Backend 生成；Adapter 执行完必须回 result.qq，`trace_id` 必须等于此值。
+ 即使内容只有纯文本，也使用 `message_segments` 数组，保持统一格式。禁止再提供 `text` 快捷字段，避免双份真相。

#### Go 数据结构建议

QQ Adapter 可以使用以下 DTO 表示标准化后的消息。该 DTO 仅用于协议数据，不依赖 ZeroBot 的运行时 `Ctx`。

```go
package protocol

import "encoding/json"

// QQSendPrivateMsgPayload 是 Backend → Adapter 的私聊发送指令 payload。
// 注意：这不是入站消息的 QQMessagePayload，两者字段完全不同，不要复用。
type QQSendPrivateMsgPayload struct {
    ReceiverID      string      `json:"receiver_id"`      // 接收者 QQ 号，字符串避免大数精度问题
    MessageSegments []QQSegment `json:"message_segments"` // 消息段数组，保持顺序
}

// QQSegment 入站、出站共用同一份定义。
// Data 使用 json.RawMessage 以容纳未来可能出现的非字符串字段
// （例如某些实现 image 带 width/height 数字字段），
// 现阶段 OneBot v11 的 data 实际均为字符串，Adapter 在转换时按需 unmarshal。
type QQSegment struct {
    Type string                     `json:"type"`
    Data map[string]json.RawMessage `json:"data"`
}
// 常用消息段构造器。Backend 下发 command 时优先用这些，避免手写 map。
func NewTextSegment(text string) QQSegment {
    raw, _ := json.Marshal(text)
    return QQSegment{Type: "text", Data: map[string]json.RawMessage{"text": raw}}
}

func NewAtSegment(qq string) QQSegment {
    raw, _ := json.Marshal(qq)
    return QQSegment{Type: "at", Data: map[string]json.RawMessage{"qq": raw}}
}

func NewPrivateTextPayload(receiverID, text string) QQSendPrivateMsgPayload {
    return QQSendPrivateMsgPayload{
        ReceiverID:      receiverID,
        MessageSegments: []QQSegment{NewTextSegment(text)},
    }
}
```

### result.qq.send_private_message

#### 消息结构定义

该消息使用统一的 Envelope：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `version` | `string` | 是 | 协议版本，当前为 `v1` |
| `type` | `string` | 是 | 固定为 `result.qq.send_private_message` |
| `trace_id` | `string` | 是 | 原始出站指令的链路追踪标识，与对应 `command.qq.send_private_message` 的 `trace_id` 完全一致 |
| `payload` | `object` | 是 | 指令执行结果，见下 |

`payload` 字段定义如下：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `command_id` | `string` | 是 | 对应出站指令的唯一标识，由 Backend 生成，Adapter 原样返回 |
| `status` | `string` | 是 | 指令执行结果：`success` 或 `failed` |
| `platform_message_id` | `string` | 否 | 平台返回的消息 ID，仅在成功且能够获取时提供 |
| `error_code` | `object` | 否 | 错误信息，仅在 `status` 为 `failed` 时提供 |
| `error_message` | `object` | 否 | 错误信息，仅在 `status` 为 `failed` 时提供 |


错误码由协议双方统一约定。`message` 用于辅助排查，不应作为 Backend 判断错误类型的依据。

#### 示例一：发送成功

向 QQ 号为 `123456` 的用户发送私聊消息，平台返回消息 ID 为 `123456789`。

```json
{
  "version": "v1",
  "type": "result.qq.send_private_message",
  "trace_id": "019a1234-5678-7abc-8def-0123456789ab",
  "payload": {
    "command_id": "019a5678-1234-7abc-8def-0123456789ab",
    "status": "success",
    "platform_message_id": "123456789"
  }
}
```

#### 示例二：发送失败

调用 OneBot API 时发生错误，Adapter 无法确认消息发送成功。

```json
{
  "version": "v1",
  "type": "result.qq.send_private_message",
  "trace_id": "019a1234-5678-7abc-8def-0123456789ab",
  "payload": {
    "command_id": "019a5678-1234-7abc-8def-0123456789ab",
    "status": "failed",
    "error_code": "QQ_SEND_API_ERROR",
    "error_message": "OneBot API returned an error",
  }
}

```

以上错误码及消息均为示例，实际使用时应遵循统一错误码约定。

#### 补充说明

- `type` 用于标识结果类型，与原始指令 `command.qq.send_private_message` 严格区分。
- `trace_id` 由 Backend 生成，Adapter 必须原样返回，不得重新生成或修改。
- `command_id` 由 Backend 为每条出站指令生成唯一标识，Adapter 必须原样返回，用于精确关联指令与执行结果。
- `command_type` 必须与原始指令的 `type` 一致。
- `status` 为 `success` 时，表示 Adapter 已获得平台 API 的成功响应，不代表接收者已阅读消息。
- `status` 为 `failed` 时，必须提供 `error`；`status` 为 `success` 时，应省略 `error`。
- `platform_message_id` 仅在平台成功返回且能够获取消息 ID 时提供，不得由 Adapter 自行生成。
- `retryable` 表示错误是否具备重试条件，不代表 Backend 必须自动重试。对于超时、连接中断等无法确定实际发送结果的情况，应谨慎重试，以避免重复发送。
- Backend 必须处理结果超时、重复结果、迟到结果以及无法关联到待处理指令的结果。
- Adapter 应尽可能为每条合法指令返回明确的执行结果。若结果无法返回，Backend 不得直接将结果缺失视为发送失败。
- 该结果仅描述出站指令的执行情况，不代表对应业务操作的最终状态。例如，绑定码创建成功但私聊发送失败时，绑定码是否保留、失效或允许重新获取，由绑定业务规则决定。

#### Go 数据结构建议

QQ Adapter 可以使用以下 DTO 表示标准化后的执行结果。该 DTO 仅用于协议数据，不依赖 ZeroBot 的运行时 `Ctx`。

```go
package protocol

// QQSendPrivateMessageResultPayload 表示 Backend 下发的私聊发送指令的执行结果。
type QQSendPrivateMessageResultPayload struct {
    CommandID         string        `json:"command_id"`
    Status            string        `json:"status"`
    PlatformMessageID string        `json:"platform_message_id,omitempty"`
    ErrorCode         string        `json:"error_code,omitempty"`
    ErrorMessage      string        `json:"error_message,omitempty"`
}
```

建议使用常量统一管理消息类型和执行状态：

```go
package protocol

const (
    TypeQQSendPrivateMessage       = "command.qq.send_private_message"
    TypeQQSendPrivateMessageResult = "result.qq.send_private_message"

    CommandStatusSuccess = "success"
    CommandStatusFailed  = "failed"
)
```

外层 Envelope 继续复用统一协议中已有的定义，不需要为该结果单独创建新的 Envelope 结构。

同时，原有的 `command.qq.send_private_message` 应增加必填字段 `command_id`，由 Backend 生成并随指令下发，以保证出站指令与执行结果之间能够建立明确的关联关系。

---

## 4. 统一错误码约定（建议）

`result.*` 消息的 `error_code` 使用下方统一错误码，由协议双方共享，不随具体业务自由命名。`error_message` 仅用于辅助排查，Backend 不得仅凭 `error_message` 判断分支。

| error_code | 含义 | 典型触发 |
| --- | --- | --- |
| `QQ_ALREADY_BOUND` | 该 QQ 已存在有效绑定 | BIZ-001 生成绑定码时 qq_uin 已有 active `qq_bindings` |
| `RATE_LIMITED` | 触发限流 | 命中 Redis 限流计数 |
| `INTERNAL_ERROR` | 服务端内部错误（不含明细） | 绑定码碰撞重试耗尽、数据库写入失败等 |
| `USER_ALREADY_BOUND` | 该账号已存在有效绑定 | BIZ-006 赎回绑定码时 user_id 已有 active `qq_bindings` |
| `NOT_BOUND` | 当前无有效绑定 | BIZ-007 解绑时该账号无 active `qq_bindings` |
| `BIND_CODE_INVALID` | 绑定码无效 | BIZ-006 绑定码不存在 / 已使用 / 已过期 |
| `INVALID_PARAM` | 请求参数校验失败 | 绑定码长度不为 8 等 |
| `INVALID_CREDENTIALS` | 凭证错误（用户名/密码，统一） | 登录用户名不存在或密码错误（防枚举） |
| `ACCOUNT_DISABLED` | 账号被禁用（status != 1） | 登录 / 绑定被禁用账号 |
| `NOT_AUTHENTICATED` | 未认证 / 会话失效 | 未携带有效会话 Cookie 或会话已撤销/过期 |
| `USERNAME_TAKEN` | 用户名已被占用 | 注册时用户名唯一约束冲突 |
| `SERVER_CODE_INVALID` | 服务器标识格式非法 | Web WS 订阅路径中的 `server_code` 不符合 `^[a-z0-9-]+$` 或长度超限 |
| `SERVER_NOT_FOUND` | 服务器不存在 | Web WS 订阅的 `server_code` 查询无记录 |
| `SERVER_DISABLED` | 服务器已停用 | Web WS 订阅的服务器 `status != 1` |

> **单一映射源（single source of truth）**：上表的 `error_code` 字符串由 `backend/internal/biz/errors.go` 的 `errorMeta[...].WSCode` 统一定义，并通过 `biz.ResolveErrorMeta(err).WSCode` 在 WS 出口（如 `transport/handler.go` 生成绑定码失败的回复）自动推导；`protocol/qq.go` 中 `ErrorCodeXxx` 常量是其在消息层面对应的引用，二者必须保持一致。后续新增业务时请先在 `biz/errors.go` 的 `errorMeta` 登记，再同步本表，避免漂移。
>
> **错误链约定**：biz 层返回业务错误时必须用 `biz.Wrap(sentinel, cause, "上下文")` 携带底层根因，**绝不允许裸返回哨兵吞掉原因**；根因不对外暴露，但会通过 `zap.Error` 完整输出到内部日志，便于排障。


