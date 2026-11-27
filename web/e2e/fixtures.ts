import AxeBuilder from '@axe-core/playwright';
import { expect, test as base, type Page } from '@playwright/test';
import { isDockerServer, signInWithRecoveryCode, startServer, totp, type Admin, type TestServer } from './server';

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
			// What failed to load among the app's own files: not a problem by itself (a navigation aborts requests),
			// but when a console error like "Unable to preload CSS" does fail the test, it names its cause.
			const assetTrouble: string[] = [];
			page.on('requestfailed', (r) => {
				if (new URL(r.url()).pathname.startsWith('/_app/')) assetTrouble.push(`${r.url()} failed: ${r.failure()?.errorText}`);
			});
			page.on('response', (r) => {
				if (new URL(r.url()).pathname.startsWith('/_app/') && (r.status() < 200 || r.status() > 299)) assetTrouble.push(`${r.url()} answered ${r.status()}`);
				// The Overview asks system.exposure on every load; a server started without Docker answers 503 by design and the page says it could not check.
				if (r.status() >= 500 && r.url().includes('/api/v1/') && !(r.url().endsWith('/actions/system.exposure') && !isDockerServer(r.url()))) problems.push(`API ${r.status()} on ${new URL(r.url()).pathname} (call allow5xx() if the test provokes it)`);
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
			if (problems.length && assetTrouble.length) problems.push('app files that failed or answered non-2xx: ' + assetTrouble.join('; '));
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

/** Makes the next step-up-protected action ask for a fresh code (the 5-minute window ends now). */
export const expireStepUp = (server: TestServer) => server.sql('UPDATE sessions SET mfa_fresh_until = 1');

/** The first code a step-up dialog accepts after the sign-in (one step ahead of any code already used). */
export const nextCode = (admin: Admin) => totp(admin.secret, Date.now() + 30_000);

/**
 * Calls an action as the signed-in page (cookies, CSRF and Origin like the SPA). For the
 * setup of tests that deploy for real: the page then shows what the API made.
 */
export async function callAction<T = unknown>(page: Page, server: TestServer, name: string, input: object = {}, confirm?: string): Promise<T> {
	const session = await page.request.get(`${server.baseURL}/api/v1/auth/session`);
	const { csrf } = (await session.json()) as { csrf: string };
	const res = await page.request.post(`${server.baseURL}/api/v1/actions/${name}`, {
		data: input,
		headers: { 'X-SD-CSRF': csrf, Origin: server.baseURL, ...(confirm ? { 'SD-Confirm': confirm } : {}) }
	});
	if (!res.ok()) throw new Error(`${name} ${res.status()}: ${await res.text()}`);
	return ((await res.json()) as { data: T }).data;
}

/** Waits until a deploy of a real service reached the status (it fails fast, with the reason, on a failed deploy). */
export async function waitForDeploy(server: TestServer, ref: string, number: number, status = 'succeeded', timeout = 180_000) {
	const [project, name] = ref.split('/');
	await expect
		.poll(
			() => {
				const r = server.query<{ status: string; reason: string | null }>(
					`SELECT d.status, d.reason FROM deploys d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id WHERE p.name = ? AND s.name = ? AND d.number = ?`,
					project,
					name,
					number
				)[0];
				return r ? (r.status === 'failed' && status !== 'failed' ? `failed: ${r.reason}` : r.status) : 'no such deploy';
			},
			{ timeout, message: `deploy #${number} of ${ref}` }
		)
		.toBe(status);
}
