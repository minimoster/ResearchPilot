import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import ReactMarkdown from 'react-markdown';
import remarkMath from 'remark-math';
import rehypeKatex from 'rehype-katex';
import katex from 'katex';

const render = (children) => renderToStaticMarkup(React.createElement(ReactMarkdown, {
  remarkPlugins: [remarkMath], rehypePlugins: [rehypeKatex], children,
}));

test('both Markdown adapters resolve to the patched KaTeX used by the stylesheet', () => {
  const require = createRequire(import.meta.url);
  const installed = require.resolve('katex');
  for (const adapter of ['rehype-katex', 'micromark-extension-math']) {
    assert.equal(createRequire(require.resolve(adapter)).resolve('katex'), installed);
  }
  assert.equal(katex.version, '0.18.2');
  const css = readFileSync(require.resolve('katex/dist/katex.min.css'), 'utf8');
  assert.match(css, /\.katex/);
});

test('real Markdown pipeline preserves inline math, display math and accessible MathML', () => {
  const html = render('Inline $E = mc^2$.\n\n$$\n\\frac{1}{2} + \\sqrt{x}\n$$');
  assert.match(html, /class="katex"/);
  assert.match(html, /class="katex-display"/);
  assert.match(html, /<math /);
  assert.match(html, /<mfrac>/);
  assert.match(html, /<msqrt>/);
  assert.doesNotMatch(html, /katex-error/);
});

test('malformed math stays readable instead of crashing the Markdown render', () => {
  const html = render('Before $\\frac{$ after');
  assert.match(html, /Before/);
  assert.match(html, /after/);
  assert.match(html, /katex-error/);
});

test('untrusted math cannot inject links or external image requests', () => {
  const html = render('$\\href{https://example.invalid}{link}$ $\\includegraphics{https://example.invalid/image.png}$');
  assert.doesNotMatch(html, /<(?:a|img)\b/);
});

test('inherited renderer options cannot enable trusted rendering', () => {
  const options = Object.create({ trust: true });
  const html = katex.renderToString(String.raw`\href{javascript:alert(1)}{click}`, options);
  assert.doesNotMatch(html, /<a\b/);
});

test('existing prototype pollution cannot enable trust in the real Markdown pipeline', () => {
  const before = Object.getOwnPropertyDescriptor(Object.prototype, 'trust');
  try {
    Object.defineProperty(Object.prototype, 'trust', { value: true, configurable: true, writable: true });
    const html = render('$\\includegraphics{https://example.invalid/image.png}$');
    assert.doesNotMatch(html, /<img\b/);
  } finally {
    if (before) Object.defineProperty(Object.prototype, 'trust', before);
    else delete Object.prototype.trust;
  }
});
