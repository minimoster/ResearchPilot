import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

const helperSource = readFileSync(new URL('../src/app/hooks/chatSubmission.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(helperSource, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 } });
const { createSubmissionGate, consumeSubmittedAttachments } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

test('repeated Enter before a render or while waiting starts only one request', () => {
  const gate = createSubmissionGate();
  const first = gate.begin();
  assert.equal(typeof first, 'function');
  assert.equal(gate.begin(), null);
  assert.equal(gate.begin(), null);
  first();
  assert.equal(typeof gate.begin(), 'function');
});

test('a failed request releases the gate for retry through finally', async () => {
  const gate = createSubmissionGate();
  const release = gate.begin();
  await assert.rejects(async () => {
    try {
      await Promise.reject(new Error('backend unavailable'));
    } finally {
      release();
    }
  }, /backend unavailable/);
  assert.equal(typeof gate.begin(), 'function');
});

test('an obsolete duplicate release cannot unlock a newer request', () => {
  const gate = createSubmissionGate();
  const first = gate.begin();
  first();
  const second = gate.begin();
  first();
  assert.equal(gate.begin(), null);
  second();
  assert.equal(typeof gate.begin(), 'function');
});

test('normal successful submission consumes all submitted attachments', () => {
  const current = [{ id: 'paper', name: 'paper.pdf' }, { id: 'data', name: 'data.csv' }];
  assert.deepEqual(consumeSubmittedAttachments(current, ['paper', 'data']), []);
  assert.equal(current.length, 2);
});

test('an older success preserves newly attached files from another session', () => {
  const newer = { id: 'new-session-file', name: 'new.csv' };
  assert.deepEqual(consumeSubmittedAttachments([{ id: 'paper' }, newer], ['paper']), [newer]);
  assert.deepEqual(consumeSubmittedAttachments([newer], ['paper']), [newer]);
});

// Exercise the real hook callbacks with deterministic hook primitives and a
// mocked API, without a browser or additional test-renderer dependencies.
function createChatHarness(api) {
  const cells = [];
  let cursor = 0;
  const react = {
    useState(initial) {
      const index = cursor++;
      if (!(index in cells)) cells[index] = typeof initial === 'function' ? initial() : initial;
      return [cells[index], (next) => { cells[index] = typeof next === 'function' ? next(cells[index]) : next; }];
    },
    useRef(initial) {
      const index = cursor++;
      if (!(index in cells)) cells[index] = { current: initial };
      return cells[index];
    },
    useCallback: (callback) => callback,
    useMemo: (compute) => compute(),
    useEffect: () => {},
  };
  const source = readFileSync(new URL('../src/app/hooks/useScholarChatFlow.ts', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
  const module = { exports: {} };
  const modules = {
    react,
    '../../services/api/scholarApi': api,
    '../constants/uiText': { uiText: { appWelcome: 'Welcome', planGenerated: 'Created', backendError: 'Failed' } },
    './chatSubmission': { createSubmissionGate, consumeSubmittedAttachments },
  };
  new Function('require', 'module', 'exports', compiled)((name) => {
    if (!(name in modules)) throw new Error(`Unexpected import: ${name}`);
    return modules[name];
  }, module, module.exports);
  return () => {
    cursor = 0;
    return module.exports.useScholarChatFlow({ onPlanGraphChanged: () => {} });
  };
}

const upload = { id: 'uploaded-paper', name: 'paper.pdf', size: 10 };
const planResponse = { plan_graph: { id: 'plan-1', nodes: [], edges: [], status: 'pending' } };

test('the real chat flow clears attachments after an ordinary successful plan', async () => {
  const render = createChatHarness({ uploadFile: async () => upload, createPlan: async () => planResponse });
  let flow = render();
  await flow.handleAttachFiles([{ name: 'paper.pdf' }]);
  flow = render();
  assert.equal(flow.pendingAttachments.length, 1);
  await flow.handleSendMessage();
  flow = render();
  assert.equal(flow.pendingAttachments.length, 0);
  assert.equal(flow.activePlanId, 'plan-1');
  assert.equal(flow.loading, false);
});

test('the real chat flow suppresses duplicate callbacks before and after rerender', async () => {
  let resolvePlan;
  let requests = 0;
  const render = createChatHarness({
    uploadFile: async () => upload,
    createPlan: () => { requests += 1; return new Promise((resolve) => { resolvePlan = resolve; }); },
  });
  let flow = render();
  await flow.handleAttachFiles([{ name: 'paper.pdf' }]);
  flow = render();
  const first = flow.handleSendMessage();
  await flow.handleSendMessage();
  flow = render();
  assert.equal(flow.loading, true);
  await flow.handleSendMessage();
  assert.equal(requests, 1);
  resolvePlan(planResponse);
  await first;
  flow = render();
  assert.equal(flow.loading, false);
  assert.equal(flow.chatHistory.filter((message) => message.role === 'user').length, 1);
});
