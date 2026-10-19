import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/app/hooks/planUpdateGuard.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { createPlanUpdateGuard, cancelPlanWithRecovery } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

test('a late terminal snapshot cannot restore old results after retry progress', async () => {
  const updates = createPlanUpdateGuard();
  const oldRun = updates.advance();
  let finishOldGet;
  const oldGet = new Promise((resolve) => { finishOldGet = resolve; });
  let displayed = { status: 'failed', result: 'old' };
  const applying = oldGet.then((snapshot) => { if (oldRun()) displayed = snapshot; });
  const retry = updates.advance();
  if (retry()) displayed = { status: 'pending', result: '' };
  const newRun = updates.advance();
  if (newRun()) displayed = { status: 'in_progress', result: '' };
  finishOldGet({ status: 'failed', result: 'old' });
  await applying;
  assert.deepEqual(displayed, { status: 'in_progress', result: '' });
  assert.equal(oldRun(), false);
  assert.equal(retry(), false);
  assert.equal(newRun(), true);
});

test('reset or cancellation invalidates callbacks from a prior stream', () => {
  const updates = createPlanUpdateGuard();
  const oldStream = updates.advance();
  const cancel = updates.advance();
  assert.equal(oldStream(), false);
  assert.equal(cancel(), true);
  updates.advance();
  assert.equal(cancel(), false);
});

for (const reason of ['completion raced with cancellation (409)', 'network failure']) {
  test(`failed cancel reconnects after ${reason}`, async () => {
    const updates = createPlanUpdateGuard();
    const oldStream = updates.advance();
    const cancel = updates.advance();
    let newStream;
    let displayed = 'in_progress';
    let failure;
    await cancelPlanWithRecovery(
      async () => { throw new Error(reason); },
      cancel,
      () => { displayed = 'canceled'; },
      () => { newStream = updates.advance(); },
      (error) => { failure = error.message; },
    );
    assert.equal(oldStream(), false);
    assert.equal(failure, reason);
    assert.equal(newStream(), true);
    if (newStream()) displayed = 'completed'; // authoritative terminal replay
    assert.equal(displayed, 'completed');
  });
}

test('an obsolete cancel failure cannot reconnect over a newer plan', async () => {
  const updates = createPlanUpdateGuard();
  const cancel = updates.advance();
  let reject;
  const request = new Promise((_, rejectRequest) => { reject = rejectRequest; });
  let reconnects = 0;
  const work = cancelPlanWithRecovery(() => request, cancel, () => {}, () => { reconnects++; }, () => {});
  updates.advance();
  reject(new Error('old cancellation'));
  await work;
  assert.equal(reconnects, 0);
});
