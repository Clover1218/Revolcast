package repository

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// bindCodeAttemptKeyPrefix 对应数据库设计文档 §5 的限流键名：
// revolcast:bindcode:attempt:{subject}，subject 为 qq_uin。
const bindCodeAttemptKeyPrefix = "revolcast:bindcode:attempt:"

// authRateLimitKeyPrefix 对应 HTTP 接口文档 §5 的限流键名前缀：
// revolcast:ratelimit:auth:{scope}:{subject}，subject 为客户端 IP。
const authRateLimitKeyPrefix = "revolcast:ratelimit:auth:"

// RedisRepo 封装短期状态（限流计数）。
// Redis 不作为权威存储；真实业务数据以 PostgreSQL 为准。
type RedisRepo struct {
	client *redis.Client
}

func NewRedisRepo(client *redis.Client) *RedisRepo {
	return &RedisRepo{client: client}
}

// AllowAttempt 通用限流：检查并累加计数。
// key 由调用方按 revolcast:ratelimit:{scope}:{subject}（或 bindcode 前缀）拼装。
// 返回 allowed=false 表示命中限流。
//
// 失败策略：Redis 不可用时 fail-open（放行），仅返回错误供上层记录，
// 避免基础设施故障锁死用户（数据库设计文档 §7.9 日志最小化，错误不回传明细）。
func (r *RedisRepo) AllowAttempt(ctx context.Context, key string, window time.Duration, maxAttempts int64) (bool, error) {
	cnt, err := r.client.Incr(ctx, key).Result()
	if err != nil {
		return true, fmt.Errorf("限流计数失败(fail-open): %w", err)
	}
	if cnt == 1 {
		// 仅首次写入时设置过期，保证窗口从第一次请求开始计算。
		if err := r.client.Expire(ctx, key, window).Err(); err != nil {
			return true, fmt.Errorf("限流过期设置失败(fail-open): %w", err)
		}
	}
	return cnt <= maxAttempts, nil
}

// AllowBindCodeAttempt 兼容既有调用（BIZ-001）：委托 AllowAttempt。
func (r *RedisRepo) AllowBindCodeAttempt(ctx context.Context, qqUIN int64, window time.Duration, maxAttempts int64) (bool, error) {
	key := bindCodeAttemptKeyPrefix + fmt.Sprintf("%d", qqUIN)
	return r.AllowAttempt(ctx, key, window, maxAttempts)
}

// AuthRateLimitKey 拼装 auth 限流键：revolcast:ratelimit:auth:{scope}:{subject}。
func AuthRateLimitKey(scope, subject string) string {
	return authRateLimitKeyPrefix + scope + ":" + subject
}

// userRateLimitKeyPrefix 对应 HTTP 接口文档限流键名前缀：
// revolcast:ratelimit:user:{scope}:{subject}，subject 为登录用户 user_id。
const userRateLimitKeyPrefix = "revolcast:ratelimit:user:"

// UserRateLimitKey 拼装已认证接口的限流键：revolcast:ratelimit:user:{scope}:{subject}。
func UserRateLimitKey(scope, subject string) string {
	return userRateLimitKeyPrefix + scope + ":" + subject
}

// serverRateLimitKeyPrefix 对应 game_servers 管理/读接口的限流键名前缀：
// revolcast:ratelimit:server:{scope}:{subject}，subject 为 IP 或全局标识。
const serverRateLimitKeyPrefix = "revolcast:ratelimit:server:"

// ServerRateLimitKey 拼装 game_servers 接口的限流键：revolcast:ratelimit:server:{scope}:{subject}。
func ServerRateLimitKey(scope, subject string) string {
	return serverRateLimitKeyPrefix + scope + ":" + subject
}

// wsRateLimitKeyPrefix 对应 WebSocket 握手限流的键名前缀：
// revolcast:ratelimit:ws:{scope}:{subject}，scope 为 handshake_user / handshake_ip，
// subject 分别为 user_id 与客户端 IP。Redis 为共享计数，多实例部署下配额一致。
const wsRateLimitKeyPrefix = "revolcast:ratelimit:ws:"

// WSRateLimitKey 拼装 WebSocket 相关限流键：revolcast:ratelimit:ws:{scope}:{subject}。
func WSRateLimitKey(scope, subject string) string {
	return wsRateLimitKeyPrefix + scope + ":" + subject
}

// gameRateLimitKeyPrefix 对应游戏内指令的限流键名前缀：
// revolcast:ratelimit:game:{scope}:{server_id}:{subject}。
const gameRateLimitKeyPrefix = "revolcast:ratelimit:game:"

// GameRedeemRateLimitKey 拼装「游戏内兑换角色绑定码」的限流键。
//
// 维度为 (server_id + player_uuid)：同一玩家在不同服务器分别计数；同一服务器内一个角色
// 的尝试次数被独立限制。与 BIZ-009 的发码限流（qq_uin + server_code）不同，
// 这里是**猜码**场景，必须在查询绑定码之前计数（见《游戏角色绑定与解绑闭环方案》§9）。
func GameRedeemRateLimitKey(serverID int64, playerUUID string) string {
	return gameRateLimitKeyPrefix + "redeem:" + strconv.FormatInt(serverID, 10) + ":" + playerUUID
}
