package reqproxy

import (
	"errors"
	"strings"
	"testing"

	"workbuddy2api/internal/growthrouteerr"
)

func TestGrowthKernelErrorClassifiesAndDiscardsCause(t *testing.T) {
	const secret = "https://private-user:private-password@private-host/?token=private-token"
	for _, tc := range []struct {
		prefix string
		code   growthrouteerr.Code
	}{
		{"inbound 配置构造: ", growthrouteerr.KernelInbound},
		{"inbound Build: ", growthrouteerr.KernelInbound},
		{"AddInbound(growth_fixture):31080: ", growthrouteerr.KernelInbound},
		{"路由规则 Build: ", growthrouteerr.KernelRule},
		{"Xray 路由模块不可用", growthrouteerr.KernelRule},
		{"AddRule(growth_fixture): ", growthrouteerr.KernelRule},
		{"outbound 配置构造(growth_fixture): ", growthrouteerr.KernelOutbound},
		{"outbound Build(growth_fixture): ", growthrouteerr.KernelOutbound},
		{"AddOutbound(growth_fixture): ", growthrouteerr.KernelOutbound},
		{"未知协议 ", growthrouteerr.KernelOutbound},
		{"unrecognized ", growthrouteerr.Unknown},
	} {
		original := errors.New(tc.prefix + secret)
		err := growthKernelError(original)
		if got := growthrouteerr.Reason(err); got != tc.code {
			t.Errorf("prefix %q: got %s want %s", tc.prefix, got, tc.code)
		}
		if strings.Contains(err.Error(), secret) || errors.Is(err, original) {
			t.Fatal("kernel cause retained")
		}
	}
	if growthrouteerr.Reason(growthKernelError(errors.New("内核已关闭"))) != growthrouteerr.KernelUnavailable {
		t.Fatal("closed kernel not classified")
	}
}
