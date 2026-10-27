export const platformNames = { haozhuma: '豪猪', ecode: 'E码云', auto: '自动选择', both: '双平台轮换' };
export function readyPlatforms(hz = {}) {
  if (hz.configured_platforms) return hz.configured_platforms;
  return {
    haozhuma: !!hz.saved_platforms?.haozhuma && !!hz.sid,
    ecode: !!hz.saved_platforms?.ecode && !!hz.ecode_keyword,
  };
}
export function taskPlatformOptions(ready) {
  return [
    { value: 'auto', label: ready.haozhuma && ready.ecode ? '自动 · 豪猪优先，E码云备用' : `自动 · ${ready.ecode ? 'E码云' : '豪猪'}`, disabled: !ready.haozhuma && !ready.ecode },
    { value: 'haozhuma', label: '仅豪猪', disabled: !ready.haozhuma },
    { value: 'ecode', label: '仅 E码云', disabled: !ready.ecode },
    { value: 'both', label: '双平台轮换', disabled: !ready.haozhuma || !ready.ecode },
  ];
}
export function validTaskPlatform(current, ready) {
  return taskPlatformOptions(ready).some(x => x.value === current && !x.disabled) ? current : 'auto';
}
export function taskProgress(status) {
  const want = Number(status?.want || 0), ok = Number(status?.ok || 0);
  return { want, ok, percent: want ? Math.min(100, Math.round(ok / want * 100)) : 0 };
}
export function compatibleGroups(groups = []) { return groups.filter(g => !g.region || g.region === 'all' || g.region === 'cn'); }

// 始终串行续期；失败、空闲与启动均不丢失轮询，关闭后不再发布或排期。
export function createTaskPoller(read, publish, onError, schedule = setTimeout, cancel = clearTimeout) {
  let closed = false, timer = null, pending = false, again = false;
  async function refresh() {
    if (closed) return;
    if (pending) { again = true; return; }
    if (timer !== null) { cancel(timer); timer = null; }
    pending = true;
    let delay = 5000;
    try { const body = await read(); if (!closed) publish(body); delay = body?.running || body?.config_busy ? 2000 : 5000; }
    catch (e) { if (!closed) onError(e); delay = 3000; }
    finally {
      pending = false;
      if (!closed) { const ms = again ? 0 : delay; again = false; timer = schedule(refresh, ms); }
    }
  }
  return { refresh, close() { closed = true; if (timer !== null) cancel(timer); } };
}

// 控制台使用扁平 schedule 字段，配置接口返回嵌套对象。保留显式 false。
export function mergeSavedConfig(current, saved) {
  return { ...current, ...saved, ...(saved.schedule || {}) };
}
