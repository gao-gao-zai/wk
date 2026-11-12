import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ACCOUNT_CREDENTIAL_MAX_BYTES, ACCOUNT_IMPORT_MAX_BYTES, accountImportDeviceLabel, accountImportGrowthLabel, accountImportResultType, buildAccountImportRequest,
  createAccountImportFileLoader, identifyAccountImport, importGrowthJobView, normalizeAccountImportOptions, runMultiAccountImport, validTransferPassword,
} from './accountTransfer.js';

const official = uid => ({ auth: { accessToken: 'fake-access-token', refreshToken: 'fake-refresh-token', expiresAt: 1900000000000 }, account: { uid, nickname: 'Test account' } });
const bundle = { format: 'workbuddy2api-accounts', version: 1, salt: 'fake-salt', nonce: 'fake-nonce', ciphertext: 'fake-ciphertext' };
const password = 'test-transfer-password';
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const file = (name, content) => ({ name, size: 100, text: async () => JSON.stringify(content) });

test('official credentials build password-free preview and selected import requests', () => {
  const credential = official('u1');
  const source = identifyAccountImport(credential);
  assert.equal(source.kind, 'credential');
  assert.deepEqual(buildAccountImportRequest(source, 'leftover password'), { credential });
  const uids = ['u1'];
  const request = buildAccountImportRequest(source, '', uids);
  uids.push('u2');
  assert.deepEqual(request, { credential, uids: ['u1'] });
  assert.equal('password' in request, false);
  assert.equal('bundle' in request, false);
});

test('encrypted credentials retain bundle/password contract and require a valid password', () => {
  const source = identifyAccountImport(bundle);
  assert.equal(source.kind, 'encrypted');
  assert.deepEqual(buildAccountImportRequest(source, password), { bundle, password });
  assert.deepEqual(buildAccountImportRequest(source, password, ['u1']), { bundle, password, uids: ['u1'] });
  assert.throws(() => buildAccountImportRequest(source, 'short'));
  assert.throws(() => buildAccountImportRequest(null, password));
  assert.equal(validTransferPassword('密'.repeat(4)), true);
  assert.equal(validTransferPassword('密'.repeat(342)), false);
});

test('rejects unsupported shapes and incomplete/versioned bundles without quoting credentials', () => {
  for (const content of [null, [], {}, [official('u1')], { accessToken: 'secret', uid: 'u1' },
    { auth: { accessToken: 'secret' }, account: {} }, { auth: [], account: { uid: 'u1' } },
    { ...bundle, version: 2 }, { ...bundle, nonce: '' }]) {
    assert.throws(() => identifyAccountImport(content), error => !error.message.includes('secret'));
  }
});

test('reads an extensionless official file and reports read/JSON/size errors safely', async () => {
  const loader = createAccountImportFileLoader();
  const loaded = await loader.load(file('tmp', official('u1')));
  assert.equal(loaded.fileName, 'tmp');
  assert.equal(loaded.source.kind, 'credential');
  assert.equal(loaded.error, '');
  const malformed = await loader.load({ name: 'tmp', size: 10, text: async () => '{"accessToken":"never-display-this"' });
  assert.equal(malformed.source, null);
  assert.match(malformed.error, /有效的 JSON/);
  assert.equal(malformed.error.includes('never-display-this'), false);
  const unreadable = await loader.load({ name: 'tmp', size: 10, text: async () => { throw new Error('sensitive filesystem text'); } });
  assert.equal(unreadable.error.includes('sensitive'), false);
  let read = false;
  const oversized = await loader.load({ name: 'tmp', size: ACCOUNT_IMPORT_MAX_BYTES + 1, text: async () => { read = true; } });
  assert.match(oversized.error, /16 MiB/);
  assert.equal(read, false);
  const largeCredential = await loader.load({ ...file('tmp', official('u1')), size: ACCOUNT_CREDENTIAL_MAX_BYTES + 1 });
  assert.match(largeCredential.error, /64 KiB/);
  // Failure does not poison the next selection or leave a pending read.
  assert.equal((await loader.load(file('retry', bundle))).source.kind, 'encrypted');
});

