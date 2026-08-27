package application

import "strings"

// actorName 统一处理审计操作者字段的空白，具体命令仍由领域层校验必填性。
func actorName(value string) string {
	return strings.TrimSpace(value)
}
