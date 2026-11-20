import AxeBuilder from '@axe-core/playwright';
import { expect, test as base, type Page } from '@playwright/test';
import { startServer, type Admin, type TestServer } from './server';

export const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'];

/** Zero axe violations under the A5 tags, with a readable failure message. */
export async function expectNoAxeViolations(page: Page, what: string) {
	const { violations } = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
	const summary = violations.map((v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`);
	expect(summary, `axe violations on ${what}`).toEqual([]);
}

// Every test also fails on a CSP violation, a page error or a console error.
type Fixtures = { problems: string[]; server: TestServer; admin: Admin };

// `server` is a fresh binary with nothing set up (state-changing tests own one, so
// back-off counters and the one-time setup token are never shared); `admin` has also
// done the first run. Use them with an empty storageState.
export const test = base.extend<Fixtures>({
	server: async ({}, use) => {
		const server = await startServer();
		await use(server);
		await server.stop();
	},
	admin: async ({ server }, use) => {
		await use(await server.firstRun());
	},
	problems: [
		async ({ page }, use) => {
			const problems: string[] = [];
			await page.addInitScript(() => {
				addEventListener('securitypolicyviolation', (e) => {
					console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI);
				});
			});
			page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
			page.on('console', (m) => {
				if (m.type() !== 'error') return;
				// The browser itself logs every 4xx/5xx answer of an API call; tests that
				// provoke one on purpose assert the screen, not this line.
				if (m.text().startsWith('Failed to load resource') && m.location().url.includes('/api/v1/')) return;
				problems.push('console: ' + m.text());
			});
			await use(problems);
			expect(problems, 'CSP violations, page errors or console errors').toEqual([]);
		},
		{ auto: true }
	]
});
export { expect };
