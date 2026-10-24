package transport

import (
	"context"
	"encoding/json"
	"testing"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// newGroupMessageTask 构造一条「群内 @ 机器人」的任务（Adapter 只在这种情况下上报群聊）。
func newGroupMessageTask(t *testing.T, groupID, senderID, text string) queue.Task {
	t.Helper()
	p := protocol.QQMessagePayload{
		Platform:     "qq",
		Conversation: protocol.QQConversation{Type: "group", ID: groupID},
		Sender:       protocol.QQSender{ID: senderID},
		Message:      protocol.QQMessage{Text: text},
		IsToMe:       true,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("序列化 payload 失败: %v", err)
	}
	return queue.Task{Type: protocol.TypeQQMessageGroup, Payload: raw, TraceID: "trace-group"}
}

// groupReply 从 outbound 中取出群聊回执（群号 + 文本）。
func groupReply(t *testing.T, out *outbound) (groupID, text string) {
	t.Helper()
	if out == nil {
		t.Fatal("应产生回执指令")
	}
	if out.Type != protocol.TypeQQSendGroupMessage {
		t.Fatalf("指令类型错误: %s", out.Type)
	}
	p, ok := out.Payload.(protocol.QQSendGroupMsgPayload)
	if !ok {
		t.Fatalf("payload 类型错误: %T", out.Payload)
	}
	if len(p.MessageSegments) == 0 {
		t.Fatal("缺少消息段")
	}
	if err := json.Unmarshal(p.MessageSegments[0].Data["text"], &text); err != nil {
		t.Fatalf("解析文本段失败: %v", err)
	}
	return p.GroupID, text
}

// TestRoute_Group_ReplyToGroup 群内 @ 机器人执行指令时，回执发到原群（不是私聊发送者），
// 且文案与私聊链路完全一致（两者共用同一个 switch）。
func TestRoute_Group_ReplyToGroup(t *testing.T) {
	svc := &fakeStatusSvc{view: &biz.ServerStatusView{
		ServerCode: "element",
		Online:     2,
		Players:    []biz.StatusPlayer{{Nickname: "Bot1"}, {Nickname: "Bot2"}},
	}}
	out, err := route(context.Background(),
		newGroupMessageTask(t, "200001", "300001", "revolcast server element status"),
		nil, nil, svc, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	groupID, text := groupReply(t, out)
	if groupID != "200001" {
		t.Fatalf("回执应发往群号 200001，实际 %q", groupID)
	}
	if text != "当前服务器有 2 人：Bot1，Bot2" {
		t.Fatalf("文案不符: %s", text)
	}
}

// TestRoute_Group_NotACommand 群里 @ 机器人但说的不是 Revolcast 指令：静默，不在群里刷屏。
func TestRoute_Group_NotACommand(t *testing.T) {
	out, err := route(context.Background(),
		newGroupMessageTask(t, "200001", "300001", "大家好"),
		nil, nil, &fakeStatusSvc{}, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	if out != nil {
		t.Fatalf("非指令消息不应产生回执，实际类型 %s", out.Type)
	}
}

// TestRoute_Group_ConversationTypeMismatch event 类型与 conversation.type 错配时直接丢弃，
// 不允许「group 事件带 private 会话」这类上游组装错误被当成私聊处理。
func TestRoute_Group_ConversationTypeMismatch(t *testing.T) {
	p := protocol.QQMessagePayload{
		Platform:     "qq",
		Conversation: protocol.QQConversation{Type: "private", ID: "300001"},
		Sender:       protocol.QQSender{ID: "300001"},
		Message:      protocol.QQMessage{Text: "revolcast server list"},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("序列化 payload 失败: %v", err)
	}
	out, err := route(context.Background(),
		queue.Task{Type: protocol.TypeQQMessageGroup, Payload: raw, TraceID: "trace-mismatch"},
		nil, nil, &fakeStatusSvc{}, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	if out != nil {
		t.Fatalf("会话类型错配应丢弃，实际产生回执 %s", out.Type)
	}
}
