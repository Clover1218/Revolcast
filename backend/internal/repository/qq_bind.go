package repository

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bindCodeAlphabet 是绑定码字符集，排除易混淆字符 0/O/1/I/L。
// 长度为 32，便于用单字节随机值取模得到均匀分布。
const bindCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// 仓库层哨兵错误：由 biz 层据 errors.Is 映射为业务错误（避免 repository 反向依赖 biz 造成循环引用）。
var (
	// ErrBindCodeInvalid 绑定码无效（不存在 / 已使用 / 已过期 / 格式不符）。
	ErrBindCodeInvalid = errors.New("bind code invalid")
	// ErrUserAlreadyBound 该账号已有 active qq_bindings，需先解绑。
	ErrUserAlreadyBound = errors.New("user already bound")
	// ErrQQAlreadyBound 该 QQ 已有 active qq_bindings，需先解绑。
	ErrQQAlreadyBound = errors.New("qq already bound")
	// ErrNotBound 该账号当前没有有效（status=1）的 qq_bindings，无绑定可解。
	ErrNotBound = errors.New("not bound")
)

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
//
// 插入前先回收该 qq_uin 已过期但仍占用「待使用」槽位的旧码（见 reclaimExpiredPending），
// 否则部分唯一索引 ux_qq_bind_codes_pending_qq 会永久挡住新码插入。
//
// 处理两类唯一约束冲突：
//   - code 唯一（qq_bind_codes_code_key / ux_qq_bind_codes_code）：码碰撞，重新生成并重试（最多 maxRetries 次）。
//   - ux_qq_bind_codes_pending_qq：并发插入已存在待使用码，回退为查询现有码（幂等取/建）；
//     若查询不到（极端竞态：回收与插入之间旧码状态变化），重试本轮。
//
// 码的唯一性与“同一 qq_uin 同时仅一条待使用码”均由数据库兜底，不依赖应用层先查再写。
func (r *QQBindRepo) CreateBindCode(ctx context.Context, qqUIN int64, botID, traceID string, expiresAt time.Time) (string, error) {
	const maxRetries = 8
	const q = `INSERT INTO qq_bind_codes (code, bot_id, qq_uin, trace_id, status, expires_at) VALUES ($1, $2, $3, $4, 0, $5)`

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		// 0) 回收过期待使用码：把 status=0 且已过期的旧行标记为 status=2，
		//    释放该 qq_uin 在 ux_qq_bind_codes_pending_qq（谓词仅 status=0）中的唯一槽位。
		if err := r.reclaimExpiredPending(ctx, qqUIN); err != nil {
			return "", err
		}

		code, err := generateBindCode()
		if err != nil {
			return "", fmt.Errorf("生成绑定码随机值失败: %w", err)
		}
		_, err = r.pool.Exec(ctx, q, code, botID, qqUIN, traceID, expiresAt)
		if err == nil {
			return code, nil
		}

		switch {
		case uniqueViolationOn(err, "qq_bind_codes_code_key", "ux_qq_bind_codes_code"):
			// 码冲突，重试生成。
			lastErr = err
			continue
		case uniqueViolationOn(err, "ux_qq_bind_codes_pending_qq"):
			// 并发场景下另一请求已写入待使用码：回退为查询现有码。
			existing, _, found, qErr := r.GetPendingBindCode(ctx, qqUIN)
			if qErr != nil {
				return "", qErr
			}
			if found {
				return existing, nil
			}
			// 极端：回收后仍冲突（旧码在回收与插入之间刚过期/刚被消费），重试整轮。
			lastErr = err
			continue
		default:
			return "", fmt.Errorf("写入绑定码失败: %w", err)
		}
	}
	return "", fmt.Errorf("绑定码碰撞重试耗尽: %w", lastErr)
}

// reclaimExpiredPending 把该 qq_uin 已过期但仍待使用（status=0）的旧码标记为已过期（status=2）。
//
// 必要性：部分唯一索引 ux_qq_bind_codes_pending_qq 的谓词是 `WHERE status = 0`，
// 谓词不能写 now()（非 immutable，PostgreSQL 不允许建含 now() 的索引），
// 因此“过期但 status 仍为 0”的行会一直占住该 qq_uin 的唯一槽位。
// 若不回收：查询侧（GetPendingBindCode 带 expires_at > now()）认为没有待使用码，
// 插入侧却撞唯一约束，最终回退查询仍查不到 → 用户看到“生成绑定码失败”。
//
// status=2 仅用于清理/展示，不参与安全判定（数据库表设计文档 §3.3）；过期码本就不可兑换。
func (r *QQBindRepo) reclaimExpiredPending(ctx context.Context, qqUIN int64) error {
	const q = `UPDATE qq_bind_codes SET status = 2 WHERE qq_uin = $1 AND status = 0 AND expires_at <= now()`
	if _, err := r.pool.Exec(ctx, q, qqUIN); err != nil {
		return fmt.Errorf("回收过期绑定码失败: %w", err)
	}
	return nil
}