test('slow old reads and old errors cannot replace the latest file; unmount invalidates pending reads', async () => {
  const loader = createAccountImportFileLoader();
  for (const reject of [false, true]) {
    const old = deferred();
    const pending = loader.load({ name: 'old', size: 10, text: () => old.promise });
    assert.equal((await loader.load(file('latest', official('latest')))).source.credential.account.uid, 'latest');
    if (reject) old.reject(new Error('old error'));
    else old.resolve(JSON.stringify(bundle));
    assert.equal(await pending, null);
  }
  const closing = deferred();
  const pending = loader.load({ name: 'closing', size: 10, text: () => closing.promise });
  loader.invalidate();
  closing.resolve(JSON.stringify(official('closed')));
  assert.equal(await pending, null);
});

test('result severity uses the submitted import outcome, independent of preview size', () => {
  // Selecting one of many preview rows is a complete success.
  assert.equal(accountImportResultType({ ok: true, success: 1, results: [{ uid: 'u1', ok: true }] }), 'success');
  assert.equal(accountImportResultType({ ok: false, success: 1, results: [{ ok: true }, { ok: false, skipped: true }] }), 'info');
  assert.equal(accountImportResultType({ ok: false, results: [{ ok: false, detail: 'write failed' }] }), 'warning');
  assert.equal(accountImportResultType({ ok: false, results: [] }), 'info');
});

test('custom options remain outside credentials/bundles and preview snapshot survives later edits', () => {
  const credential = official('u1');
  const before = JSON.stringify(credential);
  const options = { group_mode: 'append', groups: [' vip ', 'vip', 'default'], nickname: '  New name  ' };
  const snapshot = normalizeAccountImportOptions(options, true);
  assert.deepEqual(snapshot, { group_mode: 'append', groups: ['vip', 'default'], nickname: 'New name', run_growth_tasks: false });
  options.groups.push('later'); options.nickname = 'later';
  const request = buildAccountImportRequest(identifyAccountImport(credential), '', ['u1'], snapshot);
  assert.deepEqual(request, { credential, uids: ['u1'], group_mode: 'append', groups: ['vip', 'default'], nickname: 'New name', run_growth_tasks: false });
  assert.equal(JSON.stringify(credential), before);
  assert.equal('password' in request, false);
  assert.equal('bundle' in request, false);
  const encrypted = buildAccountImportRequest(identifyAccountImport(bundle), password, ['u1'], snapshot, true);
  assert.equal(encrypted.bundle, bundle);
  assert.equal('credential' in encrypted, false);
  assert.equal('nickname' in bundle, false);
  assert.deepEqual(buildAccountImportRequest(identifyAccountImport(credential), '', undefined, { group_mode: 'source', groups: ['ignored'], nickname: '  ' }), { credential, group_mode: 'source', run_growth_tasks: false });
});

test('custom group strategies require nonempty valid targets; source strategy keeps original mapping', () => {
  for (const group_mode of ['replace', 'append']) {
    assert.throws(() => normalizeAccountImportOptions({ group_mode, groups: [] }, true));
    assert.throws(() => normalizeAccountImportOptions({ group_mode, groups: [' '] }, true));
    assert.throws(() => normalizeAccountImportOptions({ group_mode, groups: [null] }, true));
    assert.deepEqual(normalizeAccountImportOptions({ group_mode, groups: ['one', 'one', 'two'] }, true), { group_mode, groups: ['one', 'two'], run_growth_tasks: false });
  }
  assert.deepEqual(normalizeAccountImportOptions({ group_mode: 'source' }, false), { group_mode: 'source', run_growth_tasks: false });
  assert.throws(() => normalizeAccountImportOptions({ group_mode: 'unknown' }, true));
});

