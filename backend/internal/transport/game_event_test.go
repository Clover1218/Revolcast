package transport

import (
	"encoding/json"
	"testing"

	"go.uber.org/zap"
)

// parseOnlyAdapter 构造仅用于解析测试的 Adapter 连接（不触碰底层 conn / hub）。
func parseOnlyAdapter(clientID string) *AdapterClient {
	return newAdapterClient(nil, nil, clientID, zap.NewNop())
}

// parseOnlyGameServer 构造仅用于解析测试的游戏服务器连接。
func parseOnlyGameServer(serverID int64, code string) *GameServerClient {
	return newGameServerClient(nil, nil, serverID, code, zap.NewNop())
}

func TestParseGameServerEvent_OK(t *testing.T) {
	c := parseOnlyGameServer(42, "survival")
	raw := []byte(`{"sub_type":"player_join","timestamp":1792395025,"server_name":"TestServer","player":{"uuid":"u","nickname":"n","is_op":true}}`)

	task, ok := c.parseGameServerEvent(raw)
	if !ok {
		t.Fatal("合法事件应解析成功")
	}
	if task.Type != "event.game.player_join" {
		t.Errorf("Type = %q, want event.game.player_join", task.Type)
	}
	// 业务归属必须是已鉴权的 server_id（而非 server_code），与事件落库规则一致。
	if task.ClientID != "42" {
		t.Errorf("ClientID = %q, want 42", task.ClientID)
	}
	if task.TraceID == "" {
		t.Error("鹊桥未提供 trace_id，应生成占位 trace_id")
	}
	// Payload 必须保留原始字节，供后续 handler 解析具体字段。
	var probe map[string]any
	if err := json.Unmarshal(task.Payload, &probe); err != nil {
		t.Fatalf("Payload 应为原始 JSON: %v", err)
	}
	if probe["sub_type"] != "player_join" {
		t.Errorf("Payload 应原样保留 sub_type, got %v", probe["sub_type"])
	}
}

func TestParseGameServerEvent_MissingSubType(t *testing.T) {
	c := parseOnlyGameServer(1, "s")
	if _, ok := c.parseGameServerEvent([]byte(`{"timestamp":1}`)); ok {
		t.Fatal("缺少 sub_type 应返回 ok=false")
	}
}

func TestParseGameServerEvent_InvalidJSON(t *testing.T) {
	c := parseOnlyGameServer(1, "s")
	if _, ok := c.parseGameServerEvent([]byte("not-json")); ok {
		t.Fatal("非法 JSON 应返回 ok=false")
	}
}

func TestParseEnvelopeEvent_OK(t *testing.T) {
	c := parseOnlyAdapter("bot-1")
	raw := []byte(`{"version":"v1","type":"event.qq.message.private","trace_id":"t1","payload":{"a":1}}`)

	task, ok := c.parseEnvelopeEvent(raw)
	if !ok {
		t.Fatal("合法 Envelope 应解析成功")
	}
	if task.Type != "event.qq.message.private" || task.TraceID != "t1" {
		t.Errorf("Envelope 解析错误: %+v", task)
	}
	if task.ClientID != "bot-1" {
		t.Errorf("ClientID = %q, want bot-1", task.ClientID)
	}
}

func TestIsGameEventType(t *testing.T) {
	yes := []string{
		"event.game.player_join",
		"event.game.player_quit",
		"event.game.player_chat",
		"event.game.player_command",
	}
	for _, y := range yes {
		if !isGameEventType(y) {
			t.Errorf("%q 应识别为游戏事件类型", y)
		}
	}
	if isGameEventType("event.qq.message.private") || isGameEventType("") {
		t.Error("非游戏事件类型不应被误识别")
	}
}
