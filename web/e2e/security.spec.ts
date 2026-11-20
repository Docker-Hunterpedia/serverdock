import type { Page } from '@playwright/test';
import { request } from '@playwright/test';
import { expect, expectNoAxeViolations, test } from './fixtures';
import { PASSWORD, signInWithRecoveryCode, totp, type Admin, type TestServer } from './server';

test.use({ storageState: { cookies: [], origins: [] } });

const url = (s: TestServer, path: string) => s.baseURL + path;
const isPhone = (page: Page) => (page.viewportSize()?.width ?? 1280) < 768;
const h1 = (page: Page, name: string) => page.getByRole('heading', { level: 1, name });
const status = (page: Page) => page.locator('.alerts [role="status"]'); // the page's one status region (the certificate has its own)
const alert = (page: Page) => page.getByRole('alert');
const dialog = (page: Page, name: string | RegExp) => page.getByRole('dialog', { name });
const ABSOLUTE = /\d{4}-\d\d-\d\d \d\d:\d\d UTC/;

async function signIn(page: Page, server: TestServer, admin: Admin, n = 0) {
	// The admin fixture's own first-run session is not part of what these tests count.
	if (n === 0) server.sql('DELETE FROM sessions');
	const state = await signInWithRecoveryCode(server, admin.recoveryCodes[n]);
	await page.context().addCookies(state.cookies);
	return state;
}
async function openSecurity(page: Page, server: TestServer) {
	await page.goto(url(server, '/settings/security'));
	await expect(h1(page, 'Security')).toBeVisible();
	await expect(page.getByRole('heading', { level: 2, name: 'Sessions' })).toBeVisible();
}
async function api(server: TestServer, state: Awaited<ReturnType<typeof signInWithRecoveryCode>>) {
	const ctx = await request.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true, storageState: state, extraHTTPHeaders: { Origin: server.baseURL } });
	const csrf = (await (await ctx.get('/api/v1/auth/session')).json()).csrf;
	return { ctx, csrf };
}
async function tokenStatus(server: TestServer, token: string) {
	const ctx = await request.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true });
	const res = await ctx.post('/api/v1/actions/system.info', { data: {}, headers: { Authorization: `Bearer ${token}` } });
	const code = res.status();
	await ctx.dispose();
	return code;
}
async function createToken(page: Page, name: string, scope = 'read', days = '90') {
	await page.getByLabel('Name', { exact: true }).fill(name);
	await page.getByLabel('Scope').selectOption(scope);
	await page.getByLabel('Expires in (days)').fill(days);
	await page.getByRole('button', { name: 'Create token' }).click();
	await expect(page.locator('#new-token')).toBeVisible();
	return ((await page.locator('#new-token').textContent()) ?? '').trim();
}

