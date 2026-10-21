package biz

import "errors"

// 业务错误类型。Handler 据这些错误决定回传给用户的文案与协议错误码，
// 不得仅凭错误文本判断分支（见 WebSocket 消息结构设计文档 §4）。
var (
	// ErrQQAlreadyBound 该 QQ 已有 active qq_bindings，需先解绑。
	ErrQQAlreadyBound = errors.New("qq already bound")
	// ErrRateLimited 命中 Redis 限流。
	ErrRateLimited = errors.New("rate limited")
	// ErrInternal 数据库写入失败、碰撞重试耗尽等内部错误。
	ErrInternal = errors.New("internal error")
	// ErrUsernameTaken 用户名已被占用（应用层预检或数据库唯一约束兜底）。
	ErrUsernameTaken = errors.New("username taken")
	// ErrInvalidParam 请求参数校验失败（缺字段、长度/格式不符）。
	ErrInvalidParam = errors.New("invalid param")
)
