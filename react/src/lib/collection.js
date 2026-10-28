export const resourceLabels = { balance: '积分余额', usage: '请求用量', credential: '凭证有效期', models: '模型权限', tasks: '成长任务状态' };
export const stateLabels = { unknown: '未采集', queued: '排队中', running: '采集中', backoff: '退避重试', success: '成功', failed: '失败', skipped: '跳过', cancelled: '已取消', interrupted: '已中断', completed: '已完成', cancelling: '正在取消' };
export const isActive = job => ['queued', 'running', 'cancelling'].includes(job?.status);
export const progress = job => job?.total ? Math.min(100, Math.round(job.done * 100 / job.total)) : 0;
export const freshness = snapshot => !snapshot?.last_success ? '尚无成功快照' : `${snapshot.stale ? '已过期 · ' : ''}${new Date(snapshot.last_success * 1000).toLocaleString()}`;

// One request at a time; cleanup aborts the request and never schedules again.
export function pollCollection(load, receive, failed, { delay = 2500, schedule = setTimeout, unschedule = clearTimeout } = {}) {
  let stopped = false, timer;
  const controller = new AbortController();
  const tick = async () => {
    try { const value = await load(controller.signal); if (!stopped && receive(value) === false) stopped = true; }
    catch (error) { if (!stopped && error.name !== 'AbortError') failed(error); }
    finally { if (!stopped) timer = schedule(tick, delay); }
  };
  void tick();
  return () => { stopped = true; controller.abort(); unschedule(timer); };
}
