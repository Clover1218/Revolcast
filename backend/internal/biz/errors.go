package biz

import (
	"errors"
	"fmt"
	"net/http"
)

// 业务错误类型。Handler 据这些错误决定回传给用户的文案与协议错误码，
// 不得仅凭错误文本判断分支（见 WebSocket 消息结构设计文档 §4）。
//
// 约定：biz 层任何返回业务错误的位置都必须用 Wrap 携带底层根因，
// 禁止裸 return sentinel 吞掉底层原因（见本文件 Wrap 与 AGENTS.md §7）。
var (
	// ErrQQAlreadyBound 该 QQ 已有 active qq_bindings，需先解绑。
	ErrQQAlreadyBound = errors.New("qq already bound")
	// ErrBindCodeInvalid 绑定码无效：不存在 / 已使用 / 已过期 / 格式不符。
	ErrBindCodeInvalid = errors.New("bind code invalid")
	// ErrUserAlreadyBound 该账号已有 active qq_bindings，需先解绑。
	ErrUserAlreadyBound = errors.New("user already bound")
	// ErrNotBound 该账号当前没有有效 QQ 绑定，无绑定可解（解绑接口返回 409）。
	ErrNotBound = errors.New("not bound")
	// ErrRateLimited 命中 Redis 限流。
	ErrRateLimited = errors.New("rate limited")
	// ErrInternal 数据库写入失败、碰撞重试耗尽等内部错误。
	ErrInternal = errors.New("internal error")
	// ErrUsernameTaken 用户名已被占用（应用层预检或数据库唯一约束兜底）。
	ErrUsernameTaken = errors.New("username taken")
	// ErrInvalidParam 请求参数校验失败（缺字段、长度/格式不符）。
	ErrInvalidParam = errors.New("invalid param")
	// ErrInvalidCredentials 凭证错误（用户名不存在或密码错误，两者统一，防枚举）。
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrAccountDisabled 账号不可用（users.status != 1，例如已禁用）。
	ErrAccountDisabled = errors.New("account disabled")
	// ErrNotAuthenticated 未携带有效凭证或无有效会话（登出时 Cookie 缺失 / 会话已撤销或过期）。
	ErrNotAuthenticated = errors.New("not authenticated")

	// ErrServerCodeInvalid 服务器标识（code）格式不合法：非空、仅允许小写字母/数字/连字符、长度 1..64。
	ErrServerCodeInvalid = errors.New("server code invalid")
	// ErrServerNameInvalid 服务器展示名称不合法：非空、长度 1..128。
	ErrServerNameInvalid = errors.New("server name invalid")
	// ErrServerCodeTaken 服务器标识（code）已被占用（UNIQUE(code) 兜底）。
	ErrServerCodeTaken = errors.New("server code taken")
	// ErrServerNotFound 指定的游戏服务器不存在（按 code 查询无记录）。
	ErrServerNotFound = errors.New("server not found")
)

// ErrorMeta 是某个业务哨兵错误对外暴露的元数据，作为 HTTP 与 WS 导出的唯一依据。
// 任何新增业务错误都必须在 errorMeta 中登记，禁止在 transport 层散落硬编码文案 / 状态码。
type ErrorMeta struct {
	// HTTPStatus 对应的 HTTP 状态码（与 HTTP 接口设计文档 §10 对齐）。
	HTTPStatus int
	// HTTPCode 业务 code，与 HTTPStatus 对齐，出现在响应体 envelope.code。
	HTTPCode int
	// HTTPMsg 对外简要说明（不泄露内部细节，不作为业务分支判断依据）。
	HTTPMsg string
	// WSCode 面向 WebSocket 的字符串错误码（见 WebSocket 消息结构设计文档 §4）。
	WSCode string
	// WSMessage 面向 QQ 用户的中文友好提示。
	WSMessage string
}

