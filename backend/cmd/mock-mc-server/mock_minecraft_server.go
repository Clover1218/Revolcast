// 命令 mock-mc-server 是一个「模拟 Minecraft 服务器（鹊桥）」的压测工具。
//
// 它扮演 game-servers 接入端，连接后端的游戏服务器 WS 路由
//   /api/v1/game-servers/ws   (client_type=mc_server)
// 并以高频率发送带递增数字的玩家聊天事件（player_chat），用于压测
// 「事件落库 + 推送给 Web 订阅者」这条链路。
//
// 鉴权：该路由使用 per-server API Secret，作为查询参数 token 发送
//   （服务端 SHA256(token) 查 game_servers.api_secret_hash，要求 status=1）。
// 这是 game-servers 路由的凭证，**不是**浏览器 Cookie；Cookie 仅用于 Web 订阅路由
// （/api/v1/game-servers/{server_code}/ws，revolcast_session）。本工具额外暴露一个
// 可选 Cookie 配置项，便于你在指向其他路由/特殊部署时附带。
//
// 上行事件格式：鹊桥原始 JSON（不带统一 Envelope），后端按 sub_type 派生事件类型；
// server_id 由已鉴权身份派生，本工具不发送。
//
// 运行：
//   cd backend && go run ./cmd/mock-mc-server
// 也可用命令行参数覆盖下方变量，例如：
//   go run ./cmd/mock-mc-server -url ws://127.0.0.1:8080/api/v1/game-servers/ws -secret <APISecret> -interval 5ms

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ===================== 可调配置（压测前按需修改） =====================

// WSConnURL 游戏服务器（鹊桥）接入后端的 WS 地址。
// 对应路由 /api/v1/game-servers/ws（client_type=mc_server）。
var wsURL = "ws://127.0.0.1:8080/api/v1/game-servers/ws"

// APISecret 该服务器的 API Secret（创建/轮换服务器时一次性下发）。
// 作为查询参数 token 发送；服务端用 SHA256(token) 查 game_servers.api_secret_hash（要求 status=1）。
// 注意：这是 game-servers 路由的凭证，不是浏览器 Cookie。
var apiSecret = "f7d4495dd237a5d319b47cb8a13ac2e8f6c4d40f8cb06296d52f3f85d557ad86"

// Cookie 可选：若需把请求附带 Cookie（例如指向 Web 订阅路由或特殊部署），
// 在此填写，形如 "revolcast_session=xxxx"。game-servers 路由不需要，留空 "" 即可。
var cookie = ""

// 模拟玩家信息（仅用于事件展示 / 审计快照，server_id 不由 server_name 反查）。
var (
	playerNickname = "LoadTestBot"
	playerUUID     = "00000000-0000-0000-0000-000000000001"
	playerIsOp     = false
	serverName     = "MockServer"
)

// 发送节奏与统计。
var (
	sendInterval  = 100 * time.Millisecond // 每条聊天事件间隔；调小 = 更高频率（如 5ms / 1ms）
	autoReconnect = true                   // 断线是否自动重连
	reconnectWait = 1 * time.Second        // 重连前等待
	messagePrefix = "loadtest"             // 聊天内容前缀，后面追加递增数字
)

// ===================== 事件结构（鹊桥原始 JSON） =====================

// queqiaoPlayer 是原始事件中的玩家对象快照（仅抽取审计所需字段）。
type queqiaoPlayer struct {
	Nickname string `json:"nickname"`
	UUID     string `json:"uuid"`
	IsOp     bool   `json:"is_op"`
}

// gameEvent 与 backend/internal/protocol/game_event.go 的 GameEventPayload 对齐。
// 上行为原始 JSON（不包统一 Envelope）。
type gameEvent struct {
	Timestamp     int64         `json:"timestamp"`      // 事件时间戳（秒级 Unix）
	PostType      string        `json:"post_type"`      // message
	EventName     string        `json:"event_name"`     // PlayerChatEvent
	ServerName    string        `json:"server_name"`    // 仅展示 / 自检
	ServerVersion string        `json:"server_version"` // 1.20.1
	ServerType    string        `json:"server_type"`    // forge
	SubType       string        `json:"sub_type"`       // player_chat
	Player        queqiaoPlayer `json:"player"`
	MessageID     string        `json:"message_id"`  // 聊天事件
	RawMessage    string        `json:"raw_message"` // Minecraft 文本组件 JSON
	Message       string        `json:"message"`     // 纯文本
}

