import assert from 'node:assert/strict';
import test from 'node:test';
import { smsPlatformPatch } from './smsPlatforms.js';

const settings = { provider: 'ecode', saved_platforms: { haozhuma: true, ecode: true } };
const draft = { provider: 'haozhuma', keyword: '腾讯科技', base: 'https://example.test', card: '全部', province: '浙江', sid: '52283', token: '', fallback: '' };

test('switching to saved Haozhuma retains account credentials and Ecode settings', () => {
  const patch = smsPlatformPatch(settings, draft);
  assert.equal(patch.mode, 'auto');
  assert.equal(patch.sid, '52283');
  assert.equal(patch.ecode_province, '浙江');
  for (const key of ['token', 'user', 'pass', 'fallback']) assert.equal(Object.hasOwn(patch, key), false);
});
test('switching back to Ecode does not clear either credential or fallback', () => {
  const patch = smsPlatformPatch({ ...settings, provider: 'haozhuma' }, { ...draft, provider: 'ecode' });
  for (const key of ['token', 'user', 'pass', 'fallback', 'sid']) assert.equal(Object.hasOwn(patch, key), false);
});
test('unknown target requires credentials; a supplied token updates only the target', () => {
  assert.throws(() => smsPlatformPatch({ provider: 'ecode' }, draft), /尚未保存凭据/);
  const patch = smsPlatformPatch({ provider: 'ecode' }, { ...draft, token: ' fixture ' });
  assert.equal(patch.token, 'fixture');
  assert.equal(patch.user, '');
});
test('disabling fallback preserves saved credentials and permits clearing its keyword', () => {
  const patch = smsPlatformPatch({ ...settings, fallback_mounted: true }, { ...draft, keyword: '', fallback: 'unsaved-draft' }, false);
  assert.equal(patch.fallback_enabled, false);
  assert.equal(Object.hasOwn(patch, 'fallback'), false);
  assert.throws(() => smsPlatformPatch(settings, { ...draft, keyword: '' }, true), /短信关键词/);
});
