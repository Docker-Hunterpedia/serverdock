import { test as base, expect } from '@playwright/test';

/**
 * Every test fails on a CSP violation in the console, a page error, or a request to
 * any host other than the test server.
 */
export const test = base.extend<{ guard: void }>({
	guard: [
		async ({ page, baseURL }, use) => {
			const problems: string[] = [];
			const host = new URL(baseURL!).host;
			// The 404 page is a document that answers 404; Chromium logs that navigation as a console error.
			const notFoundDocs = new Set<string>();
			page.on('response', (r) => {
				if (r.status() === 404 && r.request().resourceType() === 'document') notFoundDocs.add(r.url());
			});
			page.on('console', (m) => {
				const t = m.text();
				if (/status of 404/.test(t) && notFoundDocs.has(m.location().url)) return;
				if (m.type() === 'error' || /content security policy|refused to/i.test(t)) problems.push(`console ${m.type()}: ${t}`);
			});
			page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`));
			page.on('request', (r) => {
				const u = new URL(r.url());
				if (u.protocol !== 'data:' && u.host !== host) problems.push(`request to another host: ${u.host}`);
			});
			await use();
			expect(problems, 'CSP violations, errors or other-host requests').toEqual([]);
		},
		{ auto: true },
	],
});
export { expect };
