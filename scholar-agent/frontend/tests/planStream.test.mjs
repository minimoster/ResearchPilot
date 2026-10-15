import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

// Use the project's existing compiler so tests also work on Node 20.
const helperSource = readFileSync(new URL('../src/services/api/planStream.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(helperSource, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 } });
const { createExecutionScope, subscribeToPlanEvents } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

class FakeEventSource extends EventTarget {
  readyState = 1;
  closeCount = 0;
  close() { this.readyState = 2; this.closeCount += 1; }
  emit(event, id = '') {
    this.dispatchEvent(new MessageEvent('plan_event', { data: JSON.stringify(event), lastEventId: id }));
  }
  fail(state) { this.readyState = state; this.onerror?.(new Event('error')); }
}

const event = { plan_id: 'plan-a', event_type: 'task_log', payload: { message: 'hello' }, timestamp: '2026-10-13T00:00:00Z' };

function setup() {
  const source = new FakeEventSource();
  const received = [];
  let errors = 0;
  subscribeToPlanEvents(source, 'plan-a', { onPlanEvent: (value) => received.push(value), onError: () => { errors += 1; } });
  return { source, received, errors: () => errors };
}

test('a transient interruption keeps browser reconnection enabled and receives later events', () => {
  const { source, received, errors } = setup();
  source.emit(event);
  source.fail(0);
  assert.equal(source.closeCount, 0);
  assert.equal(errors(), 0);
  source.readyState = 1;
  source.emit({ ...event, event_type: 'plan_completed' });
  assert.equal(received.length, 2);
});

test('durable replay after reconnect does not duplicate logs', () => {
  const { source, received } = setup();
  source.emit(event);
  source.fail(0);
  source.emit(event);
  source.emit({ ...event, timestamp: '2026-10-13T00:00:01Z' });
  assert.equal(received.length, 2);
});

test('closed streams discard queued events from the previous session', () => {
  const { source, received } = setup();
  source.close();
  source.emit(event);
  assert.deepEqual(received, []);
});

test('events from another plan and malformed frames are ignored', () => {
  const { source, received } = setup();
  source.emit({ ...event, plan_id: 'plan-b' });
  source.dispatchEvent(new MessageEvent('plan_event', { data: '{invalid' }));
  source.dispatchEvent(new MessageEvent('plan_event', { data: 'null' }));
  source.emit(event);
  assert.deepEqual(received, [event]);
});

test('permanently closed streams notify the caller once', () => {
  const { source, errors } = setup();
  source.fail(2);
  source.fail(2);
  assert.equal(errors(), 1);
  assert.equal(source.closeCount, 1);
});

test('scope changes invalidate HTTP and direct-stream continuations, including switching back', () => {
  const scope = createExecutionScope();
  const sessionA = scope.capture();
  assert.equal(sessionA(), true);
  scope.invalidate();
  const sessionB = scope.capture();
  assert.equal(sessionA(), false);
  assert.equal(sessionB(), true);
  scope.invalidate();
  assert.equal(sessionA(), false);
  assert.equal(sessionB(), false);
  assert.equal(scope.capture()(), true);
});
