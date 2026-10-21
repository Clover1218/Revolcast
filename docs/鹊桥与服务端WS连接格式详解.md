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
  "message": "21",
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
  "raw_message": "{\"text\":\"21\"}",
  "event_name": "PlayerChatEvent",
  "post_type": "message",
  "sub_type": "player_chat",
  "timestamp": 1792394977,
  "server_name": "TestServer",
  "server_version": "1.20.1",
  "server_type": "forge"
}
```
### PlayerCommandEvent
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

### 服务器状态
+ favicon 字段为服务器图标，格式为 data:image/png;base64,... 的 Base64 编码字符串，仅当设置了服务器图标时才会返回。
#### 请求体
示例：
```json
{
  "api": "get_status",
  "echo": "1"
}
```
#### 响应体
```json
{
  "api": "get_status",
  "code": 200,
  "post_type": "response",
  "status": "SUCCESS",
  "message": "success",
  "data": {
    "timestamp": 1792400686470,
    "server_type": "forge",
    "server_version": "1.20.1",
    "server_list_ping": {
      "available": true,
      "host": "127.0.0.1",
      "port": 25565,
      "reason": "ok",
      "error": null,
      "forgeData": {
        "channels": [],
        "mods": [],
        "truncated": false,
        "fmlNetworkVersion": 3,
        "d": "ë\u0000\u0000ࠊ㐤獋㙖⹌ᦘ̺⸱恤䒸⡑⛧沮婙㨹牥ఈㄵচ₀沮婙㨹牥ఈㄵচ偀泀岛㊳ఀ勨䦕᫻⛷⺎定Ƴ⸱`䰔掃䚖⁮䮌ई僆内歋瘖䲬ౄᤗ㐮扖䢸熁㌒泅岛㊳昉壒綕଒㘶⁭஌昌壒綕䌚昖ⷍ嬙ᢁ〮 ▘ᮓ睇ⷋ崙㞻歲峒ඝ熈ጂʠ寙㜻潬䣂᥽捋癖౫ᡚ㜷汥戆䂸\tၰ⺮屙ゴկ屠㣔Ɓ႐౬婝㜷楬ૄ㣀燉̲"
      },
      "description": {
        "text": "A Minecraft Server"
      },
      "players": {
        "max": 20,
        "online": 0
      },
      "version": {
        "name": "1.20.1",
        "protocol": 763
      }
    },
    "cpu_information": {
      "cpu_cores": 32,
      "load_average": -1,
      "system_load": 0,
      "process_load": -1
    },
    "memory_information": {
      "physical_memory": {
        "total": 16779849728,
        "free": 3478147072,
        "used": 13301702656,
        "percentage": 79.27
      },
      "jvm_memory": {
        "total": 1161822208,
        "free": 510322464,
        "max": 4196401152,
        "used": 651499744,
        "percentage": 15.53
      }
    }
  },
  "echo": "1"
}
```