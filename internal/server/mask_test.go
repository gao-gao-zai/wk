package server

import "testing"

func TestMaskPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"13800138000", "138****8000"},
		{"+8613800138000", "138****8000"},
		{"8613800138000", "138****8000"},
		{"17000000001", "170****0001"},
		{"12345", "*****"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := maskPhone(tc.in); got != tc.want {
			t.Errorf("maskPhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMaskPhonesInLogMessage is the property that matters for the auto-enroll
// log ring: it is served verbatim to the console, and the phone number IS the
// account nickname for CN accounts. A leaked paid number is a leaked asset.
func TestMaskPhonesInLogMessage(t *testing.T) {
	cases := []string{
		"[w1] 取号 13800138000",
		"号 13800138000 已在号池，拉黑换下一个",
		"号 +8613800138000 发码被拒（可重试）: 频率限制",
		"号 13800138000 加号成功 uid=abcdef12…（已拉黑）",
	}
	for _, in := range cases {
		out := maskPhonesIn(in)
		if phoneRe.MatchString(out) {
			t.Errorf("maskPhonesIn(%q) still contains a full phone: %q", in, out)
		}
		if out == in {
			t.Errorf("maskPhonesIn(%q) did not change the message", in)
		}
	}
}

// TestMaskPhonesInLeavesOtherTextAlone: masking must not mangle unrelated log
// content such as uids, ids or error codes.
func TestMaskPhonesInLeavesOtherTextAlone(t *testing.T) {
	in := "[w0] 第 3 次尝试失败: code=201 余额不足 sid=52283 uid=abcdef123456"
	if got := maskPhonesIn(in); got != in {
		t.Errorf("unrelated text was modified:\n in=%q\nout=%q", in, got)
	}
}
