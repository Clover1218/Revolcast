package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
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

// ServerGameBindingRepo 封装 server_game_bindings 的只读访问。
// 本期只需要「判断某账号在某服务器是否已有有效角色绑定」；
// 写入（赎回时创建绑定、visibility 设置）属于 BIZ-010，不在本仓储内。
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
