package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ServerGameBindingRecord 是 server_game_bindings 的有效绑定快照。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.7 为准。
type ServerGameBindingRecord struct {
	ID         int64
	ServerID   int64
	UserID     int64
	GameName   string // 经游戏内验证的角色名（展示用）
	GameUUID   string // 角色 UUID（身份标识，以 text 形式返回）
	Visibility int16  // 1=公开，0=隐身
	Status     int16  // 1=有效，0=已解绑
}

// GameRoleBindOutcome 是「赎回绑定码并创建角色绑定」事务的业务结论。
//
// 这些是用户可预期的正常分支，因此用结果对象表达而不是 error：
// 调用方据 outcome 渲染游戏内文案，只有真正的数据库异常才返回 error。
type GameRoleBindOutcome string

const (
	// GameRoleBindOK 新绑定成功。
	GameRoleBindOK GameRoleBindOutcome = "ok"
	// GameRoleBindAlreadyBound 同一绑定码 + 同一角色已成功绑定过（重复输入 / 消息重投，幂等）。
	GameRoleBindAlreadyBound GameRoleBindOutcome = "already_bound"
	// GameRoleBindCodeInvalid 码不存在 / 已使用（不同角色）/ 已过期 / 不属于该服务器。
	GameRoleBindCodeInvalid GameRoleBindOutcome = "code_invalid"
	// GameRoleBindAccountBound 该账号在该服务器已有有效角色绑定。
	GameRoleBindAccountBound GameRoleBindOutcome = "account_bound"
	// GameRoleBindRoleTaken 该角色（UUID 或角色名）已被其他账号占用。
	GameRoleBindRoleTaken GameRoleBindOutcome = "role_taken"
	// GameRoleBindServerUnavailable 目标服务器不存在或已停用。
	GameRoleBindServerUnavailable GameRoleBindOutcome = "server_unavailable"
	// GameRoleBindAccountDisabled 绑定码指向的账号不存在或已被禁用。
	GameRoleBindAccountDisabled GameRoleBindOutcome = "account_disabled"
)

// GameRoleBindInput 是赎回入参。Code / GameUUID 已由 biz 层归一化与格式校验，
// ServerID / GameName / GameUUID 均来自已鉴权的 WS 连接与事件体，不接受客户端自报。
type GameRoleBindInput struct {
	Code     string // 8 位绑定码（已 TOUPPER）
	ServerID int64  // 事件来源服务器（已鉴权 game_servers.id）
	GameName string // 玩家昵称快照
	GameUUID string // 玩家 UUID（合法 UUID 字符串）
}

// GameRoleBindResult 是事务结果。Outcome 之外的字段用于文案渲染与日志。
type GameRoleBindResult struct {
	Outcome GameRoleBindOutcome
	// BindingID 绑定成功 / 幂等命中时的 server_game_bindings.id。
	BindingID int64
	// CodeID 被消费的 game_bind_codes.id（审计用）。
	CodeID int64
	// UserID 绑定码所属账号。
	UserID int64
	// QQUIN 申请绑定码时的 QQ 号快照（可能为 0 表示未知）。
	QQUIN int64
	// GameName 绑定成功时的角色名；AccountBound 时为已存在的角色名。
	GameName string
	// ServerCode / ServerName 目标服务器（用于游戏内文案回显）。
	ServerCode string
	ServerName string
}

// ServerGameBindingRepo 封装 server_game_bindings 的读写访问。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.7 为准。
//
// 状态语义：1=有效，0=已解绑（软解绑，保留历史行）。
type ServerGameBindingRepo struct {
	pool *pgxpool.Pool
}

func NewServerGameBindingRepo(pool *pgxpool.Pool) *ServerGameBindingRepo {
	return &ServerGameBindingRepo{pool: pool}
}

// FindActive 查询 (server_id, user_id) 当前有效（status=1）的角色绑定。
// 无记录返回 (nil, nil)，由调用方按「未绑定」处理；DB 错误返回错误。
//
// 依赖部分唯一索引 ux_server_game_bindings_active_user(server_id, user_id) WHERE status=1，
// 因此最多返回一行；仍带 LIMIT 1 以避免索引缺失时误返回多行。
func (r *ServerGameBindingRepo) FindActive(ctx context.Context, serverID, userID int64) (*ServerGameBindingRecord, error) {
	const q = `SELECT id, server_id, user_id, game_name, game_uuid::text, visibility, status
	           FROM server_game_bindings
	           WHERE server_id = $1 AND user_id = $2 AND status = 1
	           LIMIT 1`
	var rec ServerGameBindingRecord
	err := r.pool.QueryRow(ctx, q, serverID, userID).Scan(
		&rec.ID, &rec.ServerID, &rec.UserID, &rec.GameName, &rec.GameUUID, &rec.Visibility, &rec.Status,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询服务器角色绑定失败: %w", err)
	}
	return &rec, nil
}

