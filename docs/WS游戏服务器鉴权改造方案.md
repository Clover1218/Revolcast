# WS 游戏服务器接入鉴权改造方案

**状态：** 已实施（代码 / 文档 / 数据库索引均已落地）
**目标路由：** `/api/v1/game-servers/ws`（原 `/api/v1/server/ws` 已重命名，clientType 仍为 `mc_server`）
**关联文档：** 《HTTP 接口设计文档》§12.9、《WebSocket 消息结构设计文档》「WS 连接与接入端身份」、《数据库表设计文档》§3.4

---

## 1. 背景与目标

改造前 `/api/v1/server/ws` 沿用共享密钥：客户端在 query 里带 `token`（`cfg.WS.Token`）和自声明的 `client_id`，服务端仅比对共享 token，clientID 完全由客户端自报。

问题：
- 所有 MC 服务器共用一个密钥，无法区分接入的是哪台服务器，也无法按服务器维度做权限/查询。
- clientID 自报，存在冒用与重复风险（虽有 Hub 单连接约束，但身份不权威）。

目标（按需求）：
- 客户端在 query 带 `token`，其值为「创建/轮换服务器时一次性下发的原始 API Secret」。
- 服务端对其做 `SHA256(token)`，与 `game_servers.api_secret_hash` 比对；命中则将 `clientID` 设为 `game_servers.id`，供后续按服务器身份路由/查询。

这与《HTTP 接口设计文档》§12.9「WS 鉴权升级」方向一致，但实现上采用**按 secret 哈希直接查表**（而非先按 `client_id`=code 查再比 secret），身份从 secret 派生，更简洁、无需客户端自报 `client_id`。

---

## 2. 现状（改造前）

`backend/internal/transport/ws_server.go` 的 `ServeWS`：
- `token := r.URL.Query().Get("token")` 与 `cfg.WS.Token` 比对；
- `clientID := r.URL.Query().Get("client_id")` 自报；
- 升级 → 注册 `Client{ClientID: clientID, ClientType: clientType}`。

`game_servers.api_secret_hash` 的语义（见 `biz/game_server_service.go` 的 `defaultServerSecretGenerator`）：
- 创建/轮换时生成 32 字节随机 `raw`（hex 64 字符），`api_secret_hash = hex(SHA256(raw))`；
- 库仅存哈希，原始 `raw` 仅一次性下发。

因此「`SHA256(query.token) == api_secret_hash`」与现有密钥表完全对齐，**无需任何数据库结构变更**（仅需一个加速/唯一性索引，见 §6）。

---

## 3. 目标设计（新连接逻辑）

新增函数 `ServeGameServerWS(hub, auth, baseLogger)`，专用于 `/api/v1/game-servers/ws`；原有 `ServeWS`（共享 token + 自报 client_id）保留给 `/api/v1/adapter/ws`。

### 3.1 鉴权流程

1. 取 `token := r.URL.Query().Get("token")`。
   - 空 → `401 Unauthorized`（不暴露原因）。
2. 调用 `auth.ResolveServerBySecret(ctx, token)`：
   - 内部 `hash = hex(SHA256(token))`；
   - `repo.GetGameServerBySecretHash(ctx, hash)` → `SELECT ... WHERE api_secret_hash = $1 AND status = 1`；
   - 命中返回 `rec.ID`；未命中（secret 错误 / 服务器停用 / 不存在）统一返回 `ErrServerNotFound`。
3. 鉴权失败（`err != nil`）→ 记 `Warn` 日志（含 `remote_ip` 与错误链，不含 token/secret）并 `http.Error(w, "Unauthorized", 401)`。
   - **不区分 secret 错误 vs 服务器停用 vs 不存在**，避免枚举/信息泄露（与登录接口防枚举一致）。
4. `clientID = strconv.FormatInt(rec.ID, 10)`（即 `game_servers.id`）。
5. 升级协议 → 创建 `Client{ClientID: clientID, ClientType: "mc_server"}` → 注册 Hub、启动 read/write pump。

### 3.2 身份与路由语义

