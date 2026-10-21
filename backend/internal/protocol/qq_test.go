package protocol

import (
	"encoding/json"
	"regexp"
	"testing"
)

func TestNewCommandID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := NewCommandID()
		if !re.MatchString(id) {
			t.Fatalf("command_id 格式非法: %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) < 990 {
		t.Fatalf("command_id 碰撞过多: 唯一数 %d/1000", len(seen))
	}
}

func TestQQMessagePayloadRoundTrip(t *testing.T) {
	in := QQMessagePayload{
		Platform:     "qq",
		MessageID:    "123457",
		SelfID:       "100001",
		Conversation: QQConversation{Type: "private", ID: "300001"},
		Sender:       QQSender{ID: "300001", Nickname: "Clover"},
		Message: QQMessage{
			Text:     "revolcast user bind",
			Segments: []QQSegment{NewTextSegment("revolcast user bind")},
		},
		IsToMe:     true,
		EventTime:  1734567890123,
		ReceivedAt: 1734567890156,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var out QQMessagePayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if out.Conversation.Type != "private" || out.Sender.ID != "300001" || out.Message.Text != "revolcast user bind" {
		t.Fatalf("往返后字段不一致: %+v", out)
	}
	if len(out.Message.Segments) != 1 || out.Message.Segments[0].Type != "text" {
		t.Fatalf("消息段往返异常: %+v", out.Message.Segments)
	}
}

func TestQQSendPrivateMsgPayloadCommandID(t *testing.T) {
	p := QQSendPrivateMsgPayload{
		CommandID:       NewCommandID(),
		ReceiverID:      "300001",
		MessageSegments: []QQSegment{NewTextSegment("hi")},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var out QQSendPrivateMsgPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if out.CommandID == "" || out.ReceiverID != "300001" {
		t.Fatalf("私聊指令字段不一致: %+v", out)
	}
}
