import type { Page } from '@playwright/test';
import { expect, expectNoAxeViolations, test } from './fixtures';
import { PASSWORD, totp, type TestServer } from './server';

test.use({ storageState: { cookies: [], origins: [] } });

const url = (s: TestServer, path: string) => s.baseURL + path;
const projectIn = (names: string[]) => (_: unknown, info: { project: { name: string } }) => names.includes(info.project.name);
const h1 = (page: Page, name: string | RegExp) => page.getByRole('heading', { level: 1, name });
const alert = (page: Page) => page.getByRole('alert');
const status = (page: Page) => page.getByRole('status');

async function startSetup(page: Page, server: TestServer) {
	await page.goto(url(server, '/setup'));
	await expect(h1(page, 'Prove you own this server')).toBeVisible();
}
async function toAccount(page: Page, server: TestServer, token = server.token()) {
	await startSetup(page, server);
	await page.getByLabel('Setup token').fill(token);
	await page.getByRole('button', { name: 'Continue' }).click();
	await expect(h1(page, 'Admin account and two-factor')).toBeVisible();
}
async function toEnroll(page: Page, server: TestServer) {
	await toAccount(page, server);
	await page.getByLabel('Password', { exact: true }).fill(PASSWORD);
	await page.getByRole('button', { name: 'Continue' }).click();
	await expect(h1(page, 'Add your authenticator')).toBeVisible();
}
const secretOf = async (page: Page) => ((await page.locator('code[aria-labelledby="key-label"]').textContent()) ?? '').replace(/\s/g, '');

test.describe('setup, step 1', () => {
	test('the token screen: real path, expiry, no fingerprint, stepper, axe', async ({ page, server }) => {
		await startSetup(page, server);
		await expect(page).toHaveTitle('Prove you own this server · ServerDock');
		const items = page.getByRole('list', { name: 'Setup progress' }).getByRole('listitem');
		await expect(items).toHaveText(['Step 1 of 2: Prove you own this server', 'Step 2 of 2: Admin account and two-factor']);
		await expect(items.nth(0)).toHaveAttribute('aria-current', 'step');
		await expect(items.nth(1)).not.toHaveAttribute('aria-current', /.+/);
		await expect(page.locator('code', { hasText: 'sudo cat' })).toContainText(server.tokenFile); // the real path
		await expect(page.getByText(/This token works until \d\d:\d\d UTC\. After that, restart ServerDock/)).toBeVisible();
		await expect(page.locator('body')).not.toContainText(/fingerprint:|sha256/i); // never shown on this unauthenticated page
		// A hint paragraph, linked to the field; not placeholder text.
		const field = page.getByLabel('Setup token');
		await expect(field).not.toHaveAttribute('placeholder', /.+/);
		const hintId = (await field.getAttribute('aria-describedby'))!;
		await expect(page.locator(`#${hintId}`)).toContainText('sd_setup_');
		await expect(page.getByRole('button', { name: 'Back' })).toHaveCount(0); // nothing before step 1
		await expectNoAxeViolations(page, 'setup step 1');
	});

	test('copy announces through the status region', async ({ page, server, context, browserName }) => {
		if (browserName === 'chromium') await context.grantPermissions(['clipboard-read', 'clipboard-write']);
		await startSetup(page, server);
		await page.getByRole('button', { name: 'Copy command' }).click();
		await expect(status(page)).toHaveText(/Command copied\.|Could not copy/);
		if (browserName === 'chromium') expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`sudo cat ${server.tokenFile}`);
	});

	test('empty token: error summary takes focus, field error, nothing sent', async ({ page, server }) => {
		await startSetup(page, server);
		let sent = 0;
		page.on('request', (r) => r.url().includes('/setup/enroll') && sent++);
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(alert(page)).toContainText('Enter the setup token');
		await expect(alert(page)).toBeFocused();
		await expect(page.getByLabel('Setup token')).toHaveAttribute('aria-invalid', 'true');
		await alert(page).getByRole('link', { name: 'Setup token' }).click();
		await expect(page.getByLabel('Setup token')).toBeFocused();
		expect(sent).toBe(0);
		await expectNoAxeViolations(page, 'setup step 1 with an error');
	});

	test('wrong token comes back to step 1 with the plain message; typed input is kept', async ({ page, server }) => {
		await toAccount(page, server, 'sd_setup_not-the-token');
		await page.getByLabel('Username').fill('ops.admin');
		await page.getByLabel('Password', { exact: true }).fill(PASSWORD);
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(h1(page, 'Prove you own this server')).toBeVisible();
		await expect(alert(page)).toContainText('That setup token is wrong. Copy it again from the file on the server.');
		await expectNoAxeViolations(page, 'wrong token');
		await page.getByLabel('Setup token').fill(server.token());
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(page.getByLabel('Username')).toHaveValue('ops.admin'); // kept
	});

	test('an expired token says to restart ServerDock', async ({ page, server }) => {
		await toAccount(page, server);
		server.sql('UPDATE setup_state SET expires_at = 1');
		await page.getByLabel('Password', { exact: true }).fill(PASSWORD);
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(h1(page, 'Prove you own this server')).toBeVisible();
		await expect(alert(page)).toContainText('The setup token has expired. Restart ServerDock to get a new one.');
		await expectNoAxeViolations(page, 'expired token');
	});

	test('with no token available the page says to restart ServerDock', async ({ page, server }) => {
		server.sql('DELETE FROM setup_state');
		await startSetup(page, server);
		await expect(page.getByText('There is no setup token right now. Restart ServerDock to get a new one.')).toBeVisible();
		await expectNoAxeViolations(page, 'no token');
	});

	test('a network failure keeps what was typed', async ({ page, server }) => {
		await toAccount(page, server);
		await page.route('**/api/v1/setup/enroll', (r) => r.abort());
		await page.getByLabel('Password', { exact: true }).fill(PASSWORD);
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(alert(page)).toContainText('Could not reach the server. Nothing was changed.');
		await expect(page.getByLabel('Username')).toHaveValue('admin');
		await expect(page.getByLabel('Password', { exact: true })).toHaveValue(PASSWORD);
	});
});