test.describe('Settings > Security: the page', () => {
	test('loads with its facts, sub-nav, tables and no axe violations', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await expect(page).toHaveTitle('Security · ServerDock');
		await expect(h1(page, 'Security')).toHaveCount(1);
		const sub = page.getByRole('navigation', { name: 'Settings' });
		await expect(sub.getByRole('link')).toHaveText(['Security']); // only Security (QM2-3)
		await expect(sub.getByRole('link', { name: 'Security' })).toHaveAttribute('aria-current', 'page');
		// The panel certificate and where/when the admin was created.
		await expect(page.locator('#fingerprint')).toHaveText(/^[0-9A-Fa-f:]{20,}$|^[0-9a-f]{64}$/i);
		await expect(page.locator('#admin-created')).toHaveText(/^Admin created from \S+ at \d{4}-\d\d-\d\d \d\d:\d\d UTC$/);
		await expect(page.getByText(/The panel certificate (expires in|has expired)/)).toHaveCount(0); // 825 days left
		// Sessions: a real table (caption, th scope) on wide screens, cards on phones.
		if (isPhone(page)) {
			await expect(page.getByRole('table')).toHaveCount(0);
			await expect(page.getByRole('list', { name: 'Signed-in sessions' }).getByRole('listitem')).toHaveCount(1);
		} else {
			const table = page.getByRole('table', { name: 'Signed-in sessions' });
			await expect(table.locator('caption')).toHaveText('Signed-in sessions');
			await expect(table.locator('thead th[scope="col"]')).toHaveCount(6);
			await expect(table.locator('tbody th[scope="row"]')).toHaveCount(1);
		}
		await expect(page.getByText('This device').locator('visible=true').first()).toBeVisible();
		await expect(page.getByText(ABSOLUTE).first()).toBeVisible(); // absolute times with the zone
		await expect(page.getByText('No API tokens yet.')).toBeVisible();
		await expectNoAxeViolations(page, 'Security page');
	});

	test('a certificate with fewer than 60 days left shows a warning', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/system.panel_cert', async (route) => {
			const res = await route.fetch();
			const body = await res.json();
			body.data.not_after = new Date(Date.now() + 30 * 86_400_000 + 3_600_000).toISOString();
			body.data.renew_soon = true;
			await route.fulfill({ response: res, json: body });
		});
		await openSecurity(page, server);
		await expect(page.getByRole('status').filter({ hasText: 'The panel certificate expires in 30 days' })).toBeVisible();
		await expectNoAxeViolations(page, 'Security page with a certificate warning');
	});

	test('an error loading the page uses the one alert', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/tokens.list', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"action_failed","message":"the action failed","request_id":"r"}}' }));
		await page.goto(url(server, '/settings/security'));
		await expect(alert(page)).toContainText('the action failed');
		await expect(page.getByRole('alert')).toHaveCount(1);
		await expectNoAxeViolations(page, 'Security page, load error');
	});
});

test.describe('sessions', () => {
	test('revoke another session: the dialog names it, Cancel has focus, Escape restores focus, confirming removes the row and moves focus', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await signInWithRecoveryCode(server, admin.recoveryCodes[1]);
		await openSecurity(page, server);
		const revoke = page.getByRole('button', { name: /^Sign out session from/ });
		await expect(revoke).toHaveCount(1);
		await revoke.click();
		const d = dialog(page, 'Sign out this session?');
		await expect(d).toBeVisible();
		await expect(d).toContainText('It stops working at once.');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused(); // default focus on Cancel
		await expectNoAxeViolations(page, 'revoke session dialog');
		// Focus is trapped inside the dialog.
		for (let i = 0; i < 4; i++) await page.keyboard.press('Tab');
		expect(await page.evaluate(() => !!document.activeElement?.closest('dialog'))).toBe(true);
		await page.keyboard.press('Escape');
		await expect(d).toBeHidden();
		await expect(revoke).toBeFocused(); // back on the button that opened it
		await expect(revoke).toHaveCount(1);

		await revoke.click();
		await dialog(page, 'Sign out this session?').getByRole('button', { name: 'Sign out this session' }).click();
		await expect(page.getByRole('button', { name: /^Sign out session from/ })).toHaveCount(0);
		await expect(status(page)).toContainText('Session signed out.');
		// Focus went to the next remaining action (the one for this device).
		await expect(page.getByRole('button', { name: /^Sign out this device/ }).first()).toBeFocused();
	});

	test('sign out everywhere else shows the count, then there are none', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await signInWithRecoveryCode(server, admin.recoveryCodes[1]);
		await signInWithRecoveryCode(server, admin.recoveryCodes[2]);
		await openSecurity(page, server);
		const others = page.getByRole('button', { name: 'Sign out everywhere else (2 other sessions)' });
		await others.click();
		const d = dialog(page, 'Sign out everywhere else?');
		await expect(d).toContainText('2 other sessions will be signed out. This device stays signed in.');
		await expect(d.getByRole('button', { name: 'Sign out 2 sessions' })).toBeVisible();
		await d.getByRole('button', { name: 'Sign out 2 sessions' }).click();
		await expect(page.getByRole('button', { name: 'Sign out everywhere else (0 other sessions)' })).toBeVisible();
		await expect(page.getByRole('button', { name: /^Sign out session from/ })).toHaveCount(0);
	});

	test('revoking this device signs you out', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await page.getByRole('button', { name: /^Sign out this device/ }).first().click();
		const d = dialog(page, 'Sign out this device?');
		await expect(d).toContainText('You will be signed out');
		await d.getByRole('button', { name: 'Sign out this device' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=signed_out$/);
		await expect(page.locator('#why')).toContainText('You signed out.');
	});

	test('the Sign out button in the navigation ends the session', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(url(server, '/'));
		await expect(h1(page, 'Overview')).toBeVisible();
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		await page.getByRole('navigation', { name: 'Main' }).getByRole('button', { name: 'Sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=signed_out$/);
		await page.goto(url(server, '/'));
		await expect(page).toHaveURL(/\/signed-out\?reason=ended$/); // the cookie is gone
	});
});

