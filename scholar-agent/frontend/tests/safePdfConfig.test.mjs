import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import ts from 'typescript';

const source = readFileSync(new URL('../src/features/pdf-viewer/safePdfConfig.ts', import.meta.url), 'utf8');
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
const module = { exports: {} };
new Function('require', 'module', 'exports', compiled)((name) => {
  assert.equal(name, 'pdfjs-dist/build/pdf.worker.min.js?url');
  return { default: '/assets/local-pdf-worker.js' };
}, module, module.exports);
const { safePdfDocumentParams, pdfWorkerUrl } = module.exports;

test('PDF parameters always disable eval, overriding a caller-provided true', () => {
  const original = { url: '/api/pdf-proxy?url=paper.pdf', withCredentials: true, httpHeaders: { 'X-Test': 'value' }, isEvalSupported: true };
  const safe = safePdfDocumentParams(original);
  assert.equal(safe.isEvalSupported, false);
  assert.equal(safePdfDocumentParams({}).isEvalSupported, false);
  assert.equal(safe.url, original.url);
  assert.equal(safe.withCredentials, true);
  assert.deepEqual(safe.httpHeaders, original.httpHeaders);
  assert.equal(original.isEvalSupported, true);
  assert.equal(pdfWorkerUrl, '/assets/local-pdf-worker.js');
});

for (const file of ['../src/App.tsx', '../src/features/pdf-viewer/PdfPanel.tsx']) {
  test(`${file} wires every PDF viewer to shared safe parameters and bundled worker`, () => {
    const content = readFileSync(new URL(file, import.meta.url), 'utf8');
    const ast = ts.createSourceFile(file, content, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    let viewers = 0;
    let workers = 0;
    const namedImports = new Set();
    function walk(node) {
      if (ts.isImportDeclaration(node) && node.moduleSpecifier.text.endsWith('/safePdfConfig')) {
        for (const element of node.importClause.namedBindings.elements) namedImports.add(element.name.text);
      }
      if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
        const tag = node.tagName.getText(ast);
        if (tag === 'Viewer' || tag === 'Worker') {
          const expected = tag === 'Viewer' ? ['transformGetDocumentParams', 'safePdfDocumentParams'] : ['workerUrl', 'pdfWorkerUrl'];
          const attribute = node.attributes.properties.find((property) => ts.isJsxAttribute(property) && property.name.text === expected[0]);
          assert.ok(attribute, `${tag} must set ${expected[0]}`);
          assert.equal(attribute.initializer.expression.getText(ast), expected[1]);
          if (tag === 'Viewer') viewers += 1;
          else workers += 1;
        }
      }
      ts.forEachChild(node, walk);
    }
    walk(ast);
    assert.ok(viewers > 0);
    assert.ok(workers > 0);
    assert.ok(namedImports.has('safePdfDocumentParams'));
    assert.ok(namedImports.has('pdfWorkerUrl'));
    assert.equal(content.includes('https://unpkg.com/pdfjs-dist'), false);
  });
}
