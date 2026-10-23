package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// GameServerListCacheKey 是「可用服务器列表」的缓存键。
// 沿用《Revolcast_数据库表设计文档_v1.1》§5 的 revolcast:cache:server:* 命名族；
// 该值可随时失效重建，**不是权威配置**（权威数据始终以 PostgreSQL 为准）。
const GameServerListCacheKey = "revolcast:cache:server:list:available"

// gameBindRateLimitKeyPrefix 对应游戏角色绑定码生成的限流键前缀：
// revolcast:ratelimit:qq:game_bind:{qq_uin}:{server_code}
// （《数据库表设计文档》§5 的 revolcast:ratelimit:{scope}:{subject} 口径）。
const gameBindRateLimitKeyPrefix = "revolcast:ratelimit:qq:"

// GameBindRateLimitKey 拼装游戏角色绑定码的限流键。
// 维度为 (qq_uin + server_code)：同一用户可为不同服务器分别申请，互不挤占额度。
func GameBindRateLimitKey(qqUIN int64, serverCode string) string {
	return fmt.Sprintf("%sgame_bind:%d:%s", gameBindRateLimitKeyPrefix, qqUIN, serverCode)
}

// GetCache 读取并反序列化缓存值到 dest。
//
// 返回值语义：
//   - hit=true, err=nil：命中且反序列化成功；
//   - hit=false, err=nil：键不存在（正常未命中）；
//   - hit=false, err!=nil：Redis 故障或值损坏，调用方应按 fail-open 处理（回源查库并重建缓存）。
//
// 不做 TTL 续期：TTL 由写入侧 SetCache 统一决定。
func (r *RedisRepo) GetCache(ctx context.Context, key string, dest any) (hit bool, err error) {
	raw, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return false, nil
		}
		return false, fmt.Errorf("读取缓存失败: %w", err)
	}
	if uErr := json.Unmarshal(raw, dest); uErr != nil {
		// 值损坏（结构变更 / 脏写）：视为未命中，由调用方回源并覆盖写入。
		return false, fmt.Errorf("反序列化缓存失败: %w", uErr)
	}
	return true, nil
}

// SetCache 以 JSON 序列化写入缓存并设置 TTL。
// 失败仅返回错误供上层记录，不影响业务结果（缓存永远是可丢失的加速层）。
func (r *RedisRepo) SetCache(ctx context.Context, key string, val any, ttl time.Duration) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("序列化缓存失败: %w", err)
	}
	if err := r.client.Set(ctx, key, raw, ttl).Err(); err != nil {
		return fmt.Errorf("写入缓存失败: %w", err)
	}
	return nil
}

// DelCache 删除一个或多个缓存键（数据变更后失效）。
func (r *RedisRepo) DelCache(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("删除缓存失败: %w", err)
	}
	return nil
}