// RedeemGameBindCode 在单个事务内完成「消费游戏角色绑定码 + 写入角色绑定」（BIZ-010）。
//
// 事务步骤（顺序本身是设计的一部分）：
//  1. 读目标服务器（status / code / name）——停用即拒绝，避免给已下线服务器建绑定；
//  2. `SELECT ... FOR UPDATE` 锁定待使用码（status=0 且未过期）；
//     未命中时做一次幂等重放判定（同码 + 同 UUID 已绑定成功 → already_bound）；
//  3. 账号状态（users.status != 1 → account_disabled）；
//  4. 该账号在该服务器是否已有有效角色（→ account_bound，回显已有角色名）；
//  5. 该角色（UUID 或 lower(game_name)）是否已被其他账号占用（→ role_taken）；
//  6. 消费绑定码并回填 game_name / game_uuid；
//  7. 写入 server_game_bindings（visibility 默认 1，status=1）。
//
// 并发保证：
//   - `FOR UPDATE` 使同一码的并发兑换串行化，后到者在 status=0 谓词上得到 0 行；
//   - 步骤 7 的唯一索引（active_user / active_uuid / active_name）作为最后兜底，
//     冲突时整个事务回滚——绑定码状态一并恢复为 0，用户可换角色重试同一码。
//
// 返回 error 仅表示数据库异常（调用方映射为 ErrInternal）；业务结论一律在 Result.Outcome 中。
func (r *ServerGameBindingRepo) RedeemGameBindCode(ctx context.Context, in GameRoleBindInput) (res *GameRoleBindResult, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启绑定事务失败: %w", err)
	}
	// 只有成功 Commit 才视为结束；任何早退（含业务结论）都回滚，保证不留半成品。
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	res = &GameRoleBindResult{Outcome: GameRoleBindOK}

	// 1) 服务器上下文：同时拿到 code / name 供文案回显。
	var serverStatus int16
	scanErr := tx.QueryRow(ctx,
		`SELECT status, code, name FROM game_servers WHERE id = $1`, in.ServerID,
	).Scan(&serverStatus, &res.ServerCode, &res.ServerName)
	if scanErr == pgx.ErrNoRows {
		res.Outcome = GameRoleBindServerUnavailable
		return res, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("查询目标服务器失败: %w", scanErr)
	}
	if serverStatus != 1 {
		res.Outcome = GameRoleBindServerUnavailable
		return res, nil
	}

	// 2) 锁定待使用绑定码。
	// qq_uin 可空，用 COALESCE 归零：它只是审计快照，不需要区分「NULL」与「未知(0)」，
	// 同时避免扫描 NULL 到基础类型时的指针处理。
	var codeID, userID, qqUIN int64
	scanErr = tx.QueryRow(ctx,
		`SELECT id, user_id, COALESCE(qq_uin, 0) FROM game_bind_codes
		  WHERE code = $1 AND server_id = $2 AND status = 0 AND expires_at > now()
		  FOR UPDATE`,
		in.Code, in.ServerID,
	).Scan(&codeID, &userID, &qqUIN)
	if scanErr == pgx.ErrNoRows {
		// 2b) 幂等重放：该码已被使用，且使用它的绑定就是同一个角色。
		var boundID, boundUserID int64
		var boundName string
		idemErr := tx.QueryRow(ctx,
			`SELECT b.id, b.user_id, b.game_name
			   FROM game_bind_codes c
			   JOIN server_game_bindings b ON b.bind_code_id = c.id AND b.status = 1
			  WHERE c.code = $1 AND c.server_id = $2 AND c.status = 1 AND b.game_uuid = $3
			  LIMIT 1`,
			in.Code, in.ServerID, in.GameUUID,
		).Scan(&boundID, &boundUserID, &boundName)
		switch {
		case idemErr == nil:
			res.Outcome = GameRoleBindAlreadyBound
			res.BindingID = boundID
			res.UserID = boundUserID
			res.GameName = boundName
		case errors.Is(idemErr, pgx.ErrNoRows):
			res.Outcome = GameRoleBindCodeInvalid
		default:
			return nil, fmt.Errorf("查询绑定码幂等状态失败: %w", idemErr)
		}
		return res, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("查询游戏角色绑定码失败: %w", scanErr)
	}
	res.CodeID = codeID
	res.UserID = userID
	res.QQUIN = qqUIN

	// 3) 账号状态：码指向的账号不存在或已禁用，都不允许建立绑定。
	var userStatus int16
	scanErr = tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&userStatus)
	if scanErr == pgx.ErrNoRows {
		res.Outcome = GameRoleBindAccountDisabled
		return res, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("查询账号状态失败: %w", scanErr)
	}
	if userStatus != 1 {
		res.Outcome = GameRoleBindAccountDisabled
		return res, nil
	}

	// 4) 该账号在该服务器已有有效角色。
	var existingName string
	scanErr = tx.QueryRow(ctx,
		`SELECT game_name FROM server_game_bindings
		  WHERE server_id = $1 AND user_id = $2 AND status = 1 LIMIT 1`,
		in.ServerID, userID,
	).Scan(&existingName)
	switch {
	case scanErr == nil:
		res.Outcome = GameRoleBindAccountBound
		res.GameName = existingName
		return res, nil
	case !errors.Is(scanErr, pgx.ErrNoRows):
		return nil, fmt.Errorf("查询账号角色绑定失败: %w", scanErr)
	}

	// 5) 角色是否已被其他账号占用（UUID 或角色名，名字按大小写不敏感）。
	var ownerID int64
	var ownerName string
	scanErr = tx.QueryRow(ctx,
		`SELECT user_id, game_name FROM server_game_bindings
		  WHERE server_id = $1 AND status = 1
		    AND (game_uuid = $2 OR lower(game_name) = lower($3))
		  LIMIT 1`,
		in.ServerID, in.GameUUID, in.GameName,
	).Scan(&ownerID, &ownerName)
	switch {
	case scanErr == nil:
		if ownerID == userID {
			// 步骤 4 已拦截；这里兜底，避免出现「自己占自己」的奇怪文案。
			res.Outcome = GameRoleBindAccountBound
			res.GameName = ownerName
		} else {
			res.Outcome = GameRoleBindRoleTaken
		}
		return res, nil
	case !errors.Is(scanErr, pgx.ErrNoRows):
		return nil, fmt.Errorf("查询角色占用情况失败: %w", scanErr)
	}

	// 6) 消费绑定码并回填角色快照（game_name / game_uuid 是本次校验结果的快照）。
	if _, err = tx.Exec(ctx,
		`UPDATE game_bind_codes
		    SET status = 1, used_at = now(), game_name = $2, game_uuid = $3
		  WHERE id = $1`,
		codeID, in.GameName, in.GameUUID,
	); err != nil {
		return nil, fmt.Errorf("消费游戏角色绑定码失败: %w", err)
	}

	// 7) 写入角色绑定。唯一索引作为并发兜底：冲突即回滚整个事务。
	var bindingID int64
	insErr := tx.QueryRow(ctx,
		`INSERT INTO server_game_bindings
		   (server_id, user_id, game_name, game_uuid, bind_code_id, visibility, status, bound_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 1, 1, now(), now(), now())
		 RETURNING id`,
		in.ServerID, userID, in.GameName, in.GameUUID, codeID,
	).Scan(&bindingID)
	if insErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(insErr, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "ux_server_game_bindings_active_user":
				res.Outcome = GameRoleBindAccountBound
				return res, nil
			case "ux_server_game_bindings_active_uuid", "ux_server_game_bindings_active_name":
				res.Outcome = GameRoleBindRoleTaken
				return res, nil
			}
		}
		return nil, fmt.Errorf("写入角色绑定失败: %w", insErr)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交绑定事务失败: %w", err)
	}
	committed = true

	res.BindingID = bindingID
	res.GameName = in.GameName
	return res, nil
}

