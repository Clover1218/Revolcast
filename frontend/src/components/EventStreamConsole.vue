<script setup lang="ts">
import { ref, reactive, nextTick, onUnmounted, watch } from 'vue'
import { EventFeed, type GameEvent, type GameEventPage } from '@/events/gameEvents'

/**
 * WS 网页推送测试页。
 *
 * 对接后端两个接口（契约见 HTTP 文档 §12.10 / §13）：
 * - WebSocket：GET /api/v1/game-servers/{server_code}/ws  （订阅实时事件，鉴权走 Cookie）
 * - HTTP：      GET /api/v1/game-servers/{server_code}/events （历史/增量，游标分页）
 *
 * 历史事件地址由 WS 地址自动推导：把 `ws(s)://host/.../ws` 换成 `http(s)://host/.../events`。
 *
 * 数据层直接复用 `frontend/src/events/gameEvents.ts` 的 EventFeed：
 * - 唯一去重键是事件 `id`（MongoDB _id 十六进制），绝不用 received_at / trace_id。
 * - 游标 historyCursor → before（上滑历史）；resyncCursor → after（连接间隙 / 断线补齐）。
 */

// ---- 连接地址与状态 ----
// 默认走 Vite 同源代理（localhost:5173），而非直连后端（localhost:8080）。
// 只有同源时，页面上手动设置的 Cookie 才会随 WS / HTTP 请求送达后端。
const wsUrl = ref('ws://localhost:5173/api/v1/game-servers/test/ws')
const eventsUrl = ref('')
const status = ref<'disconnected' | 'connecting' | 'connected' | 'reconnecting'>('disconnected')
const errorMessage = ref('')

// ---- 事件流 ----
const feed = new EventFeed()
const messages = ref<GameEvent[]>([])
const hasMoreHistory = ref(false)
const historyLoading = ref(false)
const dedupedCount = ref(0)

// ---- 重连 ----
const manualClose = ref(false)
const reconnectAttempts = ref(0)
const reconnectDelay = ref(1000)
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let ws: WebSocket | null = null

// ---- 运行日志 ----
const logs = reactive<{ t: string; msg: string }[]>([])
function log(msg: string) {
  const now = new Date()
  logs.push({ t: now.toLocaleTimeString(), msg })
  if (logs.length > 200) logs.shift()
}

// ---- 手动设置会话 Cookie（revolcast_session）----
// 后端 WS / 历史事件接口均靠 Cookie `revolcast_session` 鉴权。
// 浏览器不允许 JS 跨源写 Cookie，因此 Cookie 只能写到「当前页面源」；
// 要让它随请求送达后端，WS / 历史事件地址必须同源（即走本页默认代理地址，或前端由后端同源托管）。
const COOKIE_NAME = 'revolcast_session'
const cookieInput = ref('')
const cookieReveal = ref(false)
const cookieApplied = ref(false)
const currentOrigin = typeof window !== 'undefined' ? window.location.origin : ''

function readCookie(name: string): string | null {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const m = document.cookie.match(new RegExp('(?:^|; )' + escaped + '=([^;]*)'))
  return m ? decodeURIComponent(m[1]) : null
}

function applyCookie() {
  const v = cookieInput.value.trim()
  if (!v) {
    log('Cookie 值为空，未设置')
    return
  }
  // 写到当前页面源（localhost:5173）；同源代理会把 Cookie 头转发给后端。
  document.cookie = `${COOKIE_NAME}=${v}; path=/; max-age=86400; SameSite=Lax`
  cookieApplied.value = true
  log('已应用会话 Cookie（revolcast_session）')
}

function clearCookie() {
  document.cookie = `${COOKIE_NAME}=; path=/; max-age=0`
  cookieInput.value = ''
  cookieApplied.value = false
  log('已清除会话 Cookie')
}

// 初始同步：若当前源已存在该 Cookie（例如此前已应用过），则标记为已设置
cookieApplied.value = readCookie(COOKIE_NAME) !== null

// 把 EventFeed 的内部状态同步到 Vue 响应式引用
function syncState() {
  messages.value = feed.items
  hasMoreHistory.value = feed.hasMoreHistory
}

