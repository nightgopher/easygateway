package provider

import (
	"errors"

	"github.com/nightgopher/easygateway/internal/archive"
)

// RedactedErrorText 返回错误文本的脱敏版本。上游错误消息可能回显凭据
// （如 "invalid api key: sk-…"），进入运行日志或管理面之前必须先脱敏，
// 与归档链路（archive.Redact）保持同一套规则。
func RedactedErrorText(err error) string {
	if err == nil {
		return ""
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr.Message != "" {
		return string(archive.RedactText([]byte(providerErr.Error())))
	}
	return string(archive.RedactText([]byte(err.Error())))
}
