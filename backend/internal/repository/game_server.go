package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gameServersColumns 是 game_servers 表查询/返回的稳定列清单（不含敏感原始 secret，仅含其哈希）。
const gameServersColumns = `id, code, name, description, api_secret_hash, status, created_at, updated_at`

// 仓库层哨兵错误：由 biz 层据 errors.Is 映射为业务错误（避免 repository 反向依赖 biz 造成循环引用）。
var (
	// ErrServerCodeTaken 服务器标识（code）唯一约束冲突（game_servers_code_key）。
	ErrServerCodeTaken = errors.New("server code taken")
	// ErrServerNotFound 指定的游戏服务器不存在（按 code 查询无记录）。
	ErrServerNotFound = errors.New("server not found")
)

// GameServerRecord 是 game_servers 表的精简视图。
// Description 可空，以 *string 表达 NULL（nil 表示无描述）。
type GameServerRecord struct {
	ID            int64
	Code          string
	Name          string
	Description   *string
	APISecretHash string
	Status        int16
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// GameServerUpdate 是 PATCH 更新的可选字段集合（指针表达「未提供」与「提供空值」的区别）。
type GameServerUpdate struct {
	Name        *string
	Description *string // 非 nil 即更新（空字符串表示清空为 NULL）
	Status      *int16  // 仅 0/1
}

// GameServerRepo 封装 game_servers 表的访问。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.4 为准。
type GameServerRepo struct {
	pool *pgxpool.Pool
}

func NewGameServerRepo(pool *pgxpool.Pool) *GameServerRepo {
	return &GameServerRepo{pool: pool}
}

// scanGameServer 从单行扫描出 GameServerRecord；description 的 NULL 映射为 nil。
func scanGameServer(row pgx.Row) (*GameServerRecord, error) {
	var rec GameServerRecord
	var desc sql.NullString
	if err := row.Scan(&rec.ID, &rec.Code, &rec.Name, &desc, &rec.APISecretHash, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	if desc.Valid {
		v := desc.String
		rec.Description = &v
	}
	return &rec, nil
}

// CreateGameServer 写入一条游戏服务器（status=1 启用）。
// description 为空字符串时存 NULL。code 唯一冲突（game_servers_code_key）映射为 ErrServerCodeTaken；
// 非法 code 格式会撞 CHECK 约束（code ~ '^[a-z0-9-]+$'），由调用方（biz 层）先做格式校验，
// 若仍命中则按内部错误返回。
func (r *GameServerRepo) CreateGameServer(ctx context.Context, code, name, description, apiSecretHash string) (*GameServerRecord, error) {
	const q = `INSERT INTO game_servers (code, name, description, api_secret_hash, status) VALUES ($1, $2, $3, $4, 1) RETURNING ` + gameServersColumns
	var desc sql.NullString
	if description != "" {
		desc = sql.NullString{String: description, Valid: true}
	}
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, code, name, desc, apiSecretHash))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "game_servers_code_key" {
			return nil, ErrServerCodeTaken
		}
		return nil, fmt.Errorf("写入游戏服务器失败: %w", err)
	}
	return rec, nil
}

// ListGameServers 分页列出游戏服务器。
// statusFilter 为 nil 时不过滤；否则仅返回对应 status。按 created_at DESC 排序。
// limit/offset 由调用方校验范围（biz 层默认/封顶）。
func (r *GameServerRepo) ListGameServers(ctx context.Context, statusFilter *int16, limit, offset int) ([]*GameServerRecord, error) {
	base := `SELECT ` + gameServersColumns + ` FROM game_servers`
	args := []any{}
	q := base
	if statusFilter != nil {
		q += ` WHERE status = $1`
		args = append(args, *statusFilter)
		q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $2 OFFSET $3`)
		args = append(args, limit, offset)
	} else {
		q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $1 OFFSET $2`)
		args = append(args, limit, offset)
	}

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询游戏服务器列表失败: %w", err)
	}
	defer rows.Close()
	out := []*GameServerRecord{}
	for rows.Next() {
		rec, sErr := scanGameServer(rows)
		if sErr != nil {
			return nil, fmt.Errorf("扫描游戏服务器失败: %w", sErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历游戏服务器失败: %w", err)
	}
	return out, nil
}

// GetGameServerByCode 按 code 查询单个服务器（code 唯一）。不存在返回 ErrServerNotFound。
func (r *GameServerRepo) GetGameServerByCode(ctx context.Context, code string) (*GameServerRecord, error) {
	const q = `SELECT ` + gameServersColumns + ` FROM game_servers WHERE code = $1`
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("查询游戏服务器失败: %w", err)
	}
	return rec, nil
}

