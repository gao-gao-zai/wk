// blackcat.go 夜猫子任务排程侧：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话。
// 由 blackcat_hours（默认 [23]）触发；执行前二次校验 InNightWindow（时点被
// 手动改成白天时直接静默跳过，不做无意义的上游调用）。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 夜间窗口，跳过本轮")
		return
	}
	cfg := s.snapshot()
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		if a.Region() == "global" {
			continue // global 无 CN 任务体系
		}
		need, err := cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", st.UID, err)
			continue
		}
		if need <= 0 {
			continue
		}
		// 判据：每夜只计 1 次（target=3 = 3 夜累计）。只跑 1 次，
		// 不足的天数靠后续每晚的排程逐夜补足。
		if _, err := cfg.Upstream.RunNightChats(a, 1); err != nil {
			log.Printf("blackcat %s: 夜间对话失败: %v", st.UID, err)
			continue
		}
		log.Printf("blackcat %s: 今夜已计 1 次（剩余 %d 夜后续排程补足）", st.UID, need-1)
	}
}
