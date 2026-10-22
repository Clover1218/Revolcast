package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUsernameTaken 用户名唯一约束冲突（ux_users_username_lower）。
// 由 repository 层定义，biz 层据 errors.Is 映射为业务错误。
var ErrUsernameTaken = errors.New("username taken")

// ErrUserNotFound 用户名不存在（登录等场景用于友好映射，不直接暴露账号是否存在）。
var ErrUserNotFound = errors.New("user not found")

// UserRecord 是 users 表中登录等业务所需的精简视图。
type UserRecord struct {
	ID           int64
	PasswordHash string
	Status       int16
}

// UserRepo 封装 users 表的访问。
// 库表结构以 docs/Revolcast_数据库表设计文档_v1.1.md §3.1 为准。
type UserRepo struct {
	pool *pgxpool.Pool
}

func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{pool: pool}
}

// ExistsByUsername 判断用户名是否已存在。
// 查询以 lower(username) = lower($1) 进行大小写不敏感比较，并命中 ux_users_username_lower 索引；
// 因此调用方可传入保留原始大小写的用户名，重复检测仍统一按小写判定。
func (r *UserRepo) ExistsByUsername(ctx context.Context, username string) (bool, error) {
	const q = `SELECT 1 FROM users WHERE lower(username) = lower($1) LIMIT 1`
	var dummy int
	err := r.pool.QueryRow(ctx, q, username).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询用户名失败: %w", err)
	}
	return true, nil
}

// GetByID 按账号 ID 查用户（绑定等已认证场景用）。
// 不存在返回 ErrUserNotFound；返回 status 供 biz 层做账号禁用校验。
func (r *UserRepo) GetByID(ctx context.Context, id int64) (*UserRecord, error) {
	const q = `SELECT id, password_hash, status FROM users WHERE id = $1 LIMIT 1`
	var rec UserRecord
	err := r.pool.QueryRow(ctx, q, id).Scan(&rec.ID, &rec.PasswordHash, &rec.Status)
	if err == pgx.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	return &rec, nil
}

// GetByUsername 按用户名查用户（登录等业务用）。
// 以 lower(username) = lower($1) 大小写不敏感匹配；不存在返回 ErrUserNotFound。
// 返回 id、password_hash、status，供 biz 层做 bcrypt 比对与状态校验。
func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*UserRecord, error) {
	const q = `SELECT id, password_hash, status FROM users WHERE lower(username) = lower($1) LIMIT 1`
	var rec UserRecord
	err := r.pool.QueryRow(ctx, q, username).Scan(&rec.ID, &rec.PasswordHash, &rec.Status)
	if err == pgx.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	return &rec, nil
}

// CreateUser 写入新账号（username 为调用方裁剪后的原始大小写值、password_hash、status=1）。
// 库表唯一索引 ux_users_username_lower 以 lower(username) 兜底大小写不敏感去重；created_at / updated_at 由数据库默认值填充。
// 唯一约束冲突（ux_users_username_lower）返回 ErrUsernameTaken，作为并发兜底的权威信号。
func (r *UserRepo) CreateUser(ctx context.Context, username, passwordHash string) error {
	const q = `INSERT INTO users (username, password_hash, status) VALUES ($1, $2, 1)`
	_, err := r.pool.Exec(ctx, q, username, passwordHash)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "ux_users_username_lower" {
		return ErrUsernameTaken
	}
	return fmt.Errorf("写入用户失败: %w", err)
}