test.describe('API tokens', () => {
	test('create (shown once, copyable), use, revoke with a named confirmation, revoked stays listed', async ({ page, server, admin, context, browserName }) => {
		if (browserName === 'chromium') await context.grantPermissions(['clipboard-read', 'clipboard-write']);
		await signIn(page, server, admin);
		await openSecurity(page, server);

		// Errors first.
		await page.getByRole('button', { name: 'Create token' }).click();
		await expect(alert(page)).toBeFocused();
		await expect(page.locator('#token-name-error')).toContainText('Say what the token is for.');
		await page.getByLabel('Name', { exact: true }).fill('backup script');
		await page.getByLabel('Expires in (days)').fill('0');
		await page.getByRole('button', { name: 'Create token' }).click();
		await expect(page.locator('#token-days-error')).toContainText('from 1 to 365');
		await expectNoAxeViolations(page, 'create token, errors');

		const token = await createToken(page, 'backup script', 'write', '30');
		expect(token).toMatch(/^sd_[A-Za-z0-9_-]{43}$/);
		await expect(page.getByRole('heading', { name: 'Your new token' })).toBeFocused();
		await expect(status(page)).toContainText('Token created. It is shown once.');
		await expect(page.getByText('write scope, expires')).toBeVisible();
		await expectNoAxeViolations(page, 'new token shown once');
		await page.getByRole('button', { name: 'Copy token' }).click();
		await expect(status(page)).toHaveText(/Token copied\.|Could not copy/);
		if (browserName === 'chromium') expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(token);
		expect(await tokenStatus(server, token)).toBe(200); // it works

		// It is listed (prefix only) and gone from the page once hidden or reloaded.
		await page.getByRole('button', { name: 'I have copied it, hide it' }).click();
		await expect(page.getByRole('heading', { level: 2, name: 'API tokens' })).toBeFocused(); // the button is gone: focus goes to the section
		await expect(page.locator('body')).not.toContainText(token);
		await page.reload();
		await expect(page.getByRole('heading', { level: 2, name: 'API tokens' })).toBeVisible();
		await expect(page.locator('body')).not.toContainText(token);
		const prefix = ((await page.locator('code', { hasText: '…' }).first().textContent()) ?? '').replace('…', '');
		expect(prefix.startsWith('sd_') && token.startsWith(prefix.slice(3, -1)) === false && token.includes(prefix.slice(3, -1)) && prefix.length < token.length / 2).toBe(true); // only the prefix is ever listed
		await expect(page.getByText('● Active').locator('visible=true').first()).toBeVisible();
		if (!isPhone(page)) {
			const table = page.getByRole('table', { name: 'API tokens' });
			await expect(table.locator('caption')).toBeVisible();
			await expect(table.locator('tbody th[scope="row"]')).toHaveText(['backup script']);
		}
		await expectNoAxeViolations(page, 'token list');

		// Revoke: the dialog names the token and its prefix.
		await page.getByRole('button', { name: /^Revoke token backup script/ }).click();
		const d = dialog(page, 'Revoke this token?');
		await expect(d).toContainText('"backup script"');
		await expect(d).toContainText('sd_' + token.slice(3, 11)); // the prefix, never the token
		await expect(d).toContainText('This cannot be undone.');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await d.getByRole('button', { name: 'Revoke token' }).click();
		await expect(page.getByText('◌ Revoked').locator('visible=true').first()).toBeVisible(); // revoked tokens stay listed
		await expect(page.getByRole('button', { name: /^Revoke token backup script/ })).toHaveCount(0);
		expect(await tokenStatus(server, token)).toBe(401);
		await expectNoAxeViolations(page, 'token list with a revoked token');
	});

	test('an expired token shows as expired', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await createToken(page, 'old one');
		server.sql('UPDATE api_tokens SET expires_at = 1');
		await page.reload();
		await expect(page.getByText('▲ Expired').locator('visible=true').first()).toBeVisible();
		await expectNoAxeViolations(page, 'expired token');
	});
});