// GetGameServerBySecretHash 按 api_secret_hash 查询「启用中」的服务器（status=1）。
// 用于 WS 接入凭证校验：secret 错误、服务器不存在或已停用均返回 ErrServerNotFound，
// 调用方据此统一映射为 401，不暴露具体成因（避免枚举）。
func (r *GameServerRepo) GetGameServerBySecretHash(ctx context.Context, secretHash string) (*GameServerRecord, error) {
	const q = `SELECT ` + gameServersColumns + ` FROM game_servers WHERE api_secret_hash = $1 AND status = 1`
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, secretHash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("按密钥哈希查询游戏服务器失败: %w", err)
	}
	return rec, nil
}

// UpdateGameServer 部分更新服务器的 name/description/status（code 与 api_secret_hash 不可经此修改）。
// 全部字段为 nil 时视为空更新，直接返回当前记录（幂等）。description 非 nil 即更新（空串→NULL）。
// 目标 code 不存在返回 ErrServerNotFound。
func (r *GameServerRepo) UpdateGameServer(ctx context.Context, code string, u GameServerUpdate) (*GameServerRecord, error) {
	sets := []string{}
	args := []any{}
	i := 1
	if u.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", i))
		args = append(args, *u.Name)
		i++
	}
	if u.Description != nil {
		var d sql.NullString
		if *u.Description != "" {
			d = sql.NullString{String: *u.Description, Valid: true}
		}
		sets = append(sets, fmt.Sprintf("description = $%d", i))
		args = append(args, d)
		i++
	}
	if u.Status != nil {
		sets = append(sets, fmt.Sprintf("status = $%d", i))
		args = append(args, *u.Status)
		i++
	}
	if len(sets) == 0 {
		// 无字段可更新：返回当前记录（幂等，不报错）。
		return r.GetGameServerByCode(ctx, code)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, code) // $i 对应 WHERE code
	q := fmt.Sprintf(`UPDATE game_servers SET %s WHERE code = $%d RETURNING `+gameServersColumns, strings.Join(sets, ", "), i)
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("更新游戏服务器失败: %w", err)
	}
	return rec, nil
}

// DisableGameServer 软禁用服务器（status 1→0），保留历史行与所有外键引用（数据库文档 §7.10）。
// 目标不存在返回 ErrServerNotFound；已禁用（status=0）视为幂等成功，返回当前记录。
func (r *GameServerRepo) DisableGameServer(ctx context.Context, code string) (*GameServerRecord, error) {
	const q = `UPDATE game_servers SET status = 0, updated_at = now() WHERE code = $1 AND status = 1 RETURNING ` + gameServersColumns
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, code))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("禁用游戏服务器失败: %w", err)
	}
	// 0 行：要么不存在，要么已禁用。再查一次以区分。
	existing, gErr := r.GetGameServerByCode(ctx, code)
	if gErr != nil {
		return nil, gErr
	}
	// 已禁用（status=0）：幂等成功。
	return existing, nil
}

// RotateGameServerSecret 轮换服务器的 API Secret 哈希。目标不存在返回 ErrServerNotFound。
// 原始 Secret 仅由调用方（biz 层）生成并一次性下发，本方法只持久化哈希。
func (r *GameServerRepo) RotateGameServerSecret(ctx context.Context, code, newAPISecretHash string) (*GameServerRecord, error) {
	const q = `UPDATE game_servers SET api_secret_hash = $2, updated_at = now() WHERE code = $1 RETURNING ` + gameServersColumns
	rec, err := scanGameServer(r.pool.QueryRow(ctx, q, code, newAPISecretHash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("轮换游戏服务器密钥失败: %w", err)
	}
	return rec, nil
}
