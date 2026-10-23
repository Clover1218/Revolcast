package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// fakeStatusSvc 是 gameStatusService 的测试替身。
type fakeStatusSvc struct {
	view     *biz.ServerStatusView
	err      error
	lastCode string
	calls    int
}

func (f *fakeStatusSvc) GetStatus(_ context.Context, serverCode string) (*biz.ServerStatusView, error) {
	f.calls++
	f.lastCode = serverCode
	return f.view, f.err
}

// newPrivateMessageTask 构造一条「用户私聊机器人」的任务。
func newPrivateMessageTask(t *testing.T, text string) queue.Task {
	t.Helper()
	p := protocol.QQMessagePayload{
		Platform:     "qq",
		Conversation: protocol.QQConversation{Type: "private", ID: "10001"},
		Sender:       protocol.QQSender{ID: "10001"},
		Message:      protocol.QQMessage{Text: text},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("序列化 payload 失败: %v", err)
	}
	return queue.Task{Type: protocol.TypeQQMessagePrivate, Payload: raw, TraceID: "trace-status"}
}

// replyText 从 outbound 中取出回执文本（单段纯文本）。
func replyText(t *testing.T, out *outbound) (receiver, text string) {
	t.Helper()
	if out == nil {
		t.Fatal("应产生回执指令")
	}
	if out.Type != protocol.TypeQQSendPrivateMessage {
		t.Fatalf("指令类型错误: %s", out.Type)
	}
	p, ok := out.Payload.(protocol.QQSendPrivateMsgPayload)
	if !ok {
		t.Fatalf("payload 类型错误: %T", out.Payload)
	}
	if len(p.MessageSegments) == 0 {
		t.Fatal("缺少消息段")
	}
	if err := json.Unmarshal(p.MessageSegments[0].Data["text"], &text); err != nil {
		t.Fatalf("解析文本段失败: %v", err)
	}
	return p.ReceiverID, text
}

// TestRoute_ServerStatus_Hit 命中快照时回「当前服务器有 N 人：…」，且 code 已归一化为小写。
func TestRoute_ServerStatus_Hit(t *testing.T) {
	svc := &fakeStatusSvc{view: &biz.ServerStatusView{
		ServerCode: "element",
		Online:     3,
		Players:    []biz.StatusPlayer{{Nickname: "Bot1"}, {Nickname: "Bot2"}, {Nickname: "Bot3"}},
	}}
	out, err := route(context.Background(), newPrivateMessageTask(t, "revolcast server ELEMENT status"),
		nil, nil, svc, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	receiver, text := replyText(t, out)
	if receiver != "10001" {
		t.Fatalf("接收者应为发送者 QQ: %s", receiver)
	}
	if text != "当前服务器有 3 人：Bot1，Bot2，Bot3" {
		t.Fatalf("文案不符: %s", text)
	}
	if svc.lastCode != "element" {
		t.Fatalf("查询的 code 应已转小写，实际 %q", svc.lastCode)
	}
}

// TestRoute_ServerStatus_NotFound 没有快照时回「未找到」+ 指引，不暴露内部细节。
func TestRoute_ServerStatus_NotFound(t *testing.T) {
	svc := &fakeStatusSvc{}
	out, err := route(context.Background(), newPrivateMessageTask(t, "revolcast server element status"),
		nil, nil, svc, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	_, text := replyText(t, out)
	if !strings.Contains(text, "未找到「element」的服务器状态") {
		t.Fatalf("未找到文案不符: %s", text)
	}
}

// TestRoute_ServerStatus_RedisFailure Redis 故障必须与「未找到」区分。
func TestRoute_ServerStatus_RedisFailure(t *testing.T) {
	svc := &fakeStatusSvc{err: errors.New("redis down")}
	out, err := route(context.Background(), newPrivateMessageTask(t, "revolcast server element status"),
		nil, nil, svc, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	_, text := replyText(t, out)
	if text != "状态查询暂时不可用，请稍后再试。" {
		t.Fatalf("故障文案不符: %s", text)
	}
}

// TestRoute_ServerStatus_NilService 未装配状态服务时不应 panic，回内部错误文案。
func TestRoute_ServerStatus_NilService(t *testing.T) {
	out, err := route(context.Background(), newPrivateMessageTask(t, "revolcast server element status"),
		nil, nil, nil, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	_, text := replyText(t, out)
	if text != biz.ResolveErrorMeta(biz.ErrInternal).WSMessage {
		t.Fatalf("文案不符: %s", text)
	}
}
