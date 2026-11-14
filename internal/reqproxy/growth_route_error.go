package reqproxy

import (
	"strings"

	"workbuddy2api/internal/growthrouteerr"
)

// Kernel's existing API returns untyped errors. Match only its fixed stage
// prefixes, discard the entire original error, and fail closed to unknown.
// No node address, credentials, or kernel error text is retained.
func growthKernelError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, prefix := range []string{"inbound 配置构造:", "inbound Build:", "AddInbound("} {
		if strings.HasPrefix(message, prefix) {
			return growthrouteerr.New(growthrouteerr.KernelInbound)
		}
	}
	for _, prefix := range []string{"路由规则 Build:", "Xray 路由模块不可用", "AddRule("} {
		if strings.HasPrefix(message, prefix) {
			return growthrouteerr.New(growthrouteerr.KernelRule)
		}
	}
	for _, prefix := range []string{"outbound 配置构造(", "outbound Build(", "AddOutbound(", "未知协议 "} {
		if strings.HasPrefix(message, prefix) {
			return growthrouteerr.New(growthrouteerr.KernelOutbound)
		}
	}
	if message == "内核已关闭" {
		return growthrouteerr.New(growthrouteerr.KernelUnavailable)
	}
	return growthrouteerr.New(growthrouteerr.Unknown)
}
