export const ACCOUNT_IMPORT_MAX_BYTES = 16 * 1024 * 1024;
export const ACCOUNT_CREDENTIAL_MAX_BYTES = 64 * 1024;

const isObject = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const nonEmptyString = value => typeof value === 'string' && value.trim().length > 0;

export function validTransferPassword(password) {
  const length = new TextEncoder().encode(password).length;
  return length >= 12 && length <= 1024;
}

export function identifyAccountImport(content) {
  if (isObject(content) && content.format === 'workbuddy2api-accounts') {
    if (content.version !== 1 || !nonEmptyString(content.salt) || !nonEmptyString(content.nonce) || !nonEmptyString(content.ciphertext)) {
      throw new Error('加密迁移文件格式或版本无效');
    }
    return { kind: 'encrypted', bundle: content };
  }
  if (isObject(content) && isObject(content.auth) && isObject(content.account)
      && nonEmptyString(content.auth.accessToken) && nonEmptyString(content.account.uid)) {
    return { kind: 'credential', credential: content };
  }
  throw new Error('请选择 WorkBuddy2API 加密迁移文件或单个官方 auth/account 凭据 JSON 文件');
}

export function normalizeAccountImportOptions(options, singleAccount) {
  const groupMode = options.group_mode || 'source';
  if (!['source', 'replace', 'append'].includes(groupMode)) throw new Error('请选择有效的分组策略');
  if (options.run_growth_tasks !== undefined && typeof options.run_growth_tasks !== 'boolean') throw new Error('成长任务开关格式无效');
  const normalized = { group_mode: groupMode, run_growth_tasks: options.run_growth_tasks === true };
  if (groupMode !== 'source') {
    if (!Array.isArray(options.groups) || options.groups.some(group => typeof group !== 'string' || !group.trim())) {
      throw new Error('请选择有效的目标分组');
    }
    normalized.groups = [...new Set(options.groups.map(group => group.trim()))];
    if (!normalized.groups.length) throw new Error('替换或追加分组时，请至少选择一个目标分组');
  }
  if (options.nickname !== undefined && typeof options.nickname !== 'string') throw new Error('昵称格式无效');
  const nickname = (options.nickname || '').trim();
  if (nickname) {
    if (!singleAccount) throw new Error('只有单账号文件可以自定义昵称');
    if (Array.from(nickname).length > 128 || /[\u0000-\u001f\u007f-\u009f]/u.test(nickname)) {
      throw new Error('昵称最多 128 个字符，且不能包含控制字符');
    }
    normalized.nickname = nickname;
  }
  return normalized;
}

export function buildAccountImportRequest(source, password, uids, options, singleAccount = source?.kind === 'credential') {
  let request;
  if (source?.kind === 'encrypted') {
    if (!validTransferPassword(password)) throw new Error('迁移密码需要 12–1024 字节');
    request = { bundle: source.bundle, password };
  } else if (source?.kind === 'credential') {
    // Preserve all source metadata, including integers outside JS's safe range.
    request = source.credentialJSON === undefined ? { credential: source.credential } : { credential_json: source.credentialJSON };
  } else {
    throw new Error('请先选择账号文件');
  }
  if (options !== undefined) request = { ...request, ...normalizeAccountImportOptions(options, singleAccount) };
  return uids === undefined ? request : { ...request, uids: [...uids] };
}

export function accountImportDeviceLabel(account) {
  if (account.exists || account.device_action === 'skip') return '已存在，跳过设备处理';
  if (account.device_action === 'generate_mac') return '将新建 Mac 设备档案';
  if (account.device_action === 'preserve') {
    if (account.device_platform === 'macos') return '保留已有 Mac 设备档案';
    if (account.device_platform === 'windows') return '保留已有 Windows 设备档案';
    return '保留已有设备档案';
  }
  return '设备信息待确认';
}

// Only the most recently selected file may update the dialog, including errors.
export function createAccountImportFileLoader() {
  let revision = 0;
  return {
    invalidate() { revision++; },
    async loadMany(files) {
      const current = ++revision;
      const list = Array.from(files);
      if (!list.length || list.length > 1000) return { source: null, fileName: '', error: '请选择 1–1000 个账号文件' };
      const loaded = [];
      for (const file of list) {
        const result = await createAccountImportFileLoader().load(file);
        if (current !== revision) return null;
        if (result.error) return { source: null, fileName: file.name, error: `${file.name}：${result.error}` };
        loaded.push(result.source);
      }
      if (loaded.length === 1) return { source: loaded[0], fileName: list[0].name, error: '' };
      if (loaded.some(source => source.kind !== 'credential')) return { source: null, fileName: '', error: '多文件导入仅支持官方凭据；加密迁移文件请单独选择' };
      const uids = loaded.map(source => source.credential.account.uid.trim());
      if (new Set(uids).size !== uids.length) return { source: null, fileName: '', error: '所选文件包含重复账号 UID，请检查后重新选择' };
      return { source: { kind: 'credentials', sources: loaded }, fileName: `已选择 ${list.length} 个账号文件`, error: '' };
    },
    async load(file) {
      const current = ++revision;
      let result;
      try {
        if (file.size > ACCOUNT_IMPORT_MAX_BYTES) throw new Error('文件超过 16 MiB，请分批导出。');
        let content, rawText;
        try { rawText = await file.text(); content = JSON.parse(rawText); }
        catch (error) {
          // JSON.parse errors can quote credential text. Never display them.
          throw new Error(error instanceof SyntaxError ? '文件不是有效的 JSON 账号文件' : '无法读取账号文件，请重新选择');
        }
        const source = identifyAccountImport(content);
        if (source.kind === 'credential' && file.size > ACCOUNT_CREDENTIAL_MAX_BYTES) throw new Error('单个官方凭据文件超过 64 KiB');
        if (source.kind === 'credential') source.credentialJSON = rawText;
        result = { source, fileName: file.name, error: '' };
      } catch (error) {
        result = { source: null, fileName: file.name, error: error.message };
      }
      return current === revision ? result : null;
    },
  };
}

