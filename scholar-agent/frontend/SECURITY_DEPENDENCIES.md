# Frontend dependency security boundaries

Reviewed 2026-10-13. These are mitigations and upgrade constraints, not a clean security bill of health. Run `npm audit` when reviewing dependencies; do not suppress advisories or use `npm audit fix --force`.

## Math rendering

KaTeX is a direct, pinned dependency at 0.18.2, the patched release for [GHSA-238p-pmpm-9mq7](https://github.com/KaTeX/KaTeX/security/advisories/GHSA-238p-pmpm-9mq7). The `katex: "$katex"` override also resolves the older ordinary dependency ranges in `rehype-katex` and `micromark-extension-math` to this one reviewed version; it does not override peer dependencies. Remove it once both adapters support a patched version natively.

This is a deliberate compatibility exception. KaTeX 0.17 changes the internal `__defineFunction` API, which this app and these adapters do not use. Version 0.18 prefixes internal CSS classes; the app imports the installed package's own CSS and has no custom selectors against those internals. Keep the stylesheet and renderer on the same version. Offline tests exercise the real Markdown-to-React renderer, inline/display equations, MathML, malformed input, untrusted commands, inherited renderer options and already-polluted `Object.prototype.trust`.

Release notes: [0.17](https://github.com/KaTeX/KaTeX/releases/tag/v0.17.0), [0.18](https://github.com/KaTeX/KaTeX/releases/tag/v0.18.0), [0.18.2 security fix](https://github.com/KaTeX/KaTeX/releases/tag/v0.18.2).

## PDF viewer

`@react-pdf-viewer/core@3.12.0` declares PDF.js peers `^2.16.105 || ^3.0.279`. PDF.js 3.11.174 remains affected by [GHSA-wgrm-67xf-hhpq](https://github.com/mozilla/pdf.js/security/advisories/GHSA-wgrm-67xf-hhpq). Both viewer entry points force `isEvalSupported: false` through `safePdfDocumentParams`, Mozilla's documented workaround; caller values cannot re-enable it. The worker is bundled from the same installed package and origin. Regression tests verify both entry points.

Do not force PDF.js 4/5 into this viewer's incompatible peer range. Replacing it is a separately reviewable UI migration: preserve the current default-layout toolbar/sidebar, search, page navigation, zoom, text selection, highlight popup, close/cancel and selected-text-to-chat callback, including both viewer entry points. Validate actual PDFs, worker loading, zoomed selection geometry, repeated document switching and teardown in a browser before replacing the existing implementation. A clean TypeScript build alone is insufficient. The eval-off mitigation does not establish that old PDF.js is free of other defects.

## Optional native canvas and tar

PDF.js declares an optional Node-only `canvas@2` dependency whose installer uses `@mapbox/node-pre-gyp` and vulnerable `tar@6`. PDF.js explicitly disables `canvas` for browser bundles, and this frontend has no Node PDF rendering path. The critical tar audit entry concerns this dependency/install chain; it must not be described as a browser runtime parser finding.

Project `.npmrc` sets `ignore-scripts=true` so ordinary `npm install` and `npm ci` do not run that optional native installer. Explicit `npm test` and `npm run build` still work. Do not globally omit optional dependencies: Vite/Rolldown and Tailwind use platform-specific optional binaries. Do not force tar 7 or canvas 3 into old consumers merely to erase audit counts. This reduces installer exposure but does not remove the vulnerable package from the lockfile or fix its source. Review the whole chain when migrating the PDF viewer; revisit this boundary before adding any Node-side PDF rendering or enabling dependency lifecycle scripts.

Only deploy the built `dist/` assets, never frontend `node_modules`, to the static web server.