- `clientID` 由 secret 派生且等于服务器主键 `id`，权威、不可冒用。
- Hub 的「单身份单连接」约束让同一服务器重连时自动踢掉旧连接；不同服务器 id 互不干扰。
- 入站事件 `task.ClientID` 与回传 `SendToClient(clientID)` 均以服务器 id 寻址，便于后续按服务器维度路由指令/查询。
- `client_id` query 参数对 `/api/v1/game-servers/ws` **不再需要**（被忽略/不接受）；若客户端仍带，直接忽略。

### 3.3 依赖注入（业务逻辑留在 biz，transport 不依赖 WS 运行时）

- `biz.GameServerService` 新增方法：
  ```go
  // ResolveServerBySecret 用 MC 服务器持有的原始 API Secret 解析出其 game_servers.id。
  // 要求 status=1；未命中（secret 错误/停用/不存在）返回 ErrServerNotFound，DB 错误包裹为 ErrInternal。
  func (s *GameServerService) ResolveServerBySecret(ctx context.Context, rawSecret string) (int64, error)
  ```
- `repository.GameServerRepo` 新增：
  ```go
  // GetGameServerBySecretHash 按 api_secret_hash 查启用中的服务器；不存在/停用返回 ErrServerNotFound。
  func (r *GameServerRepo) GetGameServerBySecretHash(ctx context.Context, secretHash string) (*GameServerRecord, error)
  ```
  并登记进 `biz.GameServerStore` 接口，便于单测用 fake 注入。
- transport 定义最小接口，避免依赖整个 service：
  ```go
  type GameServerAuth interface {
      ResolveServerBySecret(ctx context.Context, rawSecret string) (int64, error)
  }
  ```
  `*biz.GameServerService` 天然满足。
- 新增 `serveWSConnection(hub, w, r, clientID, clientType, baseLogger)` 私有助手，承载「升级 + 建 Client + 注册 + 起 pump」，供 `ServeWS` 与 `ServeGameServerWS` 共用。

### 3.4 main.go 接线

```go
// 游戏服务器业务：WS 接入鉴权与 HTTP 管理接口共用同一 service。
gameServerRepo := repository.NewGameServerRepo(PostgreSQL)
gameServerSvc := biz.NewGameServerService(gameServerRepo, redisRepo, biz.GameServerConfig{})

mux.HandleFunc("/api/v1/adapter/ws", transport.ServeWS(hub, &cfg.WS, baseLogger, "adapter"))
mux.HandleFunc("/api/v1/game-servers/ws", transport.ServeGameServerWS(hub, gameServerSvc, baseLogger))
```
`cfg.WS.Token` 仅剩 `/api/v1/adapter/ws` 使用（保留，不删除）。`gameServerSvc` 的创建必须早于 mux 注册。

> 路由优先级：Go 1.22+ `ServeMux` 中 `/api/v1/game-servers/ws`（字面量）优先于 `/api/v1/game-servers/{code}`（通配），已用临时程序实测：`/ws` 命中 WS 处理器、`/abc` 命中 `{code}`，无冲突 panic。

---

## 4. 关键设计决策（已确认）

1. **路由路径**：由 `/api/v1/server/ws` 重命名为 **`/api/v1/game-servers/ws`**。
   - 决策：采用语义化路径，与 HTTP 管理接口同前缀。**MC 端连接地址需同步修改**（旧路径不再注册，直接 404）。
2. **停用服务器（status=0）**：**拒绝接入**，与 secret 错误/不存在统一 401，不暴露「存在但停用」。
3. **`client_id` 参数**：新方案由 secret 派生身份，客户端无需也无法自报；该参数被忽略。
4. **唯一索引**：**本次执行**（见 §6），保证一密钥唯一对应一台服务器并加速查表。
5. **Secret 在 query 中泄露风险**：与现有共享 token 一致，仍建议生产用 `wss` 且网关不记录 query 串；属既有约定，本期不引入 header/subprotocol 改造。
6. **停用/轮换后的在线连接**：不做即时断开，下次重连时由鉴权拦截（如需强制下线，后续在 `RotateSecret` / `Disable` 中按 `clientID=id` 主动 `closeConn`）。

---

## 5. 改动清单（实际落地）

