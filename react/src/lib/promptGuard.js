export const stageNames = { control: '中性对照检查', verify_full: '完整提示词替换验证', reproduce_full: '完整提示词复现', bisect: '分段 / 组合检查' };
export const diagnosticStates = { running: '执行中', queued: '排队中', located: '已定位', passed: '验证通过', blocked: '仍触发 11128', inconclusive: '未能完成检查', cancelled: '已取消', interrupted: '已中断' };
const localReasons = new Set(['account_busy_or_unavailable', 'account_needs_refresh', 'upstream_unavailable', 'invalid_prompt']);
const reasons = {
  account_busy_or_unavailable: ['原账号忙碌或不可用', '等待 3 秒仍未取得该账号的执行槽位，未发送上游请求。等待账号恢复后重试；诊断不会自动更换账号。'],
  account_needs_refresh: ['原账号凭据需要刷新', '未发送上游请求。请先在账号管理中检查该账号状态，恢复后重新验证。'],
  upstream_unavailable: ['诊断上游未配置', '未发送上游请求。检查服务的账号池和上游配置后重试。'],
  invalid_prompt: ['系统提示词无法转换', '未发送上游请求。请检查替换规则和系统消息格式。'],
  transport_error: ['连接上游失败', '已尝试请求上游，但没有获得有效响应。检查网络和请求代理后重试，不能据此认定提示词被拒绝。'],
  request_timeout: ['上游请求超时', '请求没有在时限内完成，无法判断本次提示词是否通过，请稍后重试。'],
  rate_limit: ['上游限流', '上游暂不接受该账号或模型的请求，等待限流恢复后重试。'],
  upstream_auth_error: ['上游认证或权限错误', '请检查原账号的凭据与模型权限，恢复后重试。'],
  upstream_error: ['上游返回错误', '本次没有得到可判定的提示词检查结果，请结合 HTTP 状态检查账号与模型。'],
  incomplete_or_invalid_stream: ['上游响应流不完整或包含错误', 'HTTP 200 不能单独证明通过；未得到正常结束的完整响应，请稍后重试。'],
  11128: ['上游拒绝（11128）', '该步骤触发了 11128。中性对照也被拒绝时，不能把错误归因于原提示词。'],
};

export function probeExplanation(row = {}) {
  const info = reasons[row.reason];
  if (info) return { title: info[0], description: info[1] };
  if (row.outcome === 'pass') return { title: '检查通过', description: '上游正常完成响应。' };
  if (row.outcome === 'blocked') return { title: '上游拒绝（11128）', description: '该步骤触发了 11128。' };
  return { title: '未获得确定结果', description: '旧记录没有提供更具体的原因；这不表示提示词已通过或已被拒绝。' };
}

export function requestAttempted(row = {}) {
  if (row.cached_from) return false;
  if (typeof row.request_attempted === 'boolean') return row.request_attempted;
  // Recover only facts that the old adapter's fixed classifications guarantee.
  if (localReasons.has(row.reason)) return false;
  if (row.http > 0) return true;
  return null;
}

export function httpDescription(row = {}) {
  if (row.cached_from) return '复用先前响应';
  if (row.http > 0) return String(row.http);
  const attempted = requestAttempted(row);
  return attempted === false ? '未发送' : attempted === true ? '未收到响应' : '未记录响应';
}

export function jobView(job = {}) {
  const trace = job.trace || [];
  const active = ['running', 'queued'].includes(job.status);
  const requests = trace.filter(row => requestAttempted(row) === true).length;
  const unknown = trace.filter(row => !row.cached_from && requestAttempted(row) === null).length;
  const checks = trace.length || job.calls || 0;
  const cached = job.cache_hits || trace.filter(row => row.cached_from).length;
  const last = trace.at(-1);
  const explanation = probeExplanation(last);
  const controlFailed = trace.some(row => row.step === 'control' && row.outcome !== 'pass');
  const unavailable = last && requestAttempted(last) === false && !last.cached_from;
  let headline = diagnosticStates[job.status] || '等待启动';
  let description = job.note || '先检查中性对照，再检查完整系统提示词；对照不能完成时会停止后续检查。';
  if (job.status === 'inconclusive' && controlFailed) {
    headline = `对照检查未完成：${explanation.title}`;
    description = `${explanation.description} 后续完整提示词检查未执行，本次不能判断替换是否有效。`;
  } else if (!active && job.status === 'inconclusive' && last?.outcome === 'inconclusive') {
    headline = explanation.title;
    description = `${explanation.description}${job.note ? ` ${job.note}` : ''}`;
  }
  const type = job.status === 'passed' ? 'success' : active ? 'info' : 'warning';
  return { active, requests, unknown, checks, cached, headline, description, type, unavailable, budget: job.kind === 'verify' ? 2 : job.budget, requestLabel: unknown ? `${requests} 次已确认，${unknown} 次未记录` : `${requests} 次` };
}

export function verificationStages(job = {}) {
  const trace = job.trace || [];
  const active = ['running', 'queued'].includes(job.status);
  const full = job.kind === 'verify' ? 'verify_full' : 'reproduce_full';
  return ['control', full].map((step, index) => {
    const row = trace.find(t => t.step === step);
    const previousFailed = index > 0 && trace.some(t => t.step === 'control' && t.outcome !== 'pass');
    let state = '未执行', explanation = previousFailed ? '对照检查未完成，已停止后续检查' : '本次任务未执行到此步骤';
    if (row) {
      state = row.outcome === 'pass' ? '通过' : row.outcome === 'blocked' ? '11128 拒绝' : requestAttempted(row) === false ? '未发送' : '未完成';
      explanation = probeExplanation(row).title;
    } else if (active) {
      state = job.current_step === step ? '执行中' : '等待执行';
      explanation = job.current_step === step ? '正在检查账号与上游响应' : index === 0 ? '先确认中性对照可以正常请求' : '仅在中性对照通过后执行';
    }
    return { step, name: stageNames[step], state, explanation, outcome: row?.outcome };
  });
}

export function candidateView(item, revision) {
  if (!item?.candidate) return null;
  const current = item.candidate_located_at && item.job?.kind === 'locate' && new Date(item.candidate_located_at) >= new Date(item.job.started) && item.candidate_revision === revision;
  if (current) return { historical: false, title: item.candidate_minimal ? '本次定位出的可复现片段' : '本次已复现片段（尚未完成最小化检查）', description: '仅表示该片段已复现 11128；仍需设置替换并验证完整系统提示词。' };
  return { historical: true, title: '历史定位片段（本次未重新确认）', description: item.job?.kind === 'verify' ? '下方片段来自之前的定位任务。本次验证没有重新定位这句话，请以上方本次验证结果为准。' : '本次任务没有产出可确认的新片段，保留先前结果供参考。旧记录可能没有保存定位时间与配置版本。' };
}
