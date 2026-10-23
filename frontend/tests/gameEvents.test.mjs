import test from 'node:test'
import assert from 'node:assert/strict'

import { compareEvents, mergeEvents, sortEvents, EventFeed } from '../src/events/gameEvents.ts'

// 构造与后端 items[] / WS payload 同构的事件（id 用 24 位十六进制便于与真实值对齐）。
function ev(id, receivedAt, extra = {}) {
  return {
    id,
    cursor: 'cursor-' + id,
    event_type: 'player_chat',
    server_id: 10,
    player: { uuid: 'a3b91ae7', nickname: 'Clover', is_op: false },
    occurred_at: '2026-10-22T15:09:34.000Z',
    received_at: receivedAt,
    data: { message: 'hi' },
    ...extra,
  }
}

function page(items, hasMore = false) {
  const oldest = items.length > 0 ? items[0].cursor : null
  const newest = items.length > 0 ? items[items.length - 1].cursor : null
  return {
    items,
    oldest_cursor: oldest,
    newest_cursor: newest,
    next_cursor: oldest,
    has_more: hasMore,
  }
}

const A = '65000000000000000000000a'
const B = '65000000000000000000000b'
const C = '65000000000000000000000c'

// 同一毫秒内两条事件：顺序由 id 决定（对应后端 received_at ASC, _id ASC）。
test('compareEvents 先按 received_at、再按 id 排序', () => {
  const t = '2026-10-22T15:09:34.401Z'
  assert.equal(compareEvents(ev(A, t), ev(B, t)), -1)
  assert.equal(compareEvents(ev(B, t), ev(A, t)), 1)
  assert.equal(compareEvents(ev(A, t), ev(A, t)), 0)
  // 时间更早者永远在前，即使 id 更大。
  assert.equal(compareEvents(ev(B, '2026-10-22T15:09:34.400Z'), ev(A, t)), -1)
})

test('sortEvents 同毫秒事件不定序输入也得到稳定升序', () => {
  const t = '2026-10-22T15:09:34.401Z'
  const sorted = sortEvents([ev(B, t), ev(A, t), ev(C, t)])
  assert.deepEqual(
    sorted.map((e) => e.id),
    [A, B, C],
  )
})

// HTTP 与 WebSocket 重叠：同一个 id 只保留一条。
test('mergeEvents 按 id 去重（HTTP 与 WS 重叠）', () => {
  const t = '2026-10-22T15:09:34.401Z'
  const fromHttp = [ev(A, t), ev(B, t)]
  const fromWs = [ev(B, t), ev(C, t)]

  const merged = mergeEvents(fromHttp, fromWs)
  assert.deepEqual(
    merged.map((e) => e.id),
    [A, B, C],
  )

  // 不能按 received_at 去重：同毫秒的两条不同事件必须都保留。
  const sameMs = mergeEvents([ev(A, t)], [ev(B, t)])
  assert.equal(sameMs.length, 2)

  // 也不能按 trace_id 去重（trace_id 不在事件里，仅用于链路追踪）。
  assert.equal(mergeEvents([], []).length, 0)
})

// 首次加载：取最新一页，游标指向两端。
test('EventFeed.loadInitial 建立列表与两个游标', () => {
  const feed = new EventFeed()
  const t = '2026-10-22T15:09:34.401Z'
  feed.loadInitial(page([ev(A, t), ev(B, t)], true))

  assert.equal(feed.items.length, 2)
  assert.equal(feed.historyCursor, 'cursor-' + A) // before + oldest
  assert.equal(feed.resyncCursor, 'cursor-' + B) // after + newest
  assert.equal(feed.hasMoreHistory, true)
})

// 历史加载：before + oldest_cursor，结果前置；重叠项不重复。
test('EventFeed.prependHistory 追加更早历史且按 id 去重', () => {
  const feed = new EventFeed()
  feed.loadInitial(page([ev(B, '2026-10-22T15:09:34.402Z')]))

  // 更早一页与当前最早一条重叠（B 同时出现在两页边界）。
  feed.prependHistory(
    page([ev(A, '2026-10-22T15:09:34.401Z'), ev(B, '2026-10-22T15:09:34.402Z')], false),
  )

  assert.deepEqual(
    feed.items.map((e) => e.id),
    [A, B],
  )
  // 位置回退到真正的最早一条；两端游标一致指向正确方向。
  assert.equal(feed.historyCursor, 'cursor-' + A)
  assert.equal(feed.resyncCursor, 'cursor-' + B)
  assert.equal(feed.hasMoreHistory, false)
})

// 断线补齐：after + newest_cursor，与 WS 重叠时去重，顺序仍为升序。
test('EventFeed.appendUpdates 与实时推送重叠时按 id 去重', () => {
  const feed = new EventFeed()
  feed.loadInitial(page([ev(A, '2026-10-22T15:09:34.401Z')]))

  // 实时推送先到 C（此时列表 A、C 之间有缺口 B）。
  assert.equal(feed.ingest(ev(C, '2026-10-22T15:09:34.403Z')), true)
  // 断线补齐把 B、C 一起补回来（C 与实时推送重叠）。
  feed.appendUpdates(
    page([ev(B, '2026-10-22T15:09:34.402Z'), ev(C, '2026-10-22T15:09:34.403Z')], false),
  )

  assert.deepEqual(
    feed.items.map((e) => e.id),
    [A, B, C],
  )
  assert.equal(feed.items.length, 3) // C 未被重复加入
  assert.equal(feed.resyncCursor, 'cursor-' + C)
})

// 实时事件乱序到达：按 (received_at, id) 插入正确位置；重复 id 直接丢弃。
test('EventFeed.ingest 处理乱序与重复', () => {
  const feed = new EventFeed()
  feed.loadInitial(page([ev(B, '2026-10-22T15:09:34.402Z')]))

  // 后到的更早事件（例如上行乱序）应插到前面。
  assert.equal(feed.ingest(ev(A, '2026-10-22T15:09:34.401Z')), true)
  // 同一条事件的重复推送（断线重连、HTTP 已补齐过）必须被丢弃。
  assert.equal(feed.ingest(ev(A, '2026-10-22T15:09:34.401Z')), false)
  assert.equal(feed.ingest(ev(B, '2026-10-22T15:09:34.402Z')), false)

  assert.deepEqual(
    feed.items.map((e) => e.id),
    [A, B],
  )
  assert.equal(feed.has(A), true)
  assert.equal(feed.has(C), false)
})

// 同一毫秒的多条事件必须都能进入列表（不能用 received_at 去重）。
test('EventFeed 同一毫秒多条事件互不覆盖', () => {
  const feed = new EventFeed()
  const t = '2026-10-22T15:09:34.401Z'
  feed.loadInitial(page([ev(A, t)]))

  assert.equal(feed.ingest(ev(B, t)), true)
  assert.equal(feed.ingest(ev(C, t)), true)
  assert.deepEqual(
    feed.items.map((e) => e.id),
    [A, B, C],
  )
})

// 与 WebSocket 推送同结构：server_code 存在也不影响去重与排序。
test('mergeEvents 兼容 WS payload（含 server_code）', () => {
  const t = '2026-10-22T15:09:34.401Z'
  const fromWs = ev(A, t, { server_code: 'survival' })
  const merged = mergeEvents([ev(A, t)], [fromWs])
  assert.equal(merged.length, 1)
  assert.equal(merged[0].id, A)
})
