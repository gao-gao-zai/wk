import test from 'node:test';
import assert from 'node:assert/strict';
import { freshness, isActive, pollCollection, progress } from './collection.js';

test('progress uses completed items, unknown snapshots stay unknown', () => {
  assert.equal(progress({ total: 300, done: 128 }), 43);
  assert.equal(isActive({ status: 'cancelling' }), true);
  assert.equal(isActive({ status: 'interrupted' }), false);
  assert.equal(freshness({ state: 'failed', last_success: 0 }), '尚无成功快照');
});
test('polling never overlaps and unmount aborts pending response', async () => {
  let receive = 0, scheduled = 0, signal, resolve;
  const stop = pollCollection(s => { signal = s; return new Promise(r => { resolve = r; }); }, () => receive++, () => {}, { schedule: () => scheduled++, unschedule: () => {} });
  assert.equal(scheduled, 0);
  stop(); resolve({ jobs: [] }); await new Promise(r => setImmediate(r));
  assert.equal(signal.aborted, true); assert.equal(receive, 0); assert.equal(scheduled, 0);
});
