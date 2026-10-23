/**
 * 游戏事件前端数据层：统一 HTTP 历史查询与 WebSocket 实时推送的事件身份。
 *
 * 契约（与后端 `GET /api/v1/game-servers/{server_code}/events` 及 `event.game.*` 推送一致）：
 * - `id`：MongoDB `_id` 的 24 位十六进制串，**唯一**去重键。不得用 `received_at` 或 `trace_id` 去重
 *   （同一毫秒可能有多条事件，`trace_id` 只是链路追踪标识）。
 * - `cursor`：不透明分页游标，由 `(received_at, _id)` 确定性生成。前端**只保存与回传**，不解析内部结构。
 * - 展示顺序：`received_at ASC, id ASC`，即后端的 `received_at ASC, _id ASC`
 *   （ObjectId 的十六进制表示保持字节序，字典序与 BSON 里的二进制比较等价）。
 *
 * 本模块不含任何框架依赖，便于单独测试，也便于在任意视图层复用。
 */

/** 事件内容：join/quit 为空对象，chat/command 按类型携带字段（零值字段后端省略）。 */
export interface GameEventData {
  message?: string
  command?: string
  message_id?: string
  raw_message?: string
}

export interface GameEventPlayer {
  uuid: string
  nickname: string
  is_op: boolean
}

/** 一条游戏事件：HTTP `items[]` 与 WebSocket `payload` 结构同构。 */
export interface GameEvent {
  /** MongoDB `_id`（24 位十六进制），事件唯一身份与去重键 */
  id: string
  /** 分页游标，不透明字符串，仅保存与回传 */
  cursor: string
  event_type: string
  server_id: number
  /** 仅 WebSocket 推送携带（HTTP 以路径参数标识服务器） */
  server_code?: string
  /** 仅 HTTP 查询携带 */
  server_name?: string
  player: GameEventPlayer
  occurred_at: string
  received_at: string
  data: GameEventData
}

/** HTTP 分页响应中的 `data` 部分。 */
export interface GameEventPage {
  items: GameEvent[]
  oldest_cursor: string | null
  newest_cursor: string | null
  next_cursor: string | null
  has_more: boolean
}

/**
 * 事件全序比较：`received_at ASC, id ASC`。
 *
 * `received_at` 是固定宽度的 UTC RFC3339 毫秒串（如 `2026-10-22T15:09:34.401Z`），
 * 长度恒定 → 字符串字典序等价于时间序，无需 new Date() 解析（也避免时区与精度误差）。
 */
export function compareEvents(a: GameEvent, b: GameEvent): number {
  if (a.received_at !== b.received_at) {
    return a.received_at < b.received_at ? -1 : 1
  }
  if (a.id !== b.id) {
    return a.id < b.id ? -1 : 1
  }
  return 0
}

/** 返回按 `received_at ASC, id ASC` 排序的新数组（不修改入参）。 */
export function sortEvents(items: readonly GameEvent[]): GameEvent[] {
  return [...items].sort(compareEvents)
}

/**
 * 按 `id` 去重合并两组事件，并保持全序。
 *
 * 同一条事件可能同时来自 HTTP（历史/补齐）与 WebSocket（实时推送），两者都是**同一条持久化记录**
 * 的投影，字段完全一致，因此保留先到的那一份即可；重复的 `id` 一律丢弃。
 */
export function mergeEvents(existing: readonly GameEvent[], incoming: readonly GameEvent[]): GameEvent[] {
  const byId = new Map<string, GameEvent>()
  for (const ev of existing) {
    if (!byId.has(ev.id)) byId.set(ev.id, ev)
  }
  for (const ev of incoming) {
    if (!byId.has(ev.id)) byId.set(ev.id, ev)
  }
  return sortEvents([...byId.values()])
}

/**
 * 事件流：维护列表、去重集合与两个游标位置。
 *
 * 游标用途（不要混用方向）：
 * - `historyCursor` → 上滑加载更早历史：`?before={historyCursor}`（即本页 `oldest_cursor`）；
 * - `resyncCursor`  → 断线补齐/增量：`?after={resyncCursor}`（即本页 `newest_cursor`）。
 *
 * 两个游标都由「当前列表」推导，而不是照抄上一次响应：补齐期间 HTTP 与 WS 会重叠，
 * 只有按去重后的实际边界推导，才能避免把已处理位置回退到重复事件上。
 */
export class EventFeed {
  items: GameEvent[] = []
  /** 加载更早历史时使用的游标：`before + historyCursor`；空列表为 null。 */
  historyCursor: string | null = null
  /** 断线补齐时使用的游标：`after + resyncCursor`；空列表为 null。 */
  resyncCursor: string | null = null
  /** 更早方向是否还有数据（来自 `has_more`）。 */
  hasMoreHistory = false
  /** 更新方向是否还有数据（来自 after 查询的 `has_more`）。 */
  hasMoreUpdates = false

  private readonly ids = new Set<string>()

  /**
   * 首屏加载（默认模式，取最新 N 条）。
   *
   * 采用「合并」而非「替换」：WebSocket 可能先于 HTTP 首屏到达，替换会把已收到的实时事件丢掉；
   * 合并按 `id` 去重，首屏重复的事件不会产生重复项。
   */
  loadInitial(page: GameEventPage): void {
    this.apply(page.items)
    this.hasMoreHistory = page.has_more
  }

  /** 上滑加载更早历史：把 `before` 查询结果并入列表（幂等，可安全重试）。 */
  prependHistory(page: GameEventPage): void {
    this.apply(page.items)
    this.hasMoreHistory = page.has_more
  }

  /** 断线补齐后的增量结果：把 `after` 查询结果并入列表（幂等，可安全重试）。 */
  appendUpdates(page: GameEventPage): void {
    this.apply(page.items)
    this.hasMoreUpdates = page.has_more
  }

  /** 处理一条 WebSocket 实时事件；返回是否为新事件（false 表示已存在、被去重丢弃）。 */
  ingest(event: GameEvent): boolean {
    if (this.ids.has(event.id)) return false
    this.apply([event])
    return true
  }

  /** 是否已包含某条事件（按 `id`，不是 `received_at`）。 */
  has(id: string): boolean {
    return this.ids.has(id)
  }

  private apply(incoming: readonly GameEvent[]): void {
    const merged = mergeEvents(this.items, incoming)
    this.items = merged
    this.ids.clear()
    for (const ev of merged) this.ids.add(ev.id)

    // 边界游标由合并后的实际内容推导（不用响应里的 oldest/newest，避免重叠时位置回退）。
    this.historyCursor = merged.length > 0 ? merged[0].cursor : null
    this.resyncCursor = merged.length > 0 ? merged[merged.length - 1].cursor : null
  }
}