// UnbindActive 软解绑 (server_id, user_id) 当前有效角色绑定（status 1→0 + unbound_at）。//
// 数据库文档 §3.7 要求保留历史行（审计），因此不删除记录；部分唯一索引只约束 status=1，
// 解绑后该账号可在该服务器重新绑定（写入新行）。
//
// 单条 UPDATE 本身即原子，无需显式事务；并发重复解绑只有一个请求命中，其余返回 (nil, nil)。
// 返回 nil 表示当前没有可解绑的有效绑定，由调用方按「无需解绑」处理。
func (r *ServerGameBindingRepo) UnbindActive(ctx context.Context, serverID, userID int64) (*ServerGameBindingRecord, error) {
	const q = `UPDATE server_game_bindings
	              SET status = 0, unbound_at = now(), updated_at = now()
	            WHERE server_id = $1 AND user_id = $2 AND status = 1
	            RETURNING id, server_id, user_id, game_name, game_uuid::text, visibility, status`
	var rec ServerGameBindingRecord
	err := r.pool.QueryRow(ctx, q, serverID, userID).Scan(
		&rec.ID, &rec.ServerID, &rec.UserID, &rec.GameName, &rec.GameUUID, &rec.Visibility, &rec.Status,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("解除角色绑定失败: %w", err)
	}
	return &rec, nil
}