test.describe('sensitive changes', () => {
	test('regenerate recovery codes: the count, the consequence, then the same screen as setup; old codes stop working', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		await expect(page.locator('#codes-left')).toHaveText('You have 9 unused recovery codes.'); // signing in used one
		await page.getByRole('button', { name: 'Make new recovery codes…' }).click();
		const d = dialog(page, 'Make new recovery codes?');
		await expect(d).toContainText('Your 9 unused codes will stop working.');
		await expectNoAxeViolations(page, 'regenerate dialog');
		await d.getByRole('button', { name: 'Make new codes' }).click();

		await expect(page.getByRole('heading', { name: 'Recovery codes' })).toBeFocused();
		const items = page.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem');
		await expect(items).toHaveCount(10);
		const fresh = await items.allTextContents();
		expect(fresh).not.toContain(admin.recoveryCodes[0]);
		await expect(page.getByText('You can make new codes later in Settings › Security.')).toBeVisible(); // the claim is true here
		const cont = page.getByRole('button', { name: 'Continue' });
		await expect(cont).toHaveAttribute('aria-disabled', 'true');
		await expect(page.locator('#why-disabled')).toContainText('Tick the box');
		await expectNoAxeViolations(page, 'new recovery codes');
		await page.getByLabel('I have saved these codes').check();
		await expect(cont).toHaveAttribute('aria-disabled', 'false');
		await cont.click();
		await expect(page.locator('#codes-left')).toHaveText('You have 10 unused recovery codes.');

		// Old codes are dead, new ones work.
		await expect(signInWithRecoveryCode(server, admin.recoveryCodes[3])).rejects.toThrow(/2fa 401|2fa 403|invalid/i);
		await signInWithRecoveryCode(server, fresh[0]);
	});

	test('step-up: a stale session asks for a code, a wrong one is explained, Cancel returns focus, the right one continues', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		server.sql('UPDATE sessions SET mfa_fresh_until = 1');
		const opener = page.getByRole('button', { name: 'Make new recovery codes…' });
		await opener.click();
		await dialog(page, 'Make new recovery codes?').getByRole('button', { name: 'Make new codes' }).click();
		const step = dialog(page, 'Confirm it is you');
		await expect(step).toBeVisible();
		await expect(step.getByLabel('Code from your authenticator')).toBeFocused();
		await expect(step.getByLabel('Code from your authenticator')).toHaveAttribute('autocomplete', 'one-time-code');
		await expectNoAxeViolations(page, 'step-up dialog');
		await step.getByLabel('Code from your authenticator').fill('000000');
		await step.getByRole('button', { name: 'Confirm' }).click();
		await expect(step).toContainText(/That code is wrong\. \d+ tries? left before you are signed out\./);
		await expectNoAxeViolations(page, 'step-up dialog, wrong code');
		// Cancel: nothing changed, focus is back on the opener.
		await step.getByRole('button', { name: 'Cancel' }).click();
		await expect(step).toBeHidden();
		await expect(opener).toBeFocused();
		await expect(page.getByRole('list', { name: 'Recovery codes' })).toHaveCount(0);
		// Try again with the right code (one step ahead of the code setup used).
		await opener.click();
		await dialog(page, 'Make new recovery codes?').getByRole('button', { name: 'Make new codes' }).click();
		await step.getByLabel('Code from your authenticator').fill(totp(admin.secret, Date.now() + 30_000));
		await step.getByRole('button', { name: 'Confirm' }).click();
		await expect(page.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem')).toHaveCount(10);
	});

	test('change password: tokens kept unticked by default, a wrong current password is explained, success signs you out with reason credentials', async ({ page, server, admin }) => {
		const state = await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		const token = await createToken(page, 'to be revoked');
		await page.getByRole('button', { name: 'I have copied it, hide it' }).click();
		const keep = page.getByLabel('Keep API tokens').first();
		await expect(keep).not.toBeChecked();
		await expect(page.getByText('Tick this only if you are rotating routinely, not after a suspected compromise.').first()).toBeVisible();

		await page.getByLabel('Current password').fill('definitely the wrong password');
		await page.getByLabel('New password').fill('a brand new long password');
		await page.getByRole('button', { name: 'Change password…' }).click();
		const d = dialog(page, 'Change the password?');
		await expect(d).toContainText('All 1 session, including this one, will be signed out.');
		await expect(d).toContainText('Your 1 active API token will be revoked.');
		await expectNoAxeViolations(page, 'change password dialog');
		await d.getByRole('button', { name: 'Change password and sign out' }).click();
		await expect(alert(page)).toContainText('That is not your current password.');
		await expect(page.getByLabel('Current password')).toHaveValue(''); // a password is never kept
		await expect(page.getByLabel('New password')).toHaveValue('a brand new long password'); // typed input is
		await expect(page.locator('#current-password-error')).toBeVisible();

		await page.getByLabel('Current password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Change password…' }).click();
		await dialog(page, 'Change the password?').getByRole('button', { name: 'Change password and sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=credentials$/);
		await expect(page.locator('#why')).toContainText('password or authenticator changed');
		await expectNoAxeViolations(page, 'signed out after a password change');
		expect(await tokenStatus(server, token)).toBe(401); // revoked
		const { ctx } = await api(server, state).catch(() => ({ ctx: null }));
		void ctx;
		// The old password is dead, the new one signs in (with a recovery code).
		const probe = await request.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true, extraHTTPHeaders: { Origin: server.baseURL } });
		expect((await probe.post('/api/v1/auth/login', { data: { username: 'admin', password: PASSWORD } })).status()).toBe(401);
		expect((await probe.post('/api/v1/auth/login', { data: { username: 'admin', password: 'a brand new long password' } })).status()).toBe(200);
		await probe.dispose();
	});

	test('change password with Keep API tokens ticked keeps the token', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		const token = await createToken(page, 'rotating');
		await page.getByRole('button', { name: 'I have copied it, hide it' }).click();
		await page.getByLabel('Keep API tokens').first().check();
		await page.getByLabel('Current password').fill(PASSWORD);
		await page.getByLabel('New password').fill('another long new password');
		await page.getByRole('button', { name: 'Change password…' }).click();
		const d = dialog(page, 'Change the password?');
		await expect(d).toContainText('Your 1 active API token will be kept.');
		await d.getByRole('button', { name: 'Change password and sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=credentials$/);
		expect(await tokenStatus(server, token)).toBe(200);
	});

	test('re-enrol the authenticator: new key before the QR, a confirmation, then signed out with reason credentials', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		await page.getByRole('button', { name: 'Replace authenticator…' }).click();
		await expect(page.getByRole('heading', { name: 'Add the new key' })).toBeFocused();
		const key = page.locator('#re-key');
		const secret = ((await key.textContent()) ?? '').replace(/\s/g, '');
		expect(secret).toMatch(/^[A-Z2-7]{16,}$/);
		const qr = page.getByRole('img', { name: 'QR code for your authenticator app' });
		await expect(qr).toBeVisible();
		expect(await qr.getAttribute('aria-label')).not.toContain(secret);
		expect(await key.evaluate((k, q) => !!(k.compareDocumentPosition(q!) & Node.DOCUMENT_POSITION_FOLLOWING), await qr.elementHandle())).toBe(true);
		await expect(page.getByText('Tick this only if you are rotating routinely, not after a suspected compromise.').first()).toBeVisible();
		await expect(page.getByLabel('Keep API tokens').last()).not.toBeChecked();
		await expectNoAxeViolations(page, 're-enrol');

		await page.getByLabel('Code from the new authenticator').fill('000000');
		await page.getByRole('button', { name: 'Replace authenticator…' }).last().click();
		await dialog(page, 'Replace the authenticator?').getByRole('button', { name: 'Replace authenticator and sign out' }).click();
		await expect(alert(page)).toContainText('That code is wrong');
		await page.getByLabel('Code from the new authenticator').fill(totp(secret, Date.now() + 30_000));
		await page.getByRole('button', { name: 'Replace authenticator…' }).last().click();
		const d = dialog(page, 'Replace the authenticator?');
		await expect(d).toContainText('Codes from your old authenticator stop working.');
		await expectNoAxeViolations(page, 're-enrol dialog');
		await d.getByRole('button', { name: 'Replace authenticator and sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=credentials$/);
	});
});