test.describe('setup, step 2', () => {
	test('the rule is shown before typing; short password is refused locally; show/hide', async ({ page, server }) => {
		await toAccount(page, server);
		await expect(page).toHaveTitle('Admin account and two-factor · ServerDock');
		const items = page.getByRole('list', { name: 'Setup progress' }).getByRole('listitem');
		await expect(items.nth(1)).toHaveAttribute('aria-current', 'step');
		const pw = page.getByLabel('Password', { exact: true });
		await expect(page.locator(`#${(await pw.getAttribute('aria-describedby'))!}`)).toContainText('At least 12 characters');
		await expect(pw).toHaveAttribute('autocomplete', 'new-password');
		await expect(page.getByLabel('Username')).toHaveAttribute('autocomplete', 'username');
		await expect(page.getByLabel(/confirm/i)).toHaveCount(0); // no confirm field
		await pw.fill('short');
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(alert(page)).toBeFocused();
		await expect(page.locator('#password-error')).toContainText('at least 12 characters');
		await expectNoAxeViolations(page, 'password error');
		const toggle = page.getByRole('button', { name: 'Show password' });
		await toggle.click();
		await expect(pw).toHaveAttribute('type', 'text');
		await expect(page.getByRole('button', { name: 'Hide password' })).toHaveAttribute('aria-pressed', 'true');
		await expectNoAxeViolations(page, 'account form');
	});

	test('Back returns to step 1 and keeps the username', async ({ page, server }) => {
		await toAccount(page, server);
		await page.getByLabel('Username').fill('me');
		await page.getByRole('button', { name: 'Back' }).click();
		await expect(h1(page, 'Prove you own this server')).toBeFocused();
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(page.getByLabel('Username')).toHaveValue('me');
	});

	test('the authenticator screen: key before QR, QR on light, link, code field, axe', async ({ page, server }) => {
		await toEnroll(page, server);
		const key = page.locator('code[aria-labelledby="key-label"]');
		const qr = page.getByRole('img', { name: 'QR code for your authenticator app' });
		await expect(qr).toBeVisible();
		// DOM order: the text key first, then the QR.
		expect(await key.evaluate((k, q) => !!(k.compareDocumentPosition(q!) & Node.DOCUMENT_POSITION_FOLLOWING), await qr.elementHandle())).toBe(true);
		const secret = await secretOf(page);
		expect(secret).toMatch(/^[A-Z2-7]{16,}$/);
		// The label never contains the secret, nor does a <title> inside the SVG.
		expect(await qr.getAttribute('aria-label')).not.toContain(secret);
		await expect(qr.locator('title')).toHaveCount(0);
		// Dark modules on a light background in every theme, a 4-module quiet zone, a minimum size.
		const look = await qr.evaluate((svg) => {
			const bg = getComputedStyle(svg.querySelector('.qr-bg')!).fill;
			const fg = getComputedStyle(svg.querySelector('.qr-fg')!).fill;
			const vb = svg.viewBox.baseVal;
			const r = svg.getBoundingClientRect();
			return { bg, fg, modules: vb.width, px: r.width };
		});
		expect(look.bg).toBe('rgb(255, 255, 255)');
		expect(look.fg).toBe('rgb(0, 0, 0)');
		expect(look.px / look.modules).toBeGreaterThanOrEqual(2);
		expect(look.px).toBeGreaterThanOrEqual(150);
		const link = page.getByRole('link', { name: 'Open in authenticator app on this device' });
		expect(await link.getAttribute('href')).toMatch(/^otpauth:\/\/totp\/ServerDock(:|%3A)admin\?/);
		const code = page.getByLabel('Code from your authenticator');
		await expect(code).toHaveAttribute('inputmode', 'numeric');
		await expect(code).toHaveAttribute('autocomplete', 'one-time-code');
		await expectNoAxeViolations(page, 'authenticator screen');
	});

	test('a wrong code explains itself and shows the server time; no auto-submit', async ({ page, server }) => {
		await toEnroll(page, server);
		let posts = 0;
		page.on('request', (r) => r.url().includes('/setup/complete') && posts++);
		const code = page.getByLabel('Code from your authenticator');
		await code.fill('123456'); // typing six digits must not submit by itself
		await page.waitForTimeout(500);
		expect(posts).toBe(0);
		await page.getByRole('button', { name: 'Create account' }).click();
		await expect(alert(page)).toContainText(/That code is wrong\. Try the next code your app shows\. The server's time is \d\d:\d\d:\d\d UTC/);
		await expect(code).toHaveValue(''); // a one-time code is not kept
		await expectNoAxeViolations(page, 'wrong code');
	});

	test('the QR code is its own chunk, loaded only on the enrolment screen', async ({ page, server, browser }) => {
		void browser;
		const loaded = new Set<string>();
		page.on('request', (r) => r.url().includes('/_app/immutable/') && r.url().endsWith('.js') && loaded.add(r.url()));
		await page.goto(url(server, '/setup'));
		await expect(h1(page, 'Prove you own this server')).toBeVisible();
		const beforeEnroll = new Set(loaded);
		await page.getByLabel('Setup token').fill(server.token());
		await page.getByRole('button', { name: 'Continue' }).click();
		await page.getByLabel('Password', { exact: true }).fill(PASSWORD);
		await page.getByRole('button', { name: 'Continue' }).click();
		await expect(page.getByRole('img', { name: /QR code/ })).toBeVisible();
		const extra = [...loaded].filter((u) => !beforeEnroll.has(u));
		expect(extra.length, 'the QR chunk loads on demand').toBeGreaterThan(0);
		// And a fresh visit to the sign-in screen never loads any of them.
		const page2 = await page.context().newPage();
		const login = new Set<string>();
		page2.on('request', (r) => r.url().includes('/_app/immutable/') && login.add(r.url()));
		await page2.goto(url(server, '/login')); // fresh install: redirected to /setup, still no QR
		await expect(h1(page2, 'Prove you own this server')).toBeVisible();
		for (const u of extra) expect(login.has(u), `${u} must not load before the enrolment step`).toBe(false);
	});
});

test.describe('first run, over HTTPS, through the screens', () => {
	test.beforeEach(({}, info) => {
		test.skip(!['desktop', 'webkit', 'phone-390'].includes(info.project.name), 'the full flow runs in three projects');
	});

	test('token file to Overview, recovery codes guarded', async ({ page, server }) => {
		await toEnroll(page, server);
		const secret = await secretOf(page);
		await page.getByLabel('Code from your authenticator').fill(totp(secret));
		await page.getByRole('button', { name: 'Create account' }).click();

		await expect(h1(page, 'Save your recovery codes')).toBeFocused();
		await expect(page).toHaveTitle('Save your recovery codes · ServerDock');
		const codes = page.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem');
		await expect(codes).toHaveCount(10);
		await expect(codes.first()).toHaveText(/^[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}$/i);
		await expect(page.getByText('You can make new codes later in Settings › Security.')).toBeVisible();
		await expect(page.getByRole('button', { name: 'Copy all' })).toBeVisible();
		await expect(page.getByRole('button', { name: 'Download as text' })).toBeVisible();
		await expectNoAxeViolations(page, 'recovery codes');

		// Continue is aria-disabled with a stated reason until the box is ticked.
		const cont = page.getByRole('button', { name: 'Continue' });
		await expect(cont).toHaveAttribute('aria-disabled', 'true');
		const reasonId = (await cont.getAttribute('aria-describedby'))!;
		await expect(page.locator(`#${reasonId}`)).toContainText('Tick the box to confirm you saved the codes.');
		const guard = () => page.evaluate(() => {
			const e = new Event('beforeunload', { cancelable: true });
			window.dispatchEvent(e);
			return e.defaultPrevented;
		});
		expect(await guard(), 'leaving is guarded until the codes are saved').toBe(true);
		await cont.click({ force: true }); // aria-disabled: pressing it must do nothing
		await expect(h1(page, 'Save your recovery codes')).toBeVisible(); // did not continue
		await expect(status(page)).toContainText('Tick the box');
		await page.getByLabel('I have saved these codes').check();
		await expect(cont).toHaveAttribute('aria-disabled', 'false');
		expect(await guard()).toBe(false);
		await cont.click();

		await expect(h1(page, 'Overview')).toBeVisible();
		await expect(page.getByRole('contentinfo')).toContainText(/\d+ MB RAM/); // the new session works
		await expectNoAxeViolations(page, 'Overview after first run');
		// Setup is over: /setup sends a signed-in browser home...
		await page.goto(url(server, '/setup'));
		await expect(h1(page, 'Overview')).toBeVisible();
		// ...and one with no session to the sign-in screen.
		const fresh = await page.context().browser()!.newContext({ ignoreHTTPSErrors: true });
		const p2 = await fresh.newPage();
		await p2.goto(url(server, '/setup'));
		await expect(h1(p2, 'Sign in')).toBeVisible();
		await fresh.close();
	});
});

test.describe('sign in', () => {
	test('Signed-out leads to the real sign-in screen, then the code step, then the Overview', async ({ page, server, admin }) => {
		await page.goto(url(server, '/'));
		await expect(page).toHaveURL(/\/signed-out\?reason=ended$/);
		await page.getByRole('link', { name: 'Sign in again' }).click();
		await expect(h1(page, 'Sign in')).toBeFocused();
		await expect(page).toHaveTitle('Sign in · ServerDock');
		await expectNoAxeViolations(page, 'sign in');

		// Empty: both fields are named.
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(alert(page)).toBeFocused();
		await expect(alert(page)).toContainText('Fill in both fields.');
		await expectNoAxeViolations(page, 'sign in, empty');

		// Wrong password: the username stays, the password goes, the message says what to do next.
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill('not the password at all');
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(alert(page)).toContainText('Wrong username or password. Check both and try again. Forgot it? Ask the server\'s operator to run `serverdock admin reset`.');
		await expect(page.getByLabel('Username')).toHaveValue('admin');
		await expect(page.getByLabel('Password')).toHaveValue('');
		await expectNoAxeViolations(page, 'sign in, wrong credentials');

		await page.getByLabel('Password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(h1(page, 'Two-factor sign-in')).toBeFocused();
		await expect(page.getByText('Signing in as admin.')).toBeVisible();
		await expect(page.getByText(/5 minutes/)).toBeVisible();
		const code = page.getByLabel('Code from your authenticator');
		await expect(code).toHaveAttribute('inputmode', 'numeric');
		await expect(code).toHaveAttribute('autocomplete', 'one-time-code');
		await expectNoAxeViolations(page, 'two-factor');

		// Six digits never submit by themselves.
		let posts = 0;
		page.on('request', (r) => r.url().includes('/auth/2fa') && posts++);
		await code.fill('000000');
		await page.waitForTimeout(500);
		expect(posts).toBe(0);
		await page.getByRole('button', { name: 'Verify' }).click();
		await expect(alert(page)).toContainText(/That code is wrong\. Check your device's clock and try the next code\. \d+ tries? left\./);
		await expect(code).toHaveValue('');
		await expectNoAxeViolations(page, 'two-factor, wrong code');

		// The next code (setup used the current one; the server allows one step ahead).
		await code.fill(totp(admin.secret, Date.now() + 30_000));
		await page.getByRole('button', { name: 'Verify' }).click();
		await expect(h1(page, 'Overview')).toBeVisible();
		await expect(page.getByRole('contentinfo')).toContainText(/\d+ MB RAM/);
	});

	test('a recovery code signs in, once', async ({ page, server, admin }) => {
		await page.goto(url(server, '/login'));
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Sign in' }).click();
		await page.getByRole('button', { name: 'Use a recovery code instead' }).click();
		const field = page.getByLabel('Recovery code');
		await expect(page.locator(`#${(await field.getAttribute('aria-describedby'))!}`)).toContainText('Each works once');
		await expectNoAxeViolations(page, 'two-factor, recovery mode');
		await field.fill(admin.recoveryCodes[0]);
		await page.getByRole('button', { name: 'Verify' }).click();
		await expect(h1(page, 'Overview')).toBeVisible();
	});

	test('busy: aria-busy, a status message, aria-disabled, and no second request', async ({ page, server, admin }) => {
		void admin;
		await page.goto(url(server, '/login'));
		let release!: () => void;
		const gate = new Promise<void>((r) => (release = r));
		let calls = 0;
		await page.route('**/api/v1/auth/login', async (route) => {
			calls++;
			await gate;
			await route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":{"code":"invalid_credentials","message":"Wrong username or password. Check both and try again.","request_id":"r"}}' });
		});
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill('whatever password');
		const submit = page.getByRole('button', { name: 'Sign in' });
		await submit.click();
		await expect(page.locator('form')).toHaveAttribute('aria-busy', 'true');
		await expect(status(page)).toHaveText('Signing in…');
		await expect(submit).toHaveAttribute('aria-disabled', 'true');
		await expect(submit).not.toHaveAttribute('disabled', /.*/); // aria-disabled, not disabled: focus stays on it
		await submit.click({ force: true }); // Playwright treats aria-disabled as not actionable
		await expectNoAxeViolations(page, 'sign in, busy');
		release();
		await expect(alert(page)).toContainText('Wrong username or password');
		expect(calls).toBe(1);
		await expect(page.locator('form')).toHaveAttribute('aria-busy', 'false');
	});

	test('a network failure and a busy server are plain, and nothing typed is lost', async ({ page, server, admin }) => {
		void admin;
		await page.goto(url(server, '/login'));
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill('whatever password');
		await page.route('**/api/v1/auth/login', (r) => r.abort());
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(alert(page)).toContainText('Could not reach the server. Nothing was changed.');
		await expect(page.getByLabel('Username')).toHaveValue('admin');
		await page.unroute('**/api/v1/auth/login');
		await page.route('**/api/v1/auth/login', (r) => r.fulfill({ status: 503, headers: { 'Retry-After': '2' }, contentType: 'application/json', body: '{"error":{"code":"busy","message":"x","request_id":"r"}}' }));
		await page.getByLabel('Password').fill('whatever password');
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(alert(page)).toContainText('The server is busy. Try again in a moment. Nothing was changed.');
		await expectNoAxeViolations(page, 'sign in, busy server');
	});

	test('the back-off countdown is announced only at coarse steps', async ({ page, server, admin }) => {
		void admin;
		await page.clock.install();
		await page.goto(url(server, '/login'));
		await page.route('**/api/v1/auth/login', (r) => r.fulfill({ status: 429, headers: { 'Retry-After': '125' }, contentType: 'application/json', body: '{"error":{"code":"rate_limited","message":"x","request_id":"r"}}' }));
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill('whatever password');
		const submit = page.getByRole('button', { name: 'Sign in' });
		await submit.click();
		await expect(status(page)).toHaveText('Too many attempts. Try again in 3 minutes.');
		await expect(submit).toHaveAttribute('aria-disabled', 'true');
		await expect(page.getByText('Try again in 2:05')).toBeVisible(); // the visible count
		await expectNoAxeViolations(page, 'sign in, back-off');
		await page.clock.runFor(3_000); // visible text moves, the announcement does not
		await expect(status(page)).toHaveText('Too many attempts. Try again in 3 minutes.');
		await page.clock.runFor(2_000);
		await expect(status(page)).toHaveText('Try again in 2 minutes.');
		await page.clock.runFor(60_000);
		await expect(status(page)).toHaveText('Try again in 1 minute.');
		await page.clock.runFor(49_000);
		await expect(status(page)).toHaveText('Try again in 1 minute.');
		await page.clock.runFor(1_000);
		await expect(status(page)).toHaveText('Try again in 10 seconds.');
		await page.clock.runFor(10_000);
		await expect(status(page)).toHaveText('You can try again now.');
		await expect(submit).toHaveAttribute('aria-disabled', 'false');
	});

	test('the two-factor step warns a minute before it ends, never moves focus, then says sign in again', async ({ page, server, admin }) => {
		void admin;
		await page.clock.install();
		await page.goto(url(server, '/login'));
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Sign in' }).click();
		const code = page.getByLabel('Code from your authenticator');
		await expect(code).toBeVisible();
		await code.focus();
		await code.pressSequentially('12');
		await page.clock.runFor(239_000);
		await expect(status(page)).toHaveText('');
		await page.clock.runFor(2_000);
		await expect(status(page)).toHaveText('About a minute left to enter your code.');
		await expect(code).toBeFocused(); // the warning never steals focus
		await expect(code).toHaveValue('12');
		await expectNoAxeViolations(page, 'two-factor, about to expire');
		await page.clock.runFor(60_000);
		await expect(alert(page)).toContainText('This step expired. Sign in again.');
		await expectNoAxeViolations(page, 'two-factor, expired');
		await page.getByRole('link', { name: 'Sign in again' }).click();
		await expect(h1(page, 'Sign in')).toBeVisible();
		await expect(page.getByLabel('Username')).toHaveValue('admin'); // kept
	});

	test('a code used twice and an expired pending step have their own messages', async ({ page, server, admin }) => {
		void admin;
		await page.goto(url(server, '/login'));
		await page.getByLabel('Username').fill('admin');
		await page.getByLabel('Password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Sign in' }).click();
		await page.route('**/api/v1/auth/2fa', (r) => r.fulfill({ status: 401, contentType: 'application/json', body: '{"error":{"code":"code_reused","message":"x","request_id":"r"}}' }));
		await page.getByLabel('Code from your authenticator').fill('123456');
		await page.getByRole('button', { name: 'Verify' }).click();
		await expect(alert(page)).toContainText('That code was already used. Wait for the next one.');
		await page.unroute('**/api/v1/auth/2fa');
		server.sql('UPDATE auth_pending SET expires_at = 1');
		await page.getByLabel('Code from your authenticator').fill('123456');
		await page.getByRole('button', { name: 'Verify' }).click();
		await expect(alert(page)).toContainText('This step expired or ran out of tries. Sign in again.');
		await expect(page.getByRole('link', { name: 'Sign in again' })).toBeVisible();
	});

	test('target sizes on the sign-in and setup screens are at least 44 px', async ({ page, server }) => {
		await page.goto(url(server, '/setup'));
		for (const name of ['Continue', 'Copy command']) {
			const b = (await page.getByRole('button', { name }).boundingBox())!;
			expect(b.height, name).toBeGreaterThanOrEqual(44);
		}
		const f = (await page.getByLabel('Setup token').boundingBox())!;
		expect(f.height).toBeGreaterThanOrEqual(44);
	});
});
