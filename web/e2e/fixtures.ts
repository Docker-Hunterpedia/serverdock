import AxeBuilder from '@axe-core/playwright';
import { expect, test as base, type Page } from '@playwright/test';
import { signInWithRecoveryCode, startServer, type Admin, type TestServer } from './server';

export const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'];

/** Zero axe violations under the A5 tags, with a readable failure message. */
export async function expectNoAxeViolations(page: Page, what: string) {
	const { violations } = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
	const summary = violations.map((v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`);
	expect(summary, `axe violations on ${what}`).toEqual([]);
}

// Every test also fails on a CSP violation, a page error or a console error.
type Fixtures = { problems: string[]; server: TestServer; admin: Admin; allow5xx: () => void };

// `server` is a fresh binary with nothing set up (state-changing tests own one, so
// back-off counters and the one-time setup token are never shared); `admin` has also
// done the first run. Use them with an empty storageState.
const allowed = { value: false };
export const test = base.extend<Fixtures>({
	server: async ({}, use) => {
		const server = await startServer();
		await use(server);
		await server.stop();
	},
	admin: async ({ server }, use) => {
		await use(await server.firstRun());
	},
	allow5xx: async ({}, use) => {
		// A test that provokes a server error on purpose calls this first.
		await use(() => (allowed.value = true));
	},
	problems: [
		async ({ page }, use) => {
			const problems: string[] = [];
			allowed.value = false;
			page.on('response', (r) => {
				// The Overview asks system.exposure on every load; a server with no Docker answers 503 by design and the page says it could not check.
				if (r.status() >= 500 && r.url().includes('/api/v1/') && !r.url().endsWith('/actions/system.exposure')) problems.push(`API ${r.status()} on ${new URL(r.url()).pathname} (call allow5xx() if the test provokes it)`);
			});
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
			if (allowed.value) problems.splice(0, problems.length, ...problems.filter((p) => !p.startsWith('API 5')));
			expect(problems, 'CSP violations, page errors or console errors').toEqual([]);
		},
		{ auto: true }
	]
});
export { expect };

/** Counts the requests a page sends to /api/ from now on (the no-requests-while-idle tests). */
export function countApiRequests(page: Page) {
	const seen: string[] = [];
	page.on('request', (r) => {
		if (r.url().includes('/api/')) seen.push(new URL(r.url()).pathname);
	});
	return { seen, reset: () => seen.splice(0, seen.length) };
}

/**
 * Signs the page in to an own server (state-changing and seeded tests) with the
 * admin's recovery code number `n` (each code works once; there are 10). Use the
 * server's absolute URL afterwards: page.goto(`${server.baseURL}/services`).
 */
export async function signIn(page: Page, server: TestServer, admin: Admin, n = 0) {
	const state = await signInWithRecoveryCode(server, admin.recoveryCodes[n]);
	await page.context().addCookies(state.cookies);
}

/**
 * A deploy (or restart, or delete) queued in the same second the account was created
 * fails closed ("the password or authenticator changed after this job was queued":
 * internal/auth/jobactor.go). Call this after firstRun() in any test that queues a job.
 */
export const letTheAccountAge = () => new Promise((r) => setTimeout(r, 1500));
