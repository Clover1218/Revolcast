package transport

import (
	"go.uber.org/zap"

	"revolcast-backend/internal/biz"
)

// logErr 统一记录业务错误：自动附带 error_code 字段，并配合 zap.Error 输出完整 %w 错误链。
// 替换各 handler 中散落的 log.Warn("xxx 失败", zap.Error(err))，保证排障信息一致且完整：
// 日志既能看到业务错误码（用于聚合告警），也能看到从根因到业务哨兵的完整错误链。
func logErr(log *zap.Logger, msg string, err error) {
	log.Warn(msg,
		zap.String("error_code", biz.ResolveErrorMeta(err).WSCode),
		zap.Error(err),
	)
}
