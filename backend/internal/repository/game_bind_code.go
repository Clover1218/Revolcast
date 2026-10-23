package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GameBindCodeRepo 封装 game_bind_codes 的访问（游戏角色绑定码）。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.6 为准。
//
// 状态语义：0=待使用，1=已使用，2=已过期，3=已取消。
// 本期只负责「发码」（status=0）；赎回（0→1 + 回填 game_name/game_uuid +
// 写 server_game_bindings）属于 BIZ-010，由游戏服务端侧接口实现，不在本仓储内。
type GameBindCodeRepo struct {
	pool *pgxpool.Pool
}

func NewGameBindCodeRepo(pool *pgxpool.Pool) *GameBindCodeRepo {
	return &GameBindCodeRepo{pool: pool}
}

// GetPendingGameBindCode 返回 (server_id, user_id) 当前未过期、待使用（status=0 且 expires_at>now()）的绑定码。
// 由部分唯一索引 ux_game_bind_codes_pending_user(server_id, user_id) WHERE status=0 保证同一时刻最多一条。
func (r *GameBindCodeRepo) GetPendingGameBindCode(ctx context.Context, serverID, userID int64) (code string, expiresAt time.Time, found bool, err error) {
	const q = `SELECT code, expires_at FROM game_bind_codes
	           WHERE server_id = $1 AND user_id = $2 AND status = 0 AND expires_at > now()
	           ORDER BY created_at DESC LIMIT 1`
	var c string
	var e time.Time
	scanErr := r.pool.QueryRow(ctx, q, serverID, userID).Scan(&c, &e)
	if scanErr == pgx.ErrNoRows {
		return "", time.Time{}, false, nil
	}
	if scanErr != nil {
		return "", time.Time{}, false, fmt.Errorf("查询待使用游戏角色绑定码失败: %w", scanErr)
	}
	return c, e, true, nil
}

// CreateGameBindCode 写入一条待使用游戏角色绑定码（status=0）。
//
// 与 qq_bind_codes 的取/建完全同构：
//   - 插入前先回收该 (server_id, user_id) 已过期但仍占用「待使用」槽位的旧码（见 reclaimExpiredPending），
//     否则部分唯一索引 ux_game_bind_codes_pending_user 会永久挡住新码插入；
//   - 码唯一冲突（ux_game_bind_codes_code）→ 重新生成并重试；
//   - 待使用槽位冲突（ux_game_bind_codes_pending_user）→ 回退查询现有码，实现并发幂等。
//
// qqUIN 仅作审计快照写入（可为 0 表示未知），不作为权限依据。
func (r *GameBindCodeRepo) CreateGameBindCode(ctx context.Context, serverID, userID, qqUIN int64, traceID string, expiresAt time.Time) (string, error) {
	const maxRetries = 8
	const q = `INSERT INTO game_bind_codes (code, server_id, user_id, qq_uin, trace_id, status, expires_at)
	           VALUES ($1, $2, $3, $4, $5, 0, $6)`

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		// 0) 回收过期待使用码：status 0→2，释放唯一槽位（谓词只认 status=0，不含 now()）。
		if err := r.reclaimExpiredPending(ctx, serverID, userID); err != nil {
			return "", err
		}

		code, err := generateBindCode()
		if err != nil {
			return "", fmt.Errorf("生成游戏角色绑定码随机值失败: %w", err)
		}
		_, err = r.pool.Exec(ctx, q, code, serverID, userID, qqUIN, traceID, expiresAt)
		if err == nil {
			return code, nil
		}

		switch {
		case uniqueViolationOn(err, "game_bind_codes_code_key", "ux_game_bind_codes_code"):
			// 码碰撞，重试生成。
			lastErr = err
			continue
		case uniqueViolationOn(err, "ux_game_bind_codes_pending_user"):
			// 并发场景下另一请求已写入待使用码：回退为查询现有码。
			existing, _, found, qErr := r.GetPendingGameBindCode(ctx, serverID, userID)
			if qErr != nil {
				return "", qErr
			}
			if found {
				return existing, nil
			}
			// 极端竞态（回收与插入之间旧码状态变化）：重试整轮。
			lastErr = err
			continue
		default:
			return "", fmt.Errorf("写入游戏角色绑定码失败: %w", err)
		}
	}
	return "", fmt.Errorf("游戏角色绑定码碰撞重试耗尽: %w", lastErr)
}

// reclaimExpiredPending 把该 (server_id, user_id) 已过期但仍待使用（status=0）的旧码标记为已过期（status=2）。
//
// 必要性同 qq_bind_codes（见 docs 与 qq_bind.go）：部分唯一索引谓词只能是 status=0，
// 过期但状态未变的行会一直占住槽位，导致「查询侧查不到、插入侧撞唯一约束」的死局。
func (r *GameBindCodeRepo) reclaimExpiredPending(ctx context.Context, serverID, userID int64) error {
	const q = `UPDATE game_bind_codes SET status = 2
	           WHERE server_id = $1 AND user_id = $2 AND status = 0 AND expires_at <= now()`
	if _, err := r.pool.Exec(ctx, q, serverID, userID); err != nil {
		return fmt.Errorf("回收过期游戏角色绑定码失败: %w", err)
	}
	return nil
}
