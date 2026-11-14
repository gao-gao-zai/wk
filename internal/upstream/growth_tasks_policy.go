package upstream

import "errors"

// ErrGrowthTaskUnavailable prevents retired task codes from reaching upstream,
// including requests using an old page or a previously collected snapshot.
var ErrGrowthTaskUnavailable = errors.New("此成长任务已暂时停用")

// CurrentGrowthTaskCodes is the temporary set verified on CN accounts on
// 2026-11-14. Keeping a task here does not make it eligible for every account:
// execution still checks that account's current upstream task and progress.
// Return a fresh slice so callers cannot change the process-wide policy.
func CurrentGrowthTaskCodes() []string {
	return []string{"RichMeow_Chat", "Library_read", "wb_wechat_oa_subscribe_task", "skill_share", "first_buddy"}
}

func IsCurrentGrowthTask(code string) bool {
	switch code {
	case "RichMeow_Chat", "Library_read", "wb_wechat_oa_subscribe_task", "skill_share", "first_buddy":
		return true
	default:
		return false
	}
}