// Each official credential uses the existing single-account endpoint. Stop on
// uncertain writes so the remaining accounts are never silently replayed.
export async function runMultiAccountImport(post, sources, options, selected, cancelled = () => false, onProgress = () => {}) {
  const frozenOptions = normalizeAccountImportOptions(options, false);
  const preview = selected === undefined;
  const wanted = new Set(selected || []);
  const targets = preview ? sources : sources.filter(source => wanted.has(source.credential.account.uid));
  const rows = [];
  let stopped = false;
  let completed = 0;
  onProgress({ completed, total: targets.length });
  for (const source of targets) {
    const uid = source.credential.account.uid;
    if (cancelled() || stopped) {
      stopped = true;
      if (!preview) rows.push({ uid, ok: false, cancelled: true, detail: '未提交，请重新预览' });
      continue;
    }
    try {
      const body = await post(preview ? '/admin/accounts/import/preview' : '/admin/accounts/import',
        buildAccountImportRequest(source, '', preview ? undefined : [uid], frozenOptions, false));
      if (preview) {
        if (!Array.isArray(body.accounts) || body.accounts.length !== 1 || body.accounts[0].uid !== uid) throw new Error('预览回执无效');
        rows.push(body.accounts[0]);
      } else {
        if (!Array.isArray(body.results) || body.results.length !== 1 || body.results[0].uid !== uid) throw new Error('导入回执不完整，状态可能已变化');
        rows.push(body.results[0]);
        if (body.stop || body.results[0].pending) stopped = true;
      }
    } catch (error) {
      if (preview) {
        if (!error.status || error.status >= 500 || [401, 403, 429].includes(error.status)) throw error;
        const nickname = source.credential.account.nickname;
        rows.push({ uid, nickname: typeof nickname === 'string' ? nickname : '', issue: error.message || '预览失败' });
      } else {
        rows.push({ uid, ok: false, detail: `${error.message || '导入请求失败'}；请刷新账号列表核对状态` });
        stopped = true;
      }
    }
    completed++;
    onProgress({ completed, total: targets.length });
  }
  if (preview) return { accounts: rows, count: rows.length, stop: stopped };
  const success = rows.filter(row => row.ok).length;
  const skipped = rows.filter(row => row.skipped).length;
  return { ok: success === rows.length && !stopped, success, skipped, stop: stopped, results: rows };
}

export function accountImportResultType(result) {
  const rows = result.results || [];
  if (rows.some(row => !row.ok && !row.skipped)) return 'warning';
  return result.ok === true ? 'success' : 'info';
}

export function accountImportGrowthLabel(account, enabled) {
  if (account.skipped || account.exists) return '重复账号，未排队';
  if (account.cancelled) return '未提交，未排队';
  if (account.pending) return '导入待核对，未排队';
  if (account.issue || account.ok === false) return '导入未完成，未排队';
  if (!enabled) return '未开启';
  if (account.growth_status === 'enqueue_failed') return '排队失败（导入已成功）';
  if (account.growth_status === 'queued') return '已排队，账号保持停用';
  if (account.growth_status === 'unsupported_region' || account.region === 'global') return '海外版不支持，未排队';
  if (account.ok === true) return '未返回排队状态，请到成长任务中心核对';
  return '成功新增后检查未完成任务';
}

const growthText = value => typeof value === 'string' ? value.replace(/\bBearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer [已隐藏]')
  .replace(/("(?:access_?token|refresh_?token|api[_-]?key|authorization)"\s*:\s*")[^"]*(")/gi, '$1[已隐藏]$2') : '';

export function importGrowthJobView(snapshot) {
  const phase = ['queued', 'running', 'done', 'failed', 'cancelled'].includes(snapshot?.phase) ? snapshot.phase : 'unknown';
  const total = Number.isFinite(snapshot?.total) ? Math.max(0, snapshot.total) : 0;
  const done = Number.isFinite(snapshot?.done) ? Math.max(0, snapshot.done) : 0;
  const results = (Array.isArray(snapshot?.results) ? snapshot.results : []).map((item, index) => ({
    key: index, task_code: growthText(item.task_code), desc: growthText(item.desc), message: growthText(item.message),
    ok: item.ok === true, skipped: item.skipped === true, claimed: item.claimed === true,
  }));
  const partialFailure = results.some(item => !item.ok);
  return {
    phase, active: phase === 'running' || phase === 'queued', total, done,
    percent: total > 0 ? Math.min(100, Math.floor(done * 100 / total)) : 0,
    label: { queued: '排队中', running: '执行中', done: partialFailure ? '已结束，部分任务失败' : '已结束', failed: '任务失败', cancelled: '任务已取消', unknown: '状态未知' }[phase],
    severity: phase === 'failed' || partialFailure ? 'warning' : phase === 'done' ? 'success' : 'info',
    current: growthText(snapshot?.current), error: growthText(snapshot?.error), results,
  };
}