test.describe('sizes', () => {
	test('row actions, create button and dialog buttons are at least 44 px', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		for (const name of [/^Sign out this device/, 'Create token', 'Change password…', 'Replace authenticator…', 'Make new recovery codes…', /^Sign out everywhere else/]) {
			const b = (await page.getByRole('button', { name }).first().boundingBox())!;
			expect(b.height, String(name)).toBeGreaterThanOrEqual(44);
		}
		await page.getByRole('button', { name: /^Sign out this device/ }).first().click();
		for (const name of ['Cancel', 'Sign out this device']) {
			const b = (await dialog(page, 'Sign out this device?').getByRole('button', { name }).boundingBox())!;
			expect(b.height, name).toBeGreaterThanOrEqual(44);
		}
	});
});

test.describe('step 13 review fixes', () => {
	test('an unrecorded audit outcome is a warning alert, and it follows a session-ending change to Signed-out', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await createToken(page, 'audited');
		await page.getByRole('button', { name: 'I have copied it, hide it' }).click();
		const unrecorded = (route: import('@playwright/test').Route) =>
			route.fetch().then(async (res) => route.fulfill({ response: res, json: { ...(await res.json()), audit: 'unrecorded' } }));
		await page.route('**/api/v1/actions/tokens.revoke', unrecorded);
		await page.getByRole('button', { name: /^Revoke token audited/ }).click();
		await dialog(page, 'Revoke this token?').getByRole('button', { name: 'Revoke token' }).click();
		await expect(alert(page)).toContainText('audit record could not be written');
		await expect(alert(page)).toBeFocused();
		await expectNoAxeViolations(page, 'unrecorded audit warning');

		await page.route('**/api/v1/actions/auth.password_change', unrecorded);
		await page.getByLabel('Current password').fill(PASSWORD);
		await page.getByLabel('New password').fill('a brand new long password');
		await page.getByRole('button', { name: 'Change password…' }).click();
		await dialog(page, 'Change the password?').getByRole('button', { name: 'Change password and sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=credentials$/);
		await expect(page.getByRole('alert')).toContainText('audit record for the change that signed you out could not be written');
		await expectNoAxeViolations(page, 'signed out with an audit warning');
	});

	test('the dialog is named by its new title the moment it opens', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await signInWithRecoveryCode(server, admin.recoveryCodes[1]);
		await openSecurity(page, server);
		await page.getByRole('button', { name: /^Sign out session from/ }).click();
		await expect(page.getByRole('dialog')).toHaveAccessibleName('Sign out this session?');
		await page.keyboard.press('Escape');
		await page.getByRole('button', { name: 'Make new recovery codes…' }).click();
		await expect(page.getByRole('dialog')).toHaveAccessibleName('Make new recovery codes?');
		await expect(page.getByRole('dialog')).toHaveAccessibleDescription(/unused codes will stop working/);
	});

	test('the step-up error belongs to the field; 401 and 429 are mapped', async ({ page, server, admin }) => {
		await signIn(page, server, admin, 0);
		await openSecurity(page, server);
		server.sql('UPDATE sessions SET mfa_fresh_until = 1');
		await page.getByRole('button', { name: 'Make new recovery codes…' }).click();
		await dialog(page, 'Make new recovery codes?').getByRole('button', { name: 'Make new codes' }).click();
		const step = dialog(page, 'Confirm it is you');
		const field = step.getByLabel('Code from your authenticator');
		await field.fill('000000');
		await step.getByRole('button', { name: 'Confirm' }).click();
		await expect(field).toHaveAttribute('aria-invalid', 'true');
		expect(await field.getAttribute('aria-describedby')).toContain('stepup-code-error');
		await expect(step.getByRole('alert')).toHaveCount(0); // one announcement path, no double alert
		await expect(field).toBeFocused();
		// 429: the coarse countdown, and the button is aria-disabled.
		await page.route('**/api/v1/auth/reauth', (r) => r.fulfill({ status: 429, headers: { 'Retry-After': '90' }, contentType: 'application/json', body: '{"error":{"code":"rate_limited","message":"x","request_id":"r"}}' }));
		await field.fill('111111');
		await step.getByRole('button', { name: 'Confirm' }).click();
		await expect(step.getByText(/^Try again in \d+:\d\d$/)).toBeVisible();
		await expect(step.getByRole('button', { name: 'Confirm' })).toHaveAttribute('aria-disabled', 'true');
		// A 401 that is not "revoked" uses the ordinary reason.
		await page.unroute('**/api/v1/auth/reauth');
		await page.route('**/api/v1/auth/reauth', (r) => r.fulfill({ status: 401, contentType: 'application/json', body: '{"error":{"code":"idle_expired","message":"x","request_id":"r"}}' }));
		await step.getByRole('button', { name: 'Cancel' }).click();
	});

	test('step-up 401 with code revoked says step_up; any other 401 uses its own reason', async ({ page, server, admin }) => {
		for (const [code, reason] of [['revoked', 'step_up'], ['idle_expired', 'idle']]) {
			server.sql('DELETE FROM sessions');
			const state = await signInWithRecoveryCode(server, admin.recoveryCodes[code === 'revoked' ? 0 : 1]);
			await page.context().clearCookies();
			await page.context().addCookies(state.cookies);
			await openSecurity(page, server);
			server.sql('UPDATE sessions SET mfa_fresh_until = 1');
			await page.route('**/api/v1/auth/reauth', (r) => r.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code, message: 'x', request_id: 'r' } }) }));
			await page.getByRole('button', { name: 'Make new recovery codes…' }).click();
			await dialog(page, 'Make new recovery codes?').getByRole('button', { name: 'Make new codes' }).click();
			const step = dialog(page, 'Confirm it is you');
			await step.getByLabel('Code from your authenticator').fill('123456');
			await step.getByRole('button', { name: 'Confirm' }).click();
			await expect(page).toHaveURL(new RegExp(`/signed-out\\?reason=${reason}$`));
			await page.unroute('**/api/v1/auth/reauth');
		}
	});

	test('the error summary links to each failing field', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await page.getByRole('button', { name: 'Change password…' }).click();
		await expect(alert(page)).toBeFocused();
		await alert(page).getByRole('link', { name: 'Current password' }).click();
		await expect(page.getByLabel('Current password')).toBeFocused();
		await alert(page).getByRole('link', { name: 'New password' }).click();
		await expect(page.getByLabel('New password')).toBeFocused();
		await page.getByRole('button', { name: 'Create token' }).click();
		await alert(page).getByRole('link', { name: 'Name' }).click();
		await expect(page.getByLabel('Name', { exact: true })).toBeFocused();
		await page.getByRole('button', { name: 'Replace authenticator…' }).click();
		await page.getByLabel('Code from the new authenticator').fill('12');
		await page.getByRole('button', { name: 'Replace authenticator…' }).last().click();
		await alert(page).getByRole('link', { name: 'Code from the new authenticator' }).click();
		await expect(page.getByLabel('Code from the new authenticator')).toBeFocused();
		// Cancelling re-enrolment returns focus to the button that started it.
		await page.getByRole('button', { name: 'Cancel' }).click();
		await expect(page.getByRole('button', { name: 'Replace authenticator…' })).toBeFocused();
	});

	test('a shown token disappears when the page is hidden or another action starts', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		const token = await createToken(page, 'one');
		await page.evaluate(() => {
			Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
			document.dispatchEvent(new Event('visibilitychange'));
		});
		await expect(page.locator('body')).not.toContainText(token);
		await page.evaluate(() => {
			Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
		});
		const second = await createToken(page, 'two');
		await page.getByRole('button', { name: /^Revoke token one/ }).click();
		await expect(page.locator('body')).not.toContainText(second); // starting another action clears it
		await dialog(page, 'Revoke this token?').getByRole('button', { name: 'Cancel' }).click();
	});

	test('Sign out in the navigation asks first while recovery codes are unsaved', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		await page.getByRole('button', { name: 'Make new recovery codes…' }).click();
		await dialog(page, 'Make new recovery codes?').getByRole('button', { name: 'Make new codes' }).click();
		await expect(page.getByRole('list', { name: 'Recovery codes' })).toBeVisible();
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		const asked: string[] = [];
		page.once('dialog', (d) => { asked.push(d.message()); void d.dismiss(); });
		await page.getByRole('navigation', { name: 'Main' }).getByRole('button', { name: 'Sign out' }).click();
		await expect.poll(() => asked.length).toBe(1);
		expect(asked[0]).toContain('not marked as saved');
		await expect(page.getByRole('list', { name: 'Recovery codes' })).toBeVisible(); // still signed in, codes still shown
		page.once('dialog', (d) => void d.accept());
		await page.getByRole('navigation', { name: 'Main' }).getByRole('button', { name: 'Sign out' }).click();
		await expect(page).toHaveURL(/\/signed-out\?reason=signed_out$/);
	});

	test('markup details: cards are lists, the current session button has no suffix, the password form names its account', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		const btn = page.getByRole('button', { name: /^Sign out this device/ });
		await expect(btn.first()).toHaveAccessibleName('Sign out this device');
		await expect(page.locator('input[name="username"][autocomplete="username"]')).toHaveValue('admin');
		await expect(page.locator('ul.cards').first()).toHaveAttribute('role', 'list');
		await expect(page.locator('ul.cards li').first()).toHaveAttribute('role', 'listitem');
		await expect(page.locator('.cert-status')).toHaveAttribute('role', 'status'); // present before the certificate is known
		await expect(page.getByLabel('Keep API tokens').first()).not.toBeChecked();
		await expect(page.getByText('Tick this only if you are rotating routinely, not after a suspected compromise.').first()).toBeVisible();
	});

	test('tokens are listed as sd_<prefix>…', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openSecurity(page, server);
		const token = await createToken(page, 'shown');
		await page.getByRole('button', { name: 'I have copied it, hide it' }).click();
		await expect(page.locator('code', { hasText: /^sd_.{6,12}…$/ }).first()).toBeAttached();
		expect(token.startsWith('sd_')).toBe(true);
	});
});