// uniqueViolationOn 判断 err 是否为唯一约束冲突（SQLSTATE 23505）且约束名命中 names 之一。
// 使用 errors.As 而非直接类型断言，避免错误被包裹后漏判。
func uniqueViolationOn(err error, names ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	for _, n := range names {
		if pgErr.ConstraintName == n {
			return true
		}
	}
	return false
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

// ConsumeBindCodeAndBind 在单个事务内原子地完成「消费绑定码 + 写入绑定关系」：
//  1. 原子消费绑定码：将 status 由 0(待使用) 置为 1(已使用)，记录 used_by_user_id / used_at；
//     条件 `status=0 AND expires_at>now()` 保证并发下仅一个请求能成功消费（0 行 → ErrBindCodeInvalid）。
//  2. 预检：该 user_id 已有 active 绑定 → ErrUserAlreadyBound；该 qq_uin 已被其他账号 active 绑定 → ErrQQAlreadyBound。
//  3. 写入 qq_bindings(status=1)；并发唯一索引 ux_qq_bindings_active_user / ux_qq_bindings_active_qq
//     冲突时回滚并映射为对应错误。
//
// 绑定码的状态语义以数据库表设计文档 §3.3 为准：0=待使用，1=已使用。用户特别强调消费后置为 1。
func (r *QQBindRepo) ConsumeBindCodeAndBind(ctx context.Context, code string, userID int64) (qqUIN int64, botID string, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("开启事务失败: %w", err)
	}
	// 仅在出错时回滚；成功路径显式 Commit 后此 defer 的 Rollback 为 no-op。
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// 1. 原子消费绑定码：成功后 status=1(已使用)，并带回 qq_uin / bot_id 用于后续写入绑定关系。
	const consumeQ = `UPDATE qq_bind_codes SET status = 1, used_by_user_id = $2, used_at = now() WHERE code = $1 AND status = 0 AND expires_at > now() RETURNING qq_uin, bot_id`
	var gotUIN int64
	var gotBotID string
	scanErr := tx.QueryRow(ctx, consumeQ, code, userID).Scan(&gotUIN, &gotBotID)
	if scanErr == pgx.ErrNoRows {
		return 0, "", ErrBindCodeInvalid
	}
	if scanErr != nil {
		return 0, "", fmt.Errorf("消费绑定码失败: %w", scanErr)
	}

	// 2a. 预检：该 user_id 是否已存在 active 绑定。
	const userBoundQ = `SELECT 1 FROM qq_bindings WHERE user_id = $1 AND status = 1 LIMIT 1`
	var dummy int
	if qErr := tx.QueryRow(ctx, userBoundQ, userID).Scan(&dummy); qErr != pgx.ErrNoRows {
		if qErr == nil {
			return 0, "", ErrUserAlreadyBound
		}
		return 0, "", fmt.Errorf("查询用户绑定状态失败: %w", qErr)
	}

	// 2b. 预检：该 qq_uin 是否已被其他账号 active 绑定。
	const qqBoundQ = `SELECT 1 FROM qq_bindings WHERE qq_uin = $1 AND status = 1 LIMIT 1`
	if qErr := tx.QueryRow(ctx, qqBoundQ, gotUIN).Scan(&dummy); qErr != pgx.ErrNoRows {
		if qErr == nil {
			return 0, "", ErrQQAlreadyBound
		}
		return 0, "", fmt.Errorf("查询 QQ 绑定状态失败: %w", qErr)
	}

	// 3. 写入绑定关系（status=1 有效）。唯一索引作为并发兜底。
	const insertQ = `INSERT INTO qq_bindings (user_id, qq_uin, bot_id, status, bound_at, created_at, updated_at) VALUES ($1, $2, $3, 1, now(), now(), now())`
	if _, err = tx.Exec(ctx, insertQ, userID, gotUIN, gotBotID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.ConstraintName {
			case "ux_qq_bindings_active_user":
				return 0, "", ErrUserAlreadyBound
			case "ux_qq_bindings_active_qq":
				return 0, "", ErrQQAlreadyBound
			}
		}
		return 0, "", fmt.Errorf("写入 QQ 绑定失败: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, "", fmt.Errorf("提交事务失败: %w", err)
	}
	return gotUIN, gotBotID, nil
}

// UnbindByUser 软解绑该账号当前的有效绑定（status 1→0），并记录 unbound_at。
//
// 数据库表设计文档 §3.2 规则要求「解绑时更新当前行状态并设置 unbound_at，保留历史记录」，
// 因此不做硬删：历史行保留用于审计，部分唯一索引 ux_qq_bindings_active_user 仅约束 status=1，
// 解绑后该账号即可重新绑定。
//
// 单条 UPDATE 本身即原子操作，无需显式事务；并发下（同一账号同时发两次解绑）
// 仅一个请求命中 status=1，其余返回 ErrNotBound。
// 返回值 qqUIN / botID 供调用方记录日志或后续扩展（本期业务层不使用，不做 QQ 通知）。
func (r *QQBindRepo) UnbindByUser(ctx context.Context, userID int64) (qqUIN int64, botID string, err error) {
	const q = `UPDATE qq_bindings SET status = 0, unbound_at = now(), updated_at = now() WHERE user_id = $1 AND status = 1 RETURNING qq_uin, bot_id`
	var gotUIN int64
	var gotBotID string
	scanErr := r.pool.QueryRow(ctx, q, userID).Scan(&gotUIN, &gotBotID)
	if scanErr == pgx.ErrNoRows {
		return 0, "", ErrNotBound
	}
	if scanErr != nil {
		return 0, "", fmt.Errorf("解绑 QQ 失败: %w", scanErr)
	}
	return gotUIN, gotBotID, nil
}
