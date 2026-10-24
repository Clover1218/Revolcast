package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"revolcast-qq-adapter/internal/config"
	"revolcast-qq-adapter/internal/pkg/logger"
	"revolcast-qq-adapter/internal/protocol"
	"revolcast-qq-adapter/internal/transport"

	zero "github.com/wdvxdr1123/ZeroBot"

	"github.com/wdvxdr1123/ZeroBot/driver"
	"github.com/wdvxdr1123/ZeroBot/message"

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
	wsURL := fmt.Sprintf("ws://%s/api/v1/adapter/ws?token=%s&client_id=%s", cfg.WS.Addr, cfg.WS.Token, cfg.WS.BotID)
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

	// ------------------------------------------------------------------
	// 【临时能力·可整块删除】群内 @ 机器人 = 私聊命令
	//
	// 目的：在群里 @ 机器人时，复用与私聊完全相同的指令识别与业务分发
	// （Backend route 对两种会话走同一套 switch），回执落在原群。
	// 只在「@ 了机器人 / 以昵称开头」时上报，普通群聊一律不转发，
	// 避免像私聊那样把整个群的消息都灌给 Backend。
	//
	// 删除方式：删掉本段 handler 与 buildGroupAtPayload / plainText 即可，
	// 不影响私聊链路（Backend 的群聊分支届时不再被激活）。
	// 已知取舍：`revolcast user bind` / `server <code> bind` 生成的绑定码会公开在群里，
	// 需要收紧时在此处按指令白名单过滤。
	// ------------------------------------------------------------------
	zero.OnMessage(zero.OnlyGroup, zero.OnlyToMe).Handle(func(zctx *zero.Ctx) {
		payload := buildGroupAtPayload(zctx)
		client.SetSelfID(zctx.Event.SelfID) // 群聊场景下也可能从未有过私聊，这里同样记录
		client.SendEnvelope(protocol.TypeEventQQMessageGroup, protocol.NewCommandID(), payload)
	})

	// ---- NapCat（OneBot WS 服务端）连接参数：全部来自配置，不再写死在代码里 ----
	// 容器化后 NapCat 通常跑在宿主机，地址形如 ws://host.docker.internal:6701；
	// 直连时可填 ws://127.0.0.1:6701。对应环境变量 REVOLCAST_NAPCAT_WS_URL。
	napcatURL := strings.TrimSpace(cfg.NapCat.WSURL)
	if napcatURL == "" {
		panic("配置缺失: napcat.ws_url 为空（请设置 REVOLCAST_NAPCAT_WS_URL）")
	}

	nickNames := make([]string, 0, len(cfg.Bot.NickName))
	for _, n := range cfg.Bot.NickName {
		if s := strings.TrimSpace(n); s != "" {
			nickNames = append(nickNames, s)
		}
	}
	if len(nickNames) == 0 {
		nickNames = []string{"bot"}
	}

	superUsers, invalidSuperUsers := parseSuperUsers(cfg.Bot.SuperUsers)
	if len(invalidSuperUsers) > 0 {
		// 只告警不阻断：写错一个 QQ 号不应让整个机器人起不来
		log.Warn("忽略非法的超级用户 QQ 号", zap.Strings("invalid", invalidSuperUsers))
	}

	log.Info("NapCat 连接配置",
		zap.String("ws_url", napcatURL),
		zap.Bool("token_set", cfg.NapCat.Token != ""),
		zap.Strings("nickname", nickNames),
		zap.Int("super_users", len(superUsers)),
	)

	zero.RunAndBlock(&zero.Config{
		NickName:      nickNames,
		CommandPrefix: cfg.Bot.CommandPrefix,
		SuperUsers:    superUsers,
		Driver: []zero.Driver{
			driver.NewWebSocketClient(napcatURL, cfg.NapCat.Token),
		},
	}, nil)
}

// parseSuperUsers 把配置里的 QQ 号字符串转成 ZeroBot 需要的 int64 列表。
// 非数字 / 非正数视为非法项，交由调用方告警后忽略。
func parseSuperUsers(list []string) (ids []int64, invalid []string) {
	for _, raw := range list {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			invalid = append(invalid, s)
			continue
		}
		ids = append(ids, id)
	}
	return ids, invalid
}

// buildGroupAtPayload 把「群内 @ 机器人」的消息转换为统一协议 payload。
//
// 与私聊 payload 的差别只有一处：message.text 不能直接用 RawMessage。
// 群聊的 raw_message 里仍带着 [CQ:at,qq=...] 前缀（ZeroBot 只会从 Message 段里移除 @，
// 不会改写 RawMessage），而 Backend 的指令识别要求首个词是 revolcast，
// 因此这里用已解析的消息段重拼纯文本。
func buildGroupAtPayload(zctx *zero.Ctx) protocol.QQMessagePayload {
	p := buildPrivatePayload(zctx)
	p.Message.Text = plainText(zctx.Event.Message)
	p.IsToMe = true // 群聊走到这里说明确实 @ 了机器人
	return p
}

// plainText 拼接消息段中的文本内容；@ / 图片 / 表情等非文本段不参与指令识别。
func plainText(msg message.Message) string {
	var b strings.Builder
	for _, seg := range msg {
		if seg.Type == "text" {
			b.WriteString(seg.Data["text"])
		}
	}
	return strings.TrimSpace(b.String())
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
