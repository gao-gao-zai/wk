export function smsPlatformPatch(settings, draft, fallbackEnabled) {
  const { provider, base, card, province, sid } = draft;
  const token = draft.token.trim();
  const fallback = draft.fallback.trim();
  const keyword = draft.keyword.trim();
  if (!settings.saved_platforms?.[provider] && !token && provider !== (settings.provider || 'haozhuma')) {
    throw new Error('该平台尚未保存凭据，请先填写 Token');
  }
  const usesFallback = fallbackEnabled ?? (!!fallback || !!settings.fallback_mounted);
  if ((provider === 'ecode' || (provider === 'haozhuma' && usesFallback)) && !keyword) {
    throw new Error('请填写 E码云短信关键词，例如腾讯科技');
  }
  const patch = { provider, ecode_keyword: keyword, ecode_base: base, ecode_card_type: card, ecode_province: province };
  if (provider === 'haozhuma') {
    patch.sid = sid;
    if (fallback && fallbackEnabled !== false) patch.fallback = fallback;
    if (fallbackEnabled !== undefined) patch.fallback_enabled = fallbackEnabled;
  }
  if (provider !== (settings.provider || 'haozhuma') || fallbackEnabled === false) patch.mode = 'auto';
  if (token) Object.assign(patch, { token, user: '', pass: '' });
  return patch;
}

export function webTokenStatus(h5 = {}) {
  const persistent = h5.storage === 'encrypted_file';
  let type = 'info', title = '尚未配置 Web 登录令牌';
  let description = persistent ? '验证通过后加密保存在服务端，重启会自动恢复。令牌不会回显。' : '当前实例未启用令牌持久化，令牌仅在本次服务运行期间生效。';
  if (h5.storage_error) {
    type = 'warning'; title = 'Web 令牌存储异常';
    description = `${h5.has ? '当前令牌仍保留。' : ''}${h5.storage_error}`;
  } else if (h5.expired) {
    type = 'warning'; title = 'Web 登录令牌已失效';
    description = '请重新登录豪猪，复制新的 qs_web_token 并验证保存。重启不会延长令牌有效期。';
  } else if (h5.has && h5.persisted) {
    type = 'success'; title = 'Web 登录令牌已加密保存';
    description = '服务重启后自动恢复，使用时检查有效性；无需每次重新粘贴。';
  } else if (h5.has) {
    type = 'warning'; title = 'Web 登录令牌仅本次生效';
    description = persistent ? '当前令牌尚未写入持久存储，请粘贴后验证保存。' : description;
  }
  return { type, title, description, saveLabel: persistent ? '验证并保存令牌' : '验证并使用令牌',
    clearDescription: persistent ? '已保存令牌也会清空，重启后不会恢复。项目搜索与监控将不可用，普通接码不受影响。' : '仅清空当前运行中的令牌。项目搜索与监控将不可用，普通接码不受影响。' };
}
