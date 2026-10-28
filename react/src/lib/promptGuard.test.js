import test from 'node:test';
import assert from 'node:assert/strict';
import { candidateView, httpDescription, jobView, requestAttempted, verificationStages } from './promptGuard.js';

const screenshotJob = { kind: 'verify', status: 'inconclusive', calls: 1, budget: 64, started: '2026-10-28T14:25:55Z', trace: [{ step: 'control', characters: 28, outcome: 'inconclusive', http: 0, reason: 'account_busy_or_unavailable' }] };

test('legacy screenshot explains zero upstream requests and skipped full verification', () => {
  const view = jobView(screenshotJob);
  assert.equal(view.requests, 0);
  assert.equal(view.unknown, 0);
  assert.equal(view.budget, 2);
  assert.equal(view.checks, 1);
  assert.match(view.headline, /原账号忙碌或不可用/);
  assert.match(view.description, /后续完整提示词检查未执行/);
  assert.equal(httpDescription(screenshotJob.trace[0]), '未发送');
  assert.equal(verificationStages(screenshotJob)[1].state, '未执行');
});

test('HTTP zero with unknown old cause is never fabricated into zero requests', () => {
  const row = { outcome: 'inconclusive', http: 0 };
  assert.equal(requestAttempted(row), null);
  const view = jobView({ ...screenshotJob, trace: [{ ...row, step: 'control' }] });
  assert.equal(view.unknown, 1);
  assert.match(view.requestLabel, /未记录/);
  assert.equal(httpDescription(row), '未记录响应');
});

test('network failure is shown as an attempt without HTTP response', () => {
  const row = { step: 'control', outcome: 'inconclusive', http: 0, request_attempted: true, reason: 'transport_error' };
  assert.equal(jobView({ ...screenshotJob, trace: [row] }).requests, 1);
  assert.equal(httpDescription(row), '未收到响应');
});

test('cached pass contributes a check but no new upstream attempt', () => {
  const row = { step: 'bisect', outcome: 'pass', http: 200, request_attempted: true };
  const view = jobView({ kind: 'locate', status: 'located', calls: 1, budget: 64, trace: [row, { ...row, cached_from: 1 }] });
  assert.equal(view.requests, 1);
  assert.equal(view.checks, 2);
  assert.equal(view.cached, 1);
  assert.equal(httpDescription({ ...row, cached_from: 1 }), '复用先前响应');
});

test('failed verification labels retained candidate as historical', () => {
  const view = candidateView({ candidate: 'Claude identity', candidate_minimal: true, job: screenshotJob }, 4);
  assert.equal(view.historical, true);
  assert.match(view.description, /没有重新定位/);
  assert.match(view.title, /历史定位片段/);
});

test('candidate needs matching task time and revision to be current', () => {
  const item = { candidate: 'trigger', candidate_minimal: true, candidate_revision: 4, candidate_located_at: '2026-10-28T14:26:00Z', job: { ...screenshotJob, kind: 'locate', status: 'located' } };
  assert.equal(candidateView(item, 4).historical, false);
  assert.equal(candidateView(item, 5).historical, true);
});

test('cancelled check keeps the cancellation explanation', () => {
  assert.equal(jobView({ ...screenshotJob, status: 'cancelled', note: '用户已取消' }).description, '用户已取消');
});