// ---- 地址推导 ----
function deriveEventsUrl(url: string): string {
  let s = url
    .replace(/^wss:\/\//i, 'https://')
    .replace(/^ws:\/\//i, 'http://')
  // 路径末尾的 /ws（可能带查询串）换成 /events
  s = s.replace(/\/ws(\?[^#]*)?$/i, '/events$1')
  return s
}

// ---- HTTP 历史/增量查询 ----
async function fetchEvents(cursor: string | null, mode: 'initial' | 'before' | 'after'): Promise<GameEventPage | null> {
  if (!eventsUrl.value) return null
  try {
    const u = new URL(eventsUrl.value)
    if (mode === 'before' && cursor) u.searchParams.set('before', cursor)
    if (mode === 'after' && cursor) u.searchParams.set('after', cursor)
    const res = await fetch(u.toString(), { credentials: 'include' })
    if (!res.ok) {
      log(`历史请求失败 HTTP ${res.status}`)
      return null
    }
    const body = (await res.json()) as { data?: GameEventPage }
    return body.data ?? null
  } catch (e) {
    log(`历史请求异常：${(e as Error).message}`)
    return null
  }
}

// ---- 滚动容器 ----
const listEl = ref<HTMLElement | null>(null)
const atBottom = ref(true)
function isAtBottom(): boolean {
  const el = listEl.value
  if (!el) return true
  return el.scrollHeight - el.scrollTop - el.clientHeight < 48
}
function scrollToBottom() {
  const el = listEl.value
  if (el) el.scrollTop = el.scrollHeight
}
function onScroll() {
  const el = listEl.value
  if (!el) return
  atBottom.value = isAtBottom()
  // 上滑接近顶部时自动加载更早历史
  if (el.scrollTop < 64 && hasMoreHistory.value && !historyLoading.value && feed.historyCursor) {
    loadBefore()
  }
}

// ---- 首屏加载（默认模式，取最新 N 条，建立基线游标）----
async function loadInitial() {
  const page = await fetchEvents(null, 'initial')
  if (!page) return
  feed.loadInitial(page)
  syncState()
  await nextTick()
  scrollToBottom()
  log(`首屏加载 ${page.items.length} 条（has_more=${page.has_more}）`)
}

// ---- 上滑加载更早历史（before）----
async function loadBefore() {
  if (!feed.historyCursor || !feed.hasMoreHistory || historyLoading.value) return
  historyLoading.value = true
  const el = listEl.value
  const prevHeight = el ? el.scrollHeight : 0
  const page = await fetchEvents(feed.historyCursor, 'before')
  historyLoading.value = false
  if (!page) return
  feed.prependHistory(page)
  syncState()
  log(`上滑加载更早 ${page.items.length} 条（has_more=${page.has_more}）`)
  // 维持视口位置：顶部插入了内容，需把滚动条下移相同高度
  await nextTick()
  if (el) el.scrollTop += el.scrollHeight - prevHeight
}

// ---- 连接 / 重连后补齐遗漏（after）----
async function afterGapFill() {
  if (!feed.resyncCursor) return
  const page = await fetchEvents(feed.resyncCursor, 'after')
  if (!page) return
  feed.appendUpdates(page)
  syncState()
  if (atBottom.value) scrollToBottom()
  if (page.items.length > 0) {
    log(`after 补齐 ${page.items.length} 条遗漏消息`)
  }
}

// ---- WebSocket 消息处理 ----
function onMessage(ev: MessageEvent) {
  let env: { type?: string; payload?: Record<string, unknown> }
  try {
    env = JSON.parse(ev.data as string)
  } catch {
    return
  }
  if (!env || typeof env.type !== 'string') return
  if (!env.type.startsWith('event.game.')) return // 仅处理游戏事件推送
  const p = (env.payload ?? {}) as Record<string, any>
  if (typeof p.id !== 'string' || typeof p.cursor !== 'string') return // 没有统一身份则丢弃，无法去重
  const event: GameEvent = {
    id: p.id,
    cursor: p.cursor,
    event_type: p.event_type,
    server_id: p.server_id,
    server_code: p.server_code,
    server_name: p.server_name,
    player: p.player,
    occurred_at: p.occurred_at,
    received_at: p.received_at,
    data: p.data ?? {},
  }
  const isNew = feed.ingest(event)
  if (isNew) {
    syncState()
    if (atBottom.value) scrollToBottom()
  } else {
    dedupedCount.value++
  }
}

// ---- 建立连接 ----
async function connect() {
  if (status.value === 'connected' || status.value === 'connecting') return
  manualClose.value = false
  errorMessage.value = ''
  // 若已填写会话 Cookie 值，连接前自动应用（确保同源代理能把 Cookie 转发给后端）
  if (cookieInput.value.trim()) applyCookie()
  eventsUrl.value = deriveEventsUrl(wsUrl.value)
  log(`历史事件地址：${eventsUrl.value}`)
  status.value = 'connecting'
  // 1) 先取基线历史（默认模式），建立游标
  await loadInitial()
  // 2) 再建立 WS
  openWs()
}

function openWs() {
  try {
    ws = new WebSocket(wsUrl.value)
  } catch (e) {
    log(`WS 创建失败：${(e as Error).message}`)
    scheduleReconnect()
    return
  }
  ws.onopen = () => {
    status.value = 'connected'
    reconnectAttempts.value = 0
    reconnectDelay.value = 1000
    log('WS 已连接')
    // 初次连接：用 after 检查连接间隙的遗漏消息（与实时推送重叠部分按 id 去重）
    afterGapFill()
  }
  ws.onmessage = onMessage
  ws.onerror = () => {
    log('WS 发生错误')
  }
  ws.onclose = (e) => {
    log(`WS 关闭（code=${e.code}）`)
    if (manualClose.value) {
      status.value = 'disconnected'
      return
    }
    scheduleReconnect()
  }
}

// ---- 断开（手动）----
function disconnect() {
  manualClose.value = true
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
  if (ws) {
    ws.onclose = null
    ws.close()
    ws = null
  }
  status.value = 'disconnected'
  log('已手动断开')
}

// ---- 指数退避自动重连 ----
function scheduleReconnect() {
  if (manualClose.value) return
  status.value = 'reconnecting'
  const delay = Math.min(1000 * 2 ** reconnectAttempts.value, 30000)
  reconnectDelay.value = delay
  reconnectAttempts.value++
  log(`将在 ${delay}ms 后重连（第 ${reconnectAttempts.value} 次）`)
  reconnectTimer = setTimeout(() => {
    if (manualClose.value) return
    status.value = 'connecting'
    openWs()
  }, delay)
}

// ---- 清空消息（仅清前端视图，不影响服务端）----
function clearMessages() {
  feed.items = []
  feed.historyCursor = null
  feed.resyncCursor = null
  feed.hasMoreHistory = false
  feed.hasMoreUpdates = false
  dedupedCount.value = 0
  syncState()
  log('已清空本地消息视图')
}

// 监听消息总数变化，保持在底部（若用户已在底部）
watch(
  () => messages.value.length,
  () => {
    if (atBottom.value) nextTick(scrollToBottom)
  }
)

onUnmounted(() => {
  manualClose.value = true
  if (reconnectTimer) clearTimeout(reconnectTimer)
  if (ws) {
    ws.onclose = null
    ws.close()
  }
})

// ---- 消息行格式化（按需求）----
function formatLine(ev: GameEvent): string {
  const time = ev.occurred_at
  const name = ev.player?.nickname ?? '?'
  switch (ev.event_type) {
    case 'player_chat':
      return `[${time}] <${name}> ${ev.data?.message ?? ''}`
    case 'player_join':
      return `[${time}] <${name}> 加入了游戏`
    case 'player_quit':
      return `[${time}] <${name}> 离开了游戏`
    default:
      return `[${time}] <${name}> (${ev.event_type})`
  }
}
function lineClass(ev: GameEvent): string {
  switch (ev.event_type) {
    case 'player_join':
      return 'text-neon'
    case 'player_quit':
      return 'text-rose-400'
    case 'player_chat':
      return 'text-neon-cyan'
    default:
      return 'text-gray-300'
  }
}
</script>

<template>
  <div class="flex h-screen flex-col bg-gray-950 text-gray-100 font-mono">
    <!-- 顶部控制栏 -->
    <header class="border-b border-gray-800 px-4 py-3 space-y-3">
      <div class="flex items-center gap-2 flex-wrap">
        <span class="text-neon font-bold tracking-widest text-lg">REVOLCAST</span>
        <span class="text-gray-500">WS 推送测试台</span>
        <span
          class="ml-auto px-2 py-1 rounded text-xs border"
          :class="{
            'border-gray-600 text-gray-400': status === 'disconnected',
            'border-amber-500 text-amber-400': status === 'connecting' || status === 'reconnecting',
            'border-neon text-neon': status === 'connected',
          }"
        >
          {{ { disconnected: '● 已断开', connecting: '○ 连接中', connected: '● 已连接', reconnecting: '○ 重连中' }[status] }}
        </span>
      </div>

      <div class="flex items-center gap-2 flex-wrap">
        <input
          v-model="wsUrl"
          :disabled="status === 'connected' || status === 'connecting'"
          type="text"
          placeholder="ws://host/api/v1/game-servers/{server_code}/ws"
          class="flex-1 min-w-[280px] rounded border border-gray-700 bg-gray-900 px-3 py-2 text-sm outline-none focus:border-neon disabled:opacity-50"
        />
        <button
          v-if="status === 'disconnected'"
          @click="connect"
          class="rounded bg-neon px-4 py-2 text-sm font-bold text-gray-950 hover:opacity-90"
        >
          连接
        </button>
        <button
          v-else
          @click="disconnect"
          class="rounded border border-gray-600 px-4 py-2 text-sm text-gray-300 hover:border-rose-400 hover:text-rose-400"
        >
          断开
        </button>
        <button
          @click="clearMessages"
          class="rounded border border-gray-700 px-3 py-2 text-sm text-gray-400 hover:text-gray-200"
        >
          清空
        </button>
      </div>

      <div class="flex items-center gap-4 text-xs text-gray-500 flex-wrap">
        <span>事件总数：<span class="text-gray-200">{{ messages.length }}</span></span>
        <span>去重丢弃：<span class="text-gray-200">{{ dedupedCount }}</span></span>
        <span>更早历史：<span class="text-gray-200">{{ hasMoreHistory ? '有' : '无' }}</span></span>
        <span v-if="status === 'reconnecting'">下次重连：<span class="text-amber-400">{{ reconnectDelay }}ms（第 {{ reconnectAttempts }} 次）</span></span>
        <span v-if="historyLoading" class="text-neon-cyan">加载历史中…</span>
      </div>

      <!-- 手动设置会话 Cookie -->
      <div class="rounded border border-gray-800 p-2 space-y-2">
        <div class="flex items-center gap-2 flex-wrap">
          <span class="text-xs font-semibold text-gray-300">会话 Cookie</span>
          <span
            class="text-xs px-1.5 rounded border"
            :class="cookieApplied ? 'border-neon text-neon' : 'border-gray-600 text-gray-500'"
          >
            {{ cookieApplied ? '● 已设置' : '○ 未设置' }}
          </span>
          <code class="text-xs text-gray-500">{{ COOKIE_NAME }}</code>
        </div>
        <div class="flex items-center gap-2 flex-wrap">
          <input
            v-model="cookieInput"
            :type="cookieReveal ? 'text' : 'password'"
            placeholder="粘贴后端 revolcast_session 的 Cookie 值"
            class="flex-1 min-w-[240px] rounded border border-gray-700 bg-gray-900 px-3 py-2 text-sm outline-none focus:border-neon"
          />
          <button
            @click="cookieReveal = !cookieReveal"
            class="rounded border border-gray-700 px-3 py-2 text-xs text-gray-400 hover:text-gray-200"
          >
            {{ cookieReveal ? '隐藏' : '显示' }}
          </button>
          <button
            @click="applyCookie"
            class="rounded bg-neon/20 border border-neon px-3 py-2 text-xs text-neon hover:bg-neon/30"
          >
            应用
          </button>
          <button
            @click="clearCookie"
            class="rounded border border-gray-700 px-3 py-2 text-xs text-gray-400 hover:text-rose-400 hover:border-rose-400"
          >
            清除
          </button>
        </div>
        <p class="text-xs text-gray-600 leading-relaxed">
          Cookie 只写到当前页面源（{{ currentOrigin || '—' }}）。要让后端收到，WS / 历史事件地址须同源——
          即用本页默认代理地址（localhost:5173），或前端由后端同源托管。跨源直连后端（如 localhost:8080）时浏览器不会附加此 Cookie。
        </p>
      </div>

      <p v-if="errorMessage" class="text-xs text-rose-400">{{ errorMessage }}</p>
    </header>

    <!-- 消息列表 -->
    <main
      ref="listEl"
      @scroll="onScroll"
      class="flex-1 overflow-y-auto px-4 py-3 text-sm leading-relaxed space-y-0.5"
    >
      <p v-if="messages.length === 0" class="text-gray-600 select-none">
        {{ status === 'connected' ? '等待实时事件…（上滑可加载更早历史）' : '尚未连接。填入 WS 地址后点击「连接」。' }}
      </p>
      <p
        v-for="ev in messages"
        :key="ev.id"
        :class="lineClass(ev)"
        class="whitespace-pre-wrap break-words"
      >
        {{ formatLine(ev) }}
      </p>
      <p v-if="historyLoading" class="text-neon-cyan">— 加载更早历史 —</p>
    </main>

    <!-- 运行日志 -->
    <footer class="border-t border-gray-800 px-4 py-2 text-xs text-gray-500 h-28 overflow-y-auto">
      <div v-for="(l, i) in logs" :key="i" class="whitespace-pre-wrap">
        <span class="text-gray-600">[{{ l.t }}]</span> {{ l.msg }}
      </div>
    </footer>
  </div>
</template>
