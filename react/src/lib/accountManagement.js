export function deadline(value) {
  if (!value) return 0;
  const date = new Date(value);
  return Number.isNaN(+date) || date.getUTCFullYear() <= 1 ? 0 : +date;
}

export function accountState(record, now = Date.now()) {
  if (record.disabled) return 'disabled';
  if (record.cooling || Math.max(deadline(record.until), deadline(record.breaker_until), deadline(record.auth_until)) > now) return 'cooling';
  if (Object.keys(record.model_cooldowns || {}).length) return 'model_cooling';
  return 'healthy';
}

export const stateLabels = { healthy: '可用', cooling: '冷却 / 熔断', model_cooling: '模型限流', disabled: '已禁用' };
export const stateColors = { healthy: 'green', cooling: 'orange', model_cooling: 'gold', disabled: 'red' };
export const groupsOf = (mapping, uid) => mapping?.[uid]?.length ? mapping[uid] : ['default'];
export const accountReason = reason => ({ 'manual disabled': '管理员手动禁用', 'imported; awaiting administrator enable': '迁移导入，待管理员核对并启用' }[reason] || reason || '');

export function filterAccounts(accounts, groups, filters, now = Date.now()) {
  const query = (filters.search || '').trim().toLowerCase();
  return accounts.filter(a => {
    const expires = Number(a.token_expires_at || 0) * 1000;
    return (!query || [a.uid, a.nickname, a.reason, accountReason(a.reason)].some(x => String(x || '').toLowerCase().includes(query)))
      && (filters.status === 'all' || accountState(a, now) === filters.status)
      && (filters.region === 'all' || (a.region || 'cn') === filters.region)
      && (filters.group === 'all' || groupsOf(groups, a.uid).includes(filters.group))
      && (filters.expiry === 'all'
        || (filters.expiry === 'expired' && expires > 0 && expires <= now)
        || (filters.expiry === 'soon' && expires > now && expires <= now + 86400000)
        || (filters.expiry === 'unknown' && !expires));
  });
}

export function groupChange(current, targets, mode) {
  const result = mode === 'append' ? [...current, ...targets]
    : mode === 'remove' ? current.filter(g => !targets.includes(g)) : targets;
  return [...new Set(result.length ? result : ['default'])];
}

// The target snapshot is immutable. Cancellation never aborts an in-flight write.
export async function runAccountBatch(uids, action, { cancelled = () => false, onResult = () => {} } = {}) {
  const targets = [...new Set(uids)];
  const results = [];
  let stopped = false;
  for (const uid of targets) {
    let row;
    if (stopped || cancelled()) row = { uid, status: 'cancelled', detail: '未执行' };
    else {
      try {
        const result = await action(uid);
        row = { uid, status: result?.ok === false ? 'failed' : 'success', detail: result?.detail || result?.message || '操作成功' };
      } catch (error) {
        row = { uid, status: 'failed', detail: error.message || '请求失败，状态可能已变化，请刷新核对' };
        if (!error.status || error.status === 401 || error.status === 403) stopped = true;
      }
    }
    results.push(row);
    onResult(row, results.length);
  }
  return results;
}

export function downloadFile(contents, name, type = 'application/json') {
  const url = URL.createObjectURL(new Blob([contents], { type }));
  const link = document.createElement('a'); link.href = url; link.download = name;
  document.body.appendChild(link); link.click(); link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function accountCSV(accounts, mapping) {
  const cell = value => {
    let text = String(value ?? '');
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
  };
  return '\ufeff' + [['UID', '昵称', '区域', '状态', '分组', '剩余积分', '原因'], ...accounts.map(a => [a.uid, a.nickname, a.region || 'cn', stateLabels[accountState(a)], groupsOf(mapping, a.uid).join(';'), a.credits || 0, a.reason])].map(row => row.map(cell).join(',')).join('\r\n');
}