test('nickname overrides require a single-account document and validate Unicode character counts and controls', () => {
  const nickname = '😀'.repeat(128);
  assert.equal(normalizeAccountImportOptions({ nickname }, true).nickname, nickname);
  assert.throws(() => normalizeAccountImportOptions({ nickname: `${nickname}a` }, true));
  for (const invalid of ['new\nname', 'new\u0000name', 'new\u0085name']) {
    assert.throws(() => normalizeAccountImportOptions({ nickname: invalid }, true));
  }
  assert.throws(() => normalizeAccountImportOptions({ nickname: 'name' }, false));
  // Selecting one UID from a multi-account encrypted document does not permit a nickname override.
  assert.throws(() => buildAccountImportRequest(identifyAccountImport(bundle), password, ['u1'], { nickname: 'name' }));
  assert.deepEqual(normalizeAccountImportOptions({ nickname: '   ' }, false), { group_mode: 'source', run_growth_tasks: false });
});

test('multiple credential files load together and reject duplicate or mixed files', async () => {
  const loader = createAccountImportFileLoader();
  const result = await loader.loadMany([file('one.json', official('u1')), file('two.json', official('u2'))]);
  assert.equal(result.source.kind, 'credentials');
  assert.equal(result.source.sources.length, 2);
  assert.match((await loader.loadMany([file('a', official('u1')), file('b', official('u1'))])).error, /重复/);
  assert.match((await loader.loadMany([file('a', official('u1')), file('bundle', bundle)])).error, /单独选择/);
  assert.equal((await loader.loadMany([file('a', official('u1'))])).source.kind, 'credential');
});

test('multiple imports preview each credential and stop after uncertain write', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2')), file('c', official('u3'))])).source.sources;
  const calls = [];
  const post = async (path, body) => {
    const uid = JSON.parse(body.credential_json).account.uid;
    calls.push([path, uid]);
    if (path.endsWith('/preview')) return { accounts: [{ uid, exists: uid === 'u2' }] };
    if (uid === 'u2') throw new Error('network error');
    return { results: [{ uid, ok: true }] };
  };
  const preview = await runMultiAccountImport(post, sources, { group_mode: 'source' });
  assert.deepEqual(preview.accounts.map(row => row.uid), ['u1', 'u2', 'u3']);
  const result = await runMultiAccountImport(post, sources, { group_mode: 'source' }, ['u1', 'u2', 'u3']);
  assert.equal(result.success, 1);
  assert.equal(result.stop, true);
  assert.match(result.results[2].detail, /未提交/);
  assert.deepEqual(calls.slice(3).map(call => call[1]), ['u1', 'u2']);
});

test('multi-file import stops dispatching after page unmount while preserving the in-flight request', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2'))])).source.sources;
  const waiting = deferred();
  const calls = [];
  let cancelled = false;
  const running = runMultiAccountImport(async (path, body) => {
    calls.push(body.uids[0]);
    await waiting.promise;
    return { results: [{ uid: body.uids[0], ok: true }] };
  }, sources, { group_mode: 'source' }, ['u1', 'u2'], () => cancelled);
  cancelled = true;
  waiting.resolve();
  const result = await running;
  assert.deepEqual(calls, ['u1']);
  assert.equal(result.success, 1);
  assert.equal(result.stop, true);
  assert.equal(result.ok, false);
  assert.deepEqual(result.results.map(row => [row.uid, !!row.cancelled]), [['u1', false], ['u2', true]]);
});

test('progress counts completed requests and cancelled preview cannot look complete', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2'))])).source.sources;
  const events = [];
  let stop = false;
  const result = await runMultiAccountImport(async () => {
    stop = true;
    return { accounts: [{ uid: 'u1' }] };
  }, sources, { group_mode: 'source' }, undefined, () => stop, event => events.push(event));
  assert.deepEqual(events, [{ completed: 0, total: 2 }, { completed: 1, total: 2 }]);
  assert.equal(result.stop, true);
  assert.equal(result.count, 1);
});

