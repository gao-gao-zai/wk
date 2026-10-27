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
