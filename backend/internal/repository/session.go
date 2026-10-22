package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionRepo 封装 web_sessions 表的访问。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.9 为准：
// 库只存 Token 的 SHA-256 哈希（token_hash），原始 Token 由调用方持有并下发给客户端。
type SessionRepo struct {
	pool *pgxpool.Pool
}

func NewSessionRepo(pool *pgxpool.Pool) *SessionRepo {
	return &SessionRepo{pool: pool}
}

// CreateSession 写入一条登录会话。
// tokenHash 为原始 Token 的 SHA-256 十六进制（64 字符）；deviceName 为空时存 NULL（列允许 NULL）。
// created_at / last_seen_at 由数据库默认值填充（now()），revoked_at 初始为 NULL（有效）。
func (r *SessionRepo) CreateSession(ctx context.Context, userID int64, tokenHash, deviceName string, expiresAt time.Time) error {
	const q = `INSERT INTO web_sessions (user_id, token_hash, device_name, expires_at) VALUES ($1, $2, $3, $4)`
	var dev sql.NullString
	if deviceName != "" {
		dev = sql.NullString{String: deviceName, Valid: true}
	}
	if _, err := r.pool.Exec(ctx, q, userID, tokenHash, dev, expiresAt); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

// ResolveSession 通过 Token 哈希解析出 user_id，并顺带刷新 last_seen_at（数据库文档 §3.9 规则：
// 每次鉴权请求由应用层显式更新）。仅当会话有效（revoked_at IS NULL 且未过期）时返回 ok=true；
// 否则 ok=false（令牌不存在 / 已撤销 / 已过期），调用方据此返回 401（未认证）。
func (r *SessionRepo) ResolveSession(ctx context.Context, tokenHash string) (userID int64, ok bool, err error) {
	const q = `UPDATE web_sessions SET last_seen_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING user_id`
	var id int64
	scanErr := r.pool.QueryRow(ctx, q, tokenHash).Scan(&id)
	if scanErr == pgx.ErrNoRows {
		return 0, false, nil
	}
	if scanErr != nil {
		return 0, false, fmt.Errorf("解析会话失败: %w", scanErr)
	}
	return id, true, nil
}

// RevokeSession 撤销一条登录会话（软删除：写入 revoked_at）。
// tokenHash 为原始 Token 的 SHA-256 十六进制（64 字符）。
// 仅当该会话当前仍有效（revoked_at IS NULL 且未过期）时才置位；
// 返回 affected 表示是否实际撤销了某条会话。0 行影响（令牌不存在 / 已撤销 / 已过期）
// 表示无有效会话可撤销，调用方据此判定为「未认证」。
func (r *SessionRepo) RevokeSession(ctx context.Context, tokenHash string) (affected bool, err error) {
	const q = `UPDATE web_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`
	tag, err := r.pool.Exec(ctx, q, tokenHash)
	if err != nil {
		return false, fmt.Errorf("撤销会话失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
