import test from 'node:test';
import assert from 'node:assert/strict';
import { mergeSavedConfig, createTaskPoller, taskProgress, validTaskPlatform, compatibleGroups } from './autoEnroll.js';

test('status poller continues through idle, start and transient errors', async () => {
  const timers = new Map(); let seq = 0, step = 0; const published = [], errors = [];
  const p = createTaskPoller(async () => { step++; if (step === 2) throw new Error('fixture offline'); return { running: step > 2 }; }, x => published.push(x), e => errors.push(e.message), (cb, delay) => { timers.set(++seq, { cb, delay }); return seq; }, id => timers.delete(id));
  await p.refresh(); assert.equal(timers.get(1).delay, 5000);
  await p.refresh(); assert.equal(errors.length, 1); assert.equal(timers.get(2).delay, 3000);
  await p.refresh(); assert.equal(published.at(-1).running, true); assert.equal(timers.get(3).delay, 2000);
  p.close(); assert.equal(timers.size, 0);
});
test('in-flight status request cannot reschedule after unmount', async () => {
  let resolve; let scheduled = 0, published = 0;
  const p = createTaskPoller(() => new Promise(r => { resolve = r; }), () => published++, () => {}, () => scheduled++, () => {});
  const request = p.refresh(); p.close(); resolve({ running: true }); await request;
  assert.equal(scheduled, 0); assert.equal(published, 0);
});
test('manual refresh is serialized with current request', async () => {
  let resolve, calls = 0, delay;
  const p = createTaskPoller(() => { calls++; return new Promise(r => { resolve = r; }); }, () => {}, () => {}, (_fn, ms) => { delay = ms; return 1; }, () => {});
  const first = p.refresh(); await p.refresh(); assert.equal(calls, 1);
  resolve({ running: true }); await first; assert.equal(delay, 0); p.close();
});
test('progress uses the actual task goal and invalid supplier choices reset', () => {
  assert.deepEqual(taskProgress({ want: 20, ok: 7 }), { want: 20, ok: 7, percent: 35 });
  assert.equal(validTaskPlatform('both', { ecode: true, haozhuma: false }), 'auto');
  assert.deepEqual(compatibleGroups([{ name: 'cn', region: 'cn' }, { name: 'global', region: 'global' }]).map(x => x.name), ['cn']);
});

test('saved profile refresh preserves nested schedule values including false', () => {
  const merged = mergeSavedConfig({ autoenroll_growth_tasks: true, checkin_hours: [1] }, { schedule: { autoenroll_growth_tasks: false, checkin_hours: [9] }, sms: { provider: 'ecode' } });
  assert.equal(merged.autoenroll_growth_tasks, false);
  assert.deepEqual(merged.checkin_hours, [9]);
  assert.equal(merged.sms.provider, 'ecode');
});
