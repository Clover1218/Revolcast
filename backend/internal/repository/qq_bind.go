package repository

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bindCodeAlphabet 是绑定码字符集，排除易混淆字符 0/O/1/I/L。
// 长度为 32，便于用单字节随机值取模得到均匀分布。
const bindCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// QQBindRepo 封装 qq_bindings / qq_bind_codes 的访问。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.2 / §3.3 为准。
type QQBindRepo struct {
	pool *pgxpool.Pool
}

func NewQQBindRepo(pool *pgxpool.Pool) *QQBindRepo {
	return &QQBindRepo{pool: pool}
}

// IsQQActiveBound 判断指定 qq_uin 是否存在有效（status=1）绑定。
func (r *QQBindRepo) IsQQActiveBound(ctx context.Context, qqUIN int64) (bool, error) {
	const q = `SELECT 1 FROM qq_bindings WHERE qq_uin = $1 AND status = 1 LIMIT 1`
	var dummy int
	err := r.pool.QueryRow(ctx, q, qqUIN).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询 QQ 绑定状态失败: %w", err)
	}
	return true, nil
}

// GetPendingBindCode 返回该 qq_uin 当前未过期、待使用（status=0 且 expires_at>now）的绑定码。
// 对应数据库设计文档 §3.3 的部分唯一索引 ux_qq_bind_codes_pending_qq，保证同一时刻最多一条。
func (r *QQBindRepo) GetPendingBindCode(ctx context.Context, qqUIN int64) (code string, expiresAt time.Time, found bool, err error) {
	const q = `SELECT code, expires_at FROM qq_bind_codes WHERE qq_uin = $1 AND status = 0 AND expires_at > now() ORDER BY created_at DESC LIMIT 1`
	var c string
	var e time.Time
	scanErr := r.pool.QueryRow(ctx, q, qqUIN).Scan(&c, &e)
	if scanErr == pgx.ErrNoRows {
		return "", time.Time{}, false, nil
	}
	if scanErr != nil {
		return "", time.Time{}, false, fmt.Errorf("查询待使用绑定码失败: %w", scanErr)
	}
	return c, e, true, nil
}

// CreateBindCode 写入一条待使用绑定码（status=0）。
// 处理两类唯一约束冲突：
//   - ux_qq_bind_codes_code：码碰撞，重新生成并重试（最多 maxRetries 次）。
//   - ux_qq_bind_codes_pending_qq：并发插入已存在待使用码，回退为查询现有码（幂等取/建）。
//
// 码的唯一性与“同一 qq_uin 同时仅一条待使用码”均由数据库兜底，不依赖应用层先查再写。
func (r *QQBindRepo) CreateBindCode(ctx context.Context, qqUIN int64, botID, traceID string, expiresAt time.Time) (string, error) {
	const maxRetries = 8
	const q = `INSERT INTO qq_bind_codes (code, bot_id, qq_uin, trace_id, status, expires_at) VALUES ($1, $2, $3, $4, 0, $5)`

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		code, err := generateBindCode()
		if err != nil {
			return "", fmt.Errorf("生成绑定码随机值失败: %w", err)
		}
		_, err = r.pool.Exec(ctx, q, code, botID, qqUIN, traceID, expiresAt)
		if err == nil {
			return code, nil
		}

		pgErr, ok := err.(*pgconn.PgError)
		if !ok {
			return "", fmt.Errorf("写入绑定码失败: %w", err)
		}
		switch pgErr.ConstraintName {
		case "ux_qq_bind_codes_code":
			// 码冲突，重试生成。
			lastErr = err
			continue
		case "ux_qq_bind_codes_pending_qq":
			// 并发场景下另一请求已写入待使用码：回退为查询现有码。
			existing, _, found, qErr := r.GetPendingBindCode(ctx, qqUIN)
			if qErr != nil {
				return "", qErr
			}
			if found {
				return existing, nil
			}
			// 极少见：冲突发生在刚过期窗口，交由上层重试。
			return "", fmt.Errorf("并发绑定码唯一约束冲突且无待使用码: %w", err)
		default:
			return "", fmt.Errorf("写入绑定码失败: %w", err)
		}
	}
	return "", fmt.Errorf("绑定码碰撞重试耗尽: %w", lastErr)
}

// generateBindCode 使用密码学安全随机数生成 8 位绑定码，字符集排除 0/O/1/I/L。
func generateBindCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = bindCodeAlphabet[int(b[i])%len(bindCodeAlphabet)]
	}
	return string(out), nil
}