test('multi-file preview stops on global errors but continues for individual credential errors', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2'))])).source.sources;
  for (const status of [undefined, 401, 403, 429, 500, 502, 503, 504]) {
    let calls = 0;
    await assert.rejects(runMultiAccountImport(async () => {
      calls++;
      throw Object.assign(new Error('global failure'), { status });
    }, sources, { group_mode: 'source' }), /global failure/);
    assert.equal(calls, 1);
  }
  let calls = 0;
  const preview = await runMultiAccountImport(async () => {
    calls++;
    throw Object.assign(new Error('invalid credential'), { status: 400 });
  }, sources, { group_mode: 'source' });
  assert.equal(calls, 2);
  assert.deepEqual(preview.accounts.map(row => row.issue), ['invalid credential', 'invalid credential']);
});

test('invalid nickname types do not enter failed preview rows', async () => {
  const invalid = official('u1');
  invalid.account.nickname = { unexpected: 'object' };
  const sources = (await createAccountImportFileLoader().loadMany([file('a', invalid), file('b', official('u2'))])).source.sources;
  const preview = await runMultiAccountImport(async (path, body) => {
    const uid = JSON.parse(body.credential_json).account.uid;
    if (uid === 'u1') throw Object.assign(new Error('invalid credential'), { status: 400 });
    return { accounts: [{ uid, nickname: 'valid', exists: false }] };
  }, sources, { group_mode: 'source' });
  assert.equal(preview.accounts[0].nickname, '');
  assert.equal(preview.accounts[0].issue, 'invalid credential');
  assert.equal(preview.accounts[1].nickname, 'valid');
});

test('device preview shows the planned platform decision and never device identity', () => {
  assert.equal(accountImportDeviceLabel({ device_action: 'generate_mac' }), '将新建 Mac 设备档案');
  assert.equal(accountImportDeviceLabel({ device_action: 'preserve', device_platform: 'macos', device_identity: 'private-id' }), '保留已有 Mac 设备档案');
  assert.equal(accountImportDeviceLabel({ device_action: 'preserve', device_platform: 'windows' }), '保留已有 Windows 设备档案');
  assert.equal(accountImportDeviceLabel({ exists: true, device_action: 'generate_mac' }), '已存在，跳过设备处理');
  assert.equal(accountImportDeviceLabel({ device_action: 'skip' }), '已存在，跳过设备处理');
  assert.equal(accountImportDeviceLabel({}), '设备信息待确认');
});

test('official file requests retain original metadata integer precision', async () => {
  const text = '{"auth":{"accessToken":"fake"},"account":{"uid":"offline"},"vendor_counter":9007199254740993}';
  const loaded = await createAccountImportFileLoader().load({ name: 'tmp', size: text.length, text: async () => text });
  const request = buildAccountImportRequest(loaded.source, '', ['offline'], { group_mode: 'source' });
  assert.equal(request.credential_json, text);
  assert.equal('credential' in request, false);
  assert.match(JSON.stringify(request), /9007199254740993/);
});

test('growth is opt-in and the same normalized snapshot is used for preview and apply without mutating credentials', () => {
  assert.equal(normalizeAccountImportOptions({}, true).run_growth_tasks, false);
  const source = identifyAccountImport(official('u1'));
  const original = JSON.stringify(source);
  const options = { run_growth_tasks: true };
  const snapshot = normalizeAccountImportOptions(options, true);
  const preview = buildAccountImportRequest(source, '', undefined, snapshot);
  options.run_growth_tasks = false;
  const apply = buildAccountImportRequest(source, '', ['u1'], snapshot);
  assert.equal(preview.run_growth_tasks, true);
  assert.equal(apply.run_growth_tasks, true);
  assert.equal(buildAccountImportRequest(source, '', undefined, normalizeAccountImportOptions(options, true)).run_growth_tasks, false);
  assert.equal(buildAccountImportRequest(identifyAccountImport(bundle), password, ['u1'], snapshot, true).run_growth_tasks, true);
  assert.equal(JSON.stringify(source), original);
  assert.throws(() => normalizeAccountImportOptions({ run_growth_tasks: 'true' }, true));
});

