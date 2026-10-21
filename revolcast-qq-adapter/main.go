package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"revolcast-qq-adapter/internal/config"
	"revolcast-qq-adapter/internal/pkg/logger"
	"revolcast-qq-adapter/internal/protocol"
	"revolcast-qq-adapter/internal/transport"

	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/driver"
	"go.uber.org/zap"
)

func main() {
	cfg, err := config.LoadConfig(".")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}
	log, err := logger.New(&cfg.Log)
	if err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer log.Sync()

	log.Info("Revolcast 启动中",
		zap.String("service", cfg.App.Name),
		zap.String("env", cfg.App.Env),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 连接 Backend WS（与 Backend 的 ws 配置对应）。
	wsURL := fmt.Sprintf("ws://%s/ws?token=%s&bot_id=%s", cfg.WS.Addr, cfg.WS.Token, cfg.WS.BotID)
	client := transport.NewWSClient(ctx, wsURL, log)
	go client.Start()

	// 保留一个简单健康检查命令，便于联调。
	zero.OnCommand("hello").Handle(func(zctx *zero.Ctx) {
		zctx.Send("world")
	})

	// BIZ-001 触发端：把 QQ 私聊消息转换为标准化 event.qq.message.private 上报 Backend。
	// 业务命令识别（revolcast user bind）与绑定码生成由 Backend 统一负责，
	// Adapter 不承担核心业务决策（见 AGENTS.md §3.2）。
	zero.OnMessage(zero.OnlyPrivate).Handle(func(zctx *zero.Ctx) {
		payload := buildPrivatePayload(zctx)
		client.SetSelfID(zctx.Event.SelfID) // 记录本 bot 的 QQ 号，供出站私聊发送使用
		client.SendEnvelope(protocol.TypeEventQQMessagePrivate, protocol.NewCommandID(), payload)
	})

	zero.RunAndBlock(&zero.Config{
		NickName:      []string{"bot"},
		CommandPrefix: "",
		SuperUsers:    []int64{1457095790},
		Driver: []zero.Driver{
			driver.NewWebSocketClient("ws://127.0.0.1:6701", "J5xTRRESL-Fsdq1b"),
		},
	}, nil)
}

// buildPrivatePayload 把原生 OneBot 私聊事件转换为统一协议 QQMessagePayload。
// 仅转换 BIZ-001 需要的字段；富媒体段原样保留在 segments 中。
func buildPrivatePayload(zctx *zero.Ctx) protocol.QQMessagePayload {
	ev := zctx.Event
	senderID := strconv.FormatInt(ev.UserID, 10)
	convID := senderID
	if ev.MessageType == "group" {
		convID = strconv.FormatInt(ev.GroupID, 10)
	}

	segs := make([]protocol.QQSegment, 0, len(ev.Message))
	for _, s := range ev.Message {
		data := make(map[string]json.RawMessage, len(s.Data))
		for k, v := range s.Data {
			raw, _ := json.Marshal(v)
			data[k] = raw
		}
		segs = append(segs, protocol.QQSegment{Type: s.Type, Data: data})
	}

	nick := ""
	card := ""
	if ev.Sender != nil {
		nick = ev.Sender.NickName
		card = ev.Sender.Card
	}

	return protocol.QQMessagePayload{
		Platform:     "qq",
		MessageID:    fmt.Sprintf("%v", ev.MessageID),
		SelfID:       strconv.FormatInt(ev.SelfID, 10),
		Conversation: protocol.QQConversation{Type: ev.MessageType, ID: convID},
		Sender:       protocol.QQSender{ID: senderID, Nickname: nick, Card: card},
		Message:      protocol.QQMessage{Text: ev.RawMessage, Segments: segs},
		IsToMe:       ev.MessageType == "private",
		PlatformData: map[string]string{
			"post_type":   ev.PostType,
			"detail_type": ev.MessageType,
			"sub_type":    ev.SubType,
		},
		EventTime:  ev.Time * 1000, // 秒级 → 毫秒
		ReceivedAt: time.Now().UnixMilli(),
	}
}
