import type { PdfJs } from '@react-pdf-viewer/core';
import pdfWorkerUrl from 'pdfjs-dist/build/pdf.worker.min.js?url';

// Keep the worker on the same origin and version as the installed PDF.js.
export { pdfWorkerUrl };

// CVE-2024-4367 / GHSA-wgrm-67xf-hhpq: mandatory while the viewer uses PDF.js 3.x.
// https://github.com/mozilla/pdf.js/security/advisories/GHSA-wgrm-67xf-hhpq
export const safePdfDocumentParams = (options: PdfJs.GetDocumentParams) => ({
  ...options,
  isEvalSupported: false,
});