test('multi-file execution freezes growth and groups before the first request and keeps independent job receipts', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2'))])).source.sources;
  const options = { group_mode: 'append', groups: ['team'], run_growth_tasks: true };
  const calls = [];
  const result = await runMultiAccountImport(async (_path, body) => {
    calls.push(body);
    options.groups[0] = 'changed'; options.run_growth_tasks = false;
    return { results: [{ uid: body.uids[0], ok: true, growth_job_id: `job-${body.uids[0]}`, growth_status: 'queued' }] };
  }, sources, options, ['u1', 'u2']);
  assert.deepEqual(calls.map(body => [body.run_growth_tasks, body.groups]), [[true, ['team']], [true, ['team']]]);
  assert.deepEqual(result.results.map(row => row.growth_job_id), ['job-u1', 'job-u2']);
  assert.equal(result.success, 2);
  assert.equal(result.ok, true);
});

test('stopping a multi-file import leaves the successful growth job and marks only unsent accounts unqueued', async () => {
  const sources = (await createAccountImportFileLoader().loadMany([file('a', official('u1')), file('b', official('u2'))])).source.sources;
  let stop = false;
  const result = await runMultiAccountImport(async (_path, body) => {
    stop = true;
    return { results: [{ uid: body.uids[0], ok: true, growth_job_id: 'job-kept', growth_status: 'queued' }] };
  }, sources, { run_growth_tasks: true }, ['u1', 'u2'], () => stop);
  assert.equal(result.success, 1);
  assert.equal(result.results[0].growth_job_id, 'job-kept');
  assert.equal(result.results[1].cancelled, true);
  assert.equal(result.results[1].growth_job_id, undefined);
  assert.equal(accountImportGrowthLabel(result.results[1], true), '未提交，未排队');
});

test('growth planning distinguishes disabled option, duplicates, pending/failed imports, global and enqueue failure', () => {
  assert.equal(accountImportGrowthLabel({ region: 'cn' }, false), '未开启');
  assert.equal(accountImportGrowthLabel({ region: 'cn', exists: true }, true), '重复账号，未排队');
  assert.equal(accountImportGrowthLabel({ skipped: true }, true), '重复账号，未排队');
  assert.equal(accountImportGrowthLabel({ pending: true, ok: false }, true), '导入待核对，未排队');
  assert.equal(accountImportGrowthLabel({ issue: 'invalid' }, true), '导入未完成，未排队');
  assert.equal(accountImportGrowthLabel({ region: 'global' }, true), '海外版不支持，未排队');
  assert.equal(accountImportGrowthLabel({ ok: true, growth_status: 'enqueue_failed' }, true), '排队失败（导入已成功）');
  assert.equal(accountImportGrowthLabel({ ok: true, growth_status: 'queued' }, true), '已排队，账号保持停用');
});

test('growth progress uses a whitelist, preserves zero, clamps progress and separates job errors from import success', () => {
  const view = importGrowthJobView({ phase: 'done', total: 2, done: 3, current: 'step', unknown: 'private', accessToken: 'private',
    results: [{ task_code: 'chat_5', ok: false, message: 'Authorization: Bearer private-secret', accessToken: 'private' }] });
  assert.equal(view.percent, 100);
  assert.equal(view.active, false);
  assert.equal(view.severity, 'warning');
  assert.match(view.label, /部分任务失败/);
  assert.equal(JSON.stringify(view).includes('private'), false);
  assert.equal(importGrowthJobView({ phase: 'running', total: 0, done: 0 }).percent, 0);
  assert.equal(importGrowthJobView({ phase: 'running' }).active, true);
  assert.equal(importGrowthJobView({ phase: 'failed', error: 'list failed' }).error, 'list failed');
  assert.equal(importGrowthJobView({ phase: 'unexpected' }).active, false);
  assert.equal(accountImportResultType({ ok: true, success: 1, results: [{ ok: true, growth_status: 'enqueue_failed' }] }), 'success');
});