| 文件 | 改动 |
| --- | --- |
| `backend/internal/repository/game_server.go` | 新增 `GetGameServerBySecretHash`（`WHERE api_secret_hash = $1 AND status = 1`，未命中 `ErrServerNotFound`） |
| `backend/internal/biz/game_server_service.go` | 新增 `ResolveServerBySecret`；`GameServerStore` 接口补该方法 |
| `backend/internal/biz/game_server_service_test.go` | fake store 补方法 + `ResolveServerBySecret` 单测（成功 / NotFound / DB 错误） |
| `backend/internal/transport/ws_server.go` | 新增 `GameServerAuth` 接口、`ServeGameServerWS`、共用助手 `serveWSConnection` |
| `backend/internal/transport/ws_server_test.go` | 新增 `ServeGameServerWS` 用例（成功 101 + clientID=id / 缺 token / 鉴权失败 / 停用） |
| `backend/internal/transport/http_game_server_test.go` | fake 补 `GetGameServerBySecretHash` 以满足接口 |
| `backend/cmd/server/main.go` | `gameServerSvc` 提前创建；`/api/v1/game-servers/ws` 接 `ServeGameServerWS`，移除 `/api/v1/server/ws` |
| `docs/Websocket消息结构设计文档.md` | 「WS 连接与接入端身份」整节改写（路由表、参数表、game-servers 鉴权流程） |
| `docs/Revolcast_HTTP接口设计文档.md` | §12.9 由「待升级」改为「已落地」，同步新路径与新机制 |

---

## 6. 数据库索引（已执行）

为「一密钥对应唯一服务器」与加速查表，补唯一索引（改造前仅 `code` 有唯一索引）：

```sql
CREATE UNIQUE INDEX IF NOT EXISTS ux_game_servers_api_secret_hash
    ON public.game_servers USING btree (api_secret_hash);
```

执行前置校验（均通过）：`game_servers` 上无同名索引；`GROUP BY api_secret_hash HAVING count(*) > 1` 无重复行。
执行后确认索引已生效。

> secret 由 256-bit 随机值派生，实际碰撞概率可忽略，索引主要为查表性能与唯一约束兜底。

---

## 7. 测试与验证

- 单测（transport）：fake `GameServerAuth` 返回固定 id / `ErrServerNotFound` / 内部错误，断言握手 101、401、401。
- 单测（biz）：`ResolveServerBySecret` 对「正确 secret → 返回 id」「错误 secret → ErrServerNotFound」「停用服务器 → ErrServerNotFound（不泄露）」「DB 错误 → ErrInternal（带 Wrap 根因）」覆盖。
- 全量：`gofmt -l .` 无输出、`go build ./...`、`go vet ./...`、`go test ./...` 全绿。
- 未做连库集成测试（握手 101 需实际 DB 中存在启用服务器与其 secret）。

---

## 8. 对端（MC 模组/服务端插件）接入要点

```text
ws://<host>/api/v1/game-servers/ws?token=<创建或轮换时下发的原始 API Secret>
```
- 不再需要 `client_id`；服务器身份由服务端根据 secret 判定。
- secret 错误 / 服务器被停用 → 握手阶段直接 `401`，客户端应视为配置错误并停止重试风暴（建议退避）。
- 轮换 secret 后旧 secret 立即失效，需用新 secret 重连。

---

## 9. 后续调整：连接索引键改为 `server_code`（WebSocket 抽象改造同步）

后续「WebSocket Client 抽象 + 前端事件推送」改造对本文方案做了两处调整，其余鉴权流程不变：

1. **Hub 连接索引键由 `game_servers.id` 改为 `server_code`**。
   原因：三类接入端需要各自稳定的身份键，且 Web 端按 `server_code` 订阅；同一台服务器的重连仍表现为「同键重复连接 → 踢旧留新」。
2. **业务归属仍使用 `game_servers.id`**。
   `task.ClientID`（事件落库时的服务器身份）保持为 `id` 的十进制字符串，与事件存储/查询口径一致；
   连接索引键（`server_code`）与业务身份（`server_id`）不可混用。

接口签名相应调整为：

```go
// ResolveServerBySecret 返回 id 与 code：id 用于业务归属，code 用于连接索引。
func (s *GameServerService) ResolveServerBySecret(ctx context.Context, rawSecret string) (serverID int64, serverCode string, err error)
```

- `ServeGameServerWS` 的鉴权顺序、统一 `401` 策略、停用服务器不可接入等规则**保持原样**。
- `SendToClient(clientID)` 的查找顺序为 adapter 的 `client_id` → `server_code`，Adapter 指令回传语义不变。
