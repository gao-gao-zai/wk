import test from 'node:test';
import assert from 'node:assert/strict';
import { accountState, filterAccounts, groupChange, runAccountBatch, accountCSV } from './accountManagement.js';

const filters = { search: '', region: 'all', status: 'all', group: 'all', expiry: 'all' };
test('account status includes auth quarantine and model cooldown, ignores Go zero dates', () => {
  assert.equal(accountState({ until: '0001-01-01T00:00:00Z' }), 'healthy');
  assert.equal(accountState({ auth_until: '2030-01-01T00:00:00Z' }, 1), 'cooling');
  assert.equal(accountState({ model_cooldowns: { model: {} } }), 'model_cooling');
  assert.equal(accountState({ disabled: true, cooling: true }), 'disabled');
});
test('filters search full UID and combine group, region and expiry across the full pool', () => {
  const accounts = Array.from({ length: 1000 }, (_, i) => ({ uid: `uid-${i}`, region: 'cn', token_expires_at: 5 }));
  assert.deepEqual(filterAccounts(accounts, { 'uid-999': ['vip'] }, { ...filters, search: 'UID-999', group: 'vip', expiry: 'expired' }, 10000).map(a => a.uid), ['uid-999']);
  assert.equal(filterAccounts(accounts, {}, { ...filters, region: 'global' }).length, 0);
});
test('group append/remove preserve unrelated memberships and empty removal returns default', () => {
  assert.deepEqual(groupChange(['a', 'b'], ['b', 'c'], 'append'), ['a', 'b', 'c']);
  assert.deepEqual(groupChange(['a', 'b'], ['b'], 'remove'), ['a']);
  assert.deepEqual(groupChange(['a'], ['a'], 'remove'), ['default']);
});
test('batch freezes and deduplicates targets; reports HTTP 200 application failures and exceptions', async () => {
  const input = ['a', 'b', 'a', 'c']; const calls = [];
  const result = await runAccountBatch(input, async uid => {
    calls.push(uid); input.push('unexpected');
    if (uid === 'b') return { ok: false, detail: 'provider rejected' };
    if (uid === 'c') throw new Error('HTTP 409 busy');
    return { ok: true };
  });
  assert.deepEqual(calls, ['a', 'b', 'c']);
  assert.deepEqual(result.map(r => r.status), ['success', 'failed', 'failed']);
});
test('cancel waits for current request but never sends subsequent writes', async () => {
  let stop = false; const calls = [];
  const result = await runAccountBatch(['a', 'b', 'c'], async uid => { calls.push(uid); stop = true; }, { cancelled: () => stop });
  assert.deepEqual(calls, ['a']); assert.deepEqual(result.map(r => r.status), ['success', 'cancelled', 'cancelled']);
});
test('authentication loss or network failure stops the remaining queue', async () => {
  for (const status of [401, 403, undefined]) {
    const calls = [];
    const result = await runAccountBatch(['a', 'b'], async uid => { calls.push(uid); const e = new Error('unavailable'); e.status = status; throw e; });
    assert.deepEqual(calls, ['a']); assert.deepEqual(result.map(r => r.status), ['failed', 'cancelled']);
  }
});
test('inventory CSV quotes fields and neutralizes spreadsheet formulas without exporting credentials', () => {
  const csv = accountCSV([{ uid: 'a', nickname: '=EVIL()', reason: 'one,"two"', accessToken: 'never-export' }], {});
  assert.ok(csv.includes("'=EVIL()")); assert.ok(csv.includes('one,""two""'));
  assert.ok(!csv.includes('never-export'));
});