// newChatEvent 构造一条递增编号的玩家聊天事件。
func newChatEvent(seq int) gameEvent {
	text := fmt.Sprintf("%s %d", messagePrefix, seq)
	rawObj, _ := json.Marshal(map[string]string{"text": text}) // {"text":"loadtest 1"}
	return gameEvent{
		Timestamp:     time.Now().Unix(),
		PostType:      "message",
		EventName:     "PlayerChatEvent",
		ServerName:    serverName,
		ServerVersion: "1.20.1",
		ServerType:    "forge",
		SubType:       "player_chat",
		Player:        queqiaoPlayer{Nickname: playerNickname, UUID: playerUUID, IsOp: playerIsOp},
		MessageID:     strconv.Itoa(seq),
		RawMessage:    string(rawObj),
		Message:       text,
	}
}

// ===================== 连接与发送 =====================

// dial 建立 WS 连接：把 APISecret 作为 token 查询参数，可选附带 Cookie 头。
func dial() (*websocket.Conn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("解析 WS 地址失败: %w", err)
	}
	q := u.Query()
	if apiSecret != "" {
		q.Set("token", apiSecret)
	}
	u.RawQuery = q.Encode()

	header := http.Header{}
	if cookie != "" {
		header.Set("Cookie", cookie)
	}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return nil, fmt.Errorf("WS 拨号失败: %w", err)
	}
	return conn, nil
}

// readPump 持续读取服务端消息（丢弃内容），同时让 gorilla 自动响应 ping/pong，
// 保持连接健康并尽快感知断开。
func readPump(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// sendLoop 以 sendInterval 为间隔持续发送递增聊天事件，直到 ctx 取消或写失败。
func sendLoop(ctx context.Context, conn *websocket.Conn, sent *int64) error {
	seq := 0
	ticker := time.NewTicker(sendInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		seq++
		data, err := json.Marshal(newChatEvent(seq))
		if err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return err
		}
		atomic.AddInt64(sent, 1)
	}
}

// reportLoop 每秒打印一次发送统计。
func reportLoop(ctx context.Context, sent *int64) {
	last := int64(0)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := atomic.LoadInt64(sent)
			rate := cur - last
			last = cur
			log.Printf("[STAT] 已发送 %d 条，近 1s 速率 %d 条/s", cur, rate)
		}
	}
}

func main() {
	flag.StringVar(&wsURL, "url", wsURL, "游戏服务器 WS 接入地址")
	flag.StringVar(&apiSecret, "secret", apiSecret, "服务器 API Secret（作为 ?token= 鉴权）")
	flag.StringVar(&cookie, "cookie", cookie, "可选 Cookie 头，形如 revolcast_session=xxxx")
	flag.DurationVar(&sendInterval, "interval", sendInterval, "每条消息间隔，如 5ms / 1ms")
	flag.Parse()

	if apiSecret == "" || apiSecret == "在此填入你的服务器 API Secret" {
		log.Println("[WARN] apiSecret 未配置，服务端会返回 401；请在文件顶部变量或 -secret 填入真实服务器 API Secret")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("收到退出信号，停止发送")
		cancel()
	}()

	var sent int64
	go reportLoop(ctx, &sent)

	for {
		select {
		case <-ctx.Done():
			log.Println("已停止")
			return
		default:
		}

		conn, err := dial()
		if err != nil {
			log.Printf("[ERROR] %v", err)
			if !autoReconnect {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectWait):
				continue
			}
		}

		log.Printf("已连接 %s，开始发送聊天事件（间隔 %s）", wsURL, sendInterval)
		go readPump(conn)
		err = sendLoop(ctx, conn, &sent)
		conn.Close()
		if err != nil {
			log.Printf("[WARN] 发送循环退出: %v", err)
		}

		if !autoReconnect {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectWait):
		}
	}
}