// errorMeta 是错误码与错误信息的唯一来源（单一映射源）。新增错误请在此登记。
// 其中 WSCode 的取值须与 protocol/qq.go 中定义的 WebSocket 错误码常量保持一致。
var errorMeta = map[error]ErrorMeta{
	ErrInvalidParam: {
		HTTPStatus: http.StatusBadRequest,
		HTTPCode:   400,
		HTTPMsg:    "invalid param",
		WSCode:     "INVALID_PARAM",
		WSMessage:  "请求参数有误，请检查后重试。",
	},
	ErrBindCodeInvalid: {
		HTTPStatus: http.StatusBadRequest,
		HTTPCode:   400,
		HTTPMsg:    "bind code invalid",
		WSCode:     "BIND_CODE_INVALID",
		WSMessage:  "绑定码无效或已过期，请重新获取。",
	},
	ErrUsernameTaken: {
		HTTPStatus: http.StatusConflict,
		HTTPCode:   409,
		HTTPMsg:    "username taken",
		WSCode:     "USERNAME_TAKEN",
		WSMessage:  "该用户名已被占用，请更换后重试。",
	},
	ErrUserAlreadyBound: {
		HTTPStatus: http.StatusConflict,
		HTTPCode:   409,
		HTTPMsg:    "user already bound",
		WSCode:     "USER_ALREADY_BOUND",
		WSMessage:  "你的账号已绑定其他 QQ，如需重新绑定请先解绑。",
	},
	ErrQQAlreadyBound: {
		HTTPStatus: http.StatusConflict,
		HTTPCode:   409,
		HTTPMsg:    "qq already bound",
		WSCode:     "QQ_ALREADY_BOUND",
		WSMessage:  "你的 QQ 已绑定其他账号，如需重新绑定请先解绑。",
	},
	ErrNotBound: {
		HTTPStatus: http.StatusConflict,
		HTTPCode:   409,
		HTTPMsg:    "not bound",
		WSCode:     "NOT_BOUND",
		WSMessage:  "当前没有已绑定的 QQ，无需解绑。",
	},
	ErrRateLimited: {
		HTTPStatus: http.StatusTooManyRequests,
		HTTPCode:   429,
		HTTPMsg:    "rate limited",
		WSCode:     "RATE_LIMITED",
		WSMessage:  "请求过于频繁，请 5 分钟后再试。",
	},
	ErrInvalidCredentials: {
		HTTPStatus: http.StatusUnauthorized,
		HTTPCode:   401,
		HTTPMsg:    "invalid credentials",
		WSCode:     "INVALID_CREDENTIALS",
		WSMessage:  "用户名或密码错误。",
	},
	ErrAccountDisabled: {
		HTTPStatus: http.StatusForbidden,
		HTTPCode:   403,
		HTTPMsg:    "account disabled",
		WSCode:     "ACCOUNT_DISABLED",
		WSMessage:  "该账号已被禁用，请联系管理员。",
	},
	ErrNotAuthenticated: {
		HTTPStatus: http.StatusUnauthorized,
		HTTPCode:   401,
		HTTPMsg:    "not authenticated",
		WSCode:     "NOT_AUTHENTICATED",
		WSMessage:  "会话已失效，请重新登录。",
	},
	ErrServerCodeInvalid: {
		HTTPStatus: http.StatusBadRequest,
		HTTPCode:   400,
		HTTPMsg:    "server code invalid",
		WSCode:     "SERVER_CODE_INVALID",
		WSMessage:  "服务器标识格式不正确，仅允许小写字母、数字和连字符，长度 1-64。",
	},
	ErrServerNameInvalid: {
		HTTPStatus: http.StatusBadRequest,
		HTTPCode:   400,
		HTTPMsg:    "server name invalid",
		WSCode:     "SERVER_NAME_INVALID",
		WSMessage:  "服务器名称不能为空，且长度不超过 128 字符。",
	},
	ErrServerCodeTaken: {
		HTTPStatus: http.StatusConflict,
		HTTPCode:   409,
		HTTPMsg:    "server code taken",
		WSCode:     "SERVER_CODE_TAKEN",
		WSMessage:  "该服务器标识已被占用，请更换后重试。",
	},
	ErrServerNotFound: {
		HTTPStatus: http.StatusNotFound,
		HTTPCode:   404,
		HTTPMsg:    "server not found",
		WSCode:     "SERVER_NOT_FOUND",
		WSMessage:  "未找到指定的游戏服务器。",
	},
	ErrInternal: {
		HTTPStatus: http.StatusInternalServerError,
		HTTPCode:   500,
		HTTPMsg:    "internal error",
		WSCode:     "INTERNAL_ERROR",
		WSMessage:  "服务内部错误，请稍后再试。",
	},
}

// Wrap 把底层 cause 包装到业务哨兵 sentinel 之下，保留完整错误链：
//   - errors.Is(结果, sentinel) 仍为 true，transport 层据此映射错误码不受破坏；
//   - Error() 文本包含上下文 ctx 与根因 cause，配合 zap.Error 即可在日志输出完整链。
//
// cause 为 nil 时直接返回 sentinel 本身（无根因可携带）。
//
// 示例：return Wrap(ErrInternal, err, "查询用户失败")
//
//	日志可见：查询用户失败: internal error: 查询用户失败: pq: ...
func Wrap(sentinel error, cause error, ctx string) error {
	if cause == nil {
		return sentinel
	}
	return fmt.Errorf("%s: %w: %v", ctx, sentinel, cause)
}

// ResolveErrorMeta 根据错误链解析出对外元数据。依次用 errors.Is 匹配已知哨兵，
// 命中即返回对应 ErrorMeta；均未命中则回退到 ErrInternal，保证任何未知错误都映射为
// 500 / INTERNAL_ERROR（不向外部泄露内部细节）。
func ResolveErrorMeta(err error) ErrorMeta {
	for sentinel, meta := range errorMeta {
		if errors.Is(err, sentinel) {
			return meta
		}
	}
	return errorMeta[ErrInternal]
}
