import type { Page } from '@playwright/test';
import { request } from '@playwright/test';
import { expect, expectNoAxeViolations, test } from './fixtures';
import { signInWithRecoveryCode, type TestServer } from './server';

const INFO = '**/api/v1/actions/system.info';
const isPhone = (page: Page) => (page.viewportSize()?.width ?? 1280) < 768;

async function openOverview(page: Page) {
	await page.goto('/');
	await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible();
	await expect(page).toHaveTitle('Overview · ServerDock');
}
// Safari on macOS skips links when tabbing unless Option is held; Playwright's macOS
// WebKit follows that. Linux WebKit (CI) and every other browser tab through links.
const tab = (page: Page) => page.keyboard.press(process.platform === 'darwin' && page.context().browser()?.browserType().name() === 'webkit' ? 'Alt+Tab' : 'Tab');
const footer = (page: Page) => page.getByRole('contentinfo');

test.describe('shell states', () => {
	test('Overview: empty state, landmarks and no axe violations', async ({ page }) => {
		await openOverview(page);
		await expect(page.getByRole('heading', { level: 2, name: 'No services yet' })).toBeVisible();
		await expect(page.getByText('Deploying from an image arrives in the next version')).toBeVisible();
		await expect(page.getByRole('link', { name: 'Read the documentation (opens GitHub)' })).toBeVisible();
		await expect(page.locator('main')).toHaveCount(1);
		await expect(page.locator('h1')).toHaveCount(1);
		await expect(page.locator('html')).toHaveAttribute('lang', 'en');
		await expectNoAxeViolations(page, 'Overview');
	});

	test('navigation shows only Overview and Settings, with aria-current', async ({ page }) => {
		await openOverview(page);
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		const nav = page.getByRole('navigation', { name: 'Main' });
		await expect(nav.getByRole('link')).toHaveText(['Overview', 'Settings']);
		await expect(nav.getByRole('link', { name: 'Overview' })).toHaveAttribute('aria-current', 'page');
		await expect(nav.getByRole('link', { name: 'Settings' })).not.toHaveAttribute('aria-current', /.+/);
		await nav.getByRole('link', { name: 'Settings' }).click();
		await expect(page).toHaveURL(/\/settings\/security$/);
		await expect(page).toHaveTitle('Security · ServerDock');
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		await expect(nav.getByRole('link', { name: 'Settings' })).toHaveAttribute('aria-current', 'page');
		await expectNoAxeViolations(page, 'Settings placeholder');
	});

	test('the phone menu: visible label, aria-expanded, Escape returns focus', async ({ page }) => {
		test.skip(!isPhone(page), 'phone layouts only');
		await openOverview(page);
		const header = page.getByRole('banner');
		await expect(header).toBeVisible();
		const button = header.getByRole('button', { name: 'Menu' });
		await expect(button).toHaveAttribute('aria-expanded', 'false');
		await expect(button).toContainText('Menu'); // a visible text label, not only an icon
		await expect(page.getByRole('navigation', { name: 'Main' })).toBeHidden();
		await expectNoAxeViolations(page, 'phone, menu closed');
		await button.click();
		await expect(button).toHaveAttribute('aria-expanded', 'true');
		await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible();
		await expectNoAxeViolations(page, 'phone, menu open');
		await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Overview' }).focus();
		await page.keyboard.press('Escape');
		await expect(button).toHaveAttribute('aria-expanded', 'false');
		await expect(button).toBeFocused();
		await expect(page.getByRole('navigation', { name: 'Main' })).toBeHidden();
	});

	test('desktop: sidebar always visible, no header bar', async ({ page }) => {
		test.skip(isPhone(page), 'wide layouts only');
		await openOverview(page);
		await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible();
		await expect(page.getByRole('banner')).toHaveCount(0); // the phone bar is display: none
		await expect(page.getByRole('button', { name: 'Menu' })).toBeHidden();
	});

	test('Signed-out for each reason, and a missing session lands there', async ({ page, browser, baseURL }) => {
		const reasons: Record<string, string> = {
			signed_out: 'You signed out.',
			idle: '12 hours without activity',
			expired: '7-day session limit',
			step_up: 'too many wrong confirmation codes',
			credentials: 'password or authenticator changed',
			revoked: 'ended from another device',
			ended: 'Your session ended or you are not signed in.'
		};
		for (const [reason, text] of Object.entries(reasons)) {
			await page.goto(`/signed-out?reason=${reason}`);
			await expect(page.getByRole('heading', { level: 1, name: 'Signed out' })).toBeVisible();
			await expect(page.locator('#why')).toContainText(text);
			await expect(page.getByRole('link', { name: 'Sign in again' })).toBeVisible();
			await expect(page).toHaveTitle('Signed out · ServerDock');
			await expectNoAxeViolations(page, `Signed out (${reason})`);
		}
		// No cookie at all: the shell redirects to the Signed-out screen.
		const ctx = await browser.newContext({ baseURL, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
		const bare = await ctx.newPage();
		await bare.goto('/');
		await expect(bare).toHaveURL(/\/signed-out\?reason=ended$/);
		await expect(bare.getByRole('heading', { level: 1, name: 'Signed out' })).toBeVisible();
		await ctx.close();
	});

	test('an unknown path shows a not-found page, not a blank screen', async ({ page }) => {
		await page.goto('/no/such/page');
		await expect(page.getByRole('heading', { level: 1, name: 'Page not found' })).toBeVisible();
		await expectNoAxeViolations(page, 'not found');
	});
});

test.describe('footer status (A12)', () => {
	test('shows the numbers, is not a live region, and says unknown when a poll fails', async ({ page, problems }) => {
		await page.clock.install();
		await openOverview(page);
		await expect(footer(page)).toContainText(/\d+ MB RAM/);
		await expect(footer(page)).toContainText('ServerDock itself');
		// Not a live region: no aria-live, no status/alert role at or inside the footer.
		expect(await page.evaluate(() => {
			const f = document.querySelector('footer')!;
			return [f, ...f.querySelectorAll('*')].some((e) => e.hasAttribute('aria-live') || ['status', 'alert', 'log'].includes(e.getAttribute('role') ?? ''));
		})).toBe(false);
		await expectNoAxeViolations(page, 'footer with numbers');

		await page.route(INFO, (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"x","request_id":"r"}}' }));
		await page.clock.runFor(15_000);
		await expect(footer(page)).toContainText('unknown');
		await expect(footer(page)).not.toContainText('MB RAM'); // never stale numbers
		await expectNoAxeViolations(page, 'footer unknown');

		await page.unroute(INFO);
		await page.clock.runFor(15_000);
		await expect(footer(page)).toContainText(/\d+ MB RAM/);
		// The browser logs the mocked 500 itself; that is the one expected console error.
		problems.splice(0, problems.length, ...problems.filter((p) => !p.includes('status of 500')));
	});

	test('a request that never answers ends as unknown', async ({ page }) => {
		await page.clock.install();
		await openOverview(page);
		await expect(footer(page)).toContainText(/\d+ MB RAM/);
		await page.route(INFO, () => {
			/* never answered */
		});
		await page.clock.runFor(15_000); // the poll starts and hangs
		await page.clock.runFor(11_000); // its 10 s limit passes
		await expect(footer(page)).toContainText('unknown');
	});

	test('polling stops while hidden; unknown until the first fresh poll', async ({ page }) => {
		await page.clock.install();
		await openOverview(page);
		await expect(footer(page)).toContainText(/\d+ MB RAM/);
		let polls = 0;
		page.on('request', (r) => r.url().endsWith('/system.info') && polls++);
		const setVisibility = (v: 'hidden' | 'visible') =>
			page.evaluate((state) => {
				Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
				document.dispatchEvent(new Event('visibilitychange'));
			}, v);

		await setVisibility('hidden');
		await expect(footer(page)).toContainText('unknown');
		await page.clock.runFor(60_000);
		expect(polls, 'no polls while hidden').toBe(0);
		await expect(footer(page)).toContainText('unknown');

		await setVisibility('visible');
		await expect(footer(page)).toContainText(/\d+ MB RAM/); // the first fresh poll
		expect(polls).toBe(1);
		await page.clock.runFor(15_000);
		await expect.poll(() => polls).toBe(2);
	});

	test('a 401 from the poll sends you to Signed out', async ({ page, problems }) => {
		await page.clock.install();
		await openOverview(page);
		await page.route(INFO, (r) => r.fulfill({ status: 401, contentType: 'application/json', body: '{"error":{"code":"unauthenticated","message":"x","request_id":"r"}}' }));
		await page.clock.runFor(15_000);
		await expect(page).toHaveURL(/\/signed-out/);
		// The browser logs the mocked 401 itself; that is the one expected console error.
		problems.splice(0, problems.length, ...problems.filter((p) => !p.includes('status of 401')));
	});
});

test.describe('keyboard, focus and targets', () => {
	test('skip link is the first tab stop and moves focus to main', async ({ page }) => {
		await openOverview(page);
		await tab(page);
		const skip = page.getByRole('link', { name: 'Skip to main content' });
		await expect(skip).toBeFocused();
		await expect(skip).toBeInViewport();
		await page.keyboard.press('Enter');
		await expect(page.locator('main')).toBeFocused();
		await expectNoAxeViolations(page, 'after the skip link');
	});

	test('keyboard-only path through the shell; the h1 takes focus on a route change', async ({ page }) => {
		await openOverview(page);
		await tab(page); // skip link
		await tab(page);
		if (isPhone(page)) {
			await expect(page.getByRole('button', { name: 'Menu' })).toBeFocused();
			await page.keyboard.press('Enter');
			await expect(page.getByRole('button', { name: 'Menu' })).toHaveAttribute('aria-expanded', 'true');
			await tab(page);
		}
		await expect(page.getByRole('link', { name: 'Overview' })).toBeFocused();
		await tab(page);
		await expect(page.getByRole('link', { name: 'Settings' })).toBeFocused();
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(/\/settings\/security$/);
		await expect(page.getByRole('heading', { level: 1, name: 'Security' })).toBeFocused();
		// the h1 is focusable by script only, never a tab stop
		await expect(page.locator('h1')).toHaveAttribute('tabindex', '-1');
	});

	test('focus is never hidden under the sticky bar', async ({ page }) => {
		test.skip(!isPhone(page), 'the sticky bar is a phone layout');
		await openOverview(page);
		const barBottom = (await page.getByRole('banner').boundingBox())!.y + (await page.getByRole('banner').boundingBox())!.height;
		const pad = await page.evaluate(() => parseFloat(getComputedStyle(document.documentElement).scrollPaddingTop));
		expect(pad).toBeGreaterThanOrEqual(barBottom);
		await page.getByRole('link', { name: /Read the documentation/ }).focus();
		const top = (await page.getByRole('link', { name: /Read the documentation/ }).boundingBox())!.y;
		expect(top).toBeGreaterThanOrEqual(barBottom);
	});

	test('interactive targets are at least 44 px', async ({ page }) => {
		await openOverview(page);
		if (isPhone(page)) {
			const b = (await page.getByRole('button', { name: 'Menu' }).boundingBox())!;
			expect(b.width).toBeGreaterThanOrEqual(44);
			expect(b.height).toBeGreaterThanOrEqual(44);
			await page.getByRole('button', { name: 'Menu' }).click();
		}
		for (const name of ['Overview', 'Settings']) {
			const box = (await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name }).boundingBox())!;
			expect(box.height, `${name} height`).toBeGreaterThanOrEqual(44);
			expect(box.width, `${name} width`).toBeGreaterThanOrEqual(44);
		}
		await tab(page);
		const skip = (await page.getByRole('link', { name: 'Skip to main content' }).boundingBox())!;
		expect(skip.height).toBeGreaterThanOrEqual(44);
	});
});

test.describe('presentation preferences', () => {
	// Only the spacing test injects a style element; the CSP is checked everywhere else.
	test.describe('text spacing', () => {
		test.use({ bypassCSP: true });
		test('text spacing (WCAG 1.4.12) does not clip or overflow', async ({ page }) => {
			await openOverview(page);
			await page.addStyleTag({
				content: '* { line-height: 1.5 !important; letter-spacing: 0.12em !important; word-spacing: 0.16em !important; } p { margin-bottom: 2em !important; }'
			});
			if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
			const clipped = await page.evaluate(() =>
				[...document.querySelectorAll('main, nav, footer, header, h1, h2, p, a, button')]
					.filter((e) => e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).display !== 'inline')
					.map((e) => e.tagName + ':' + (e.textContent ?? '').slice(0, 20))
			);
			expect(clipped).toEqual([]);
			const vertical = await page.evaluate(() => [...document.querySelectorAll('header')].filter((e) => e.scrollHeight > e.clientHeight + 1).length);
			expect(vertical, 'the phone bar grows with the text instead of clipping').toBe(0);
			expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
			await expectNoAxeViolations(page, 'text spacing');
		});
	});

	test('reflows without horizontal scrolling', async ({ page }) => {
		await openOverview(page);
		expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
	});

	test('colour scheme, reduced motion and forced colours are honoured', async ({ page }, info) => {
		await openOverview(page);
		const prefs = await page.evaluate(() => ({
			dark: matchMedia('(prefers-color-scheme: dark)').matches,
			reduce: matchMedia('(prefers-reduced-motion: reduce)').matches,
			forced: matchMedia('(forced-colors: active)').matches,
			bg: getComputedStyle(document.body).backgroundColor,
			scroll: getComputedStyle(document.documentElement).scrollBehavior,
			scheme: getComputedStyle(document.documentElement).colorScheme
		}));
		expect(prefs.scheme).toContain('dark');
		if (info.project.name === 'dark') {
			expect(prefs.dark).toBe(true);
			expect(prefs.bg).toBe('rgb(20, 23, 28)');
		}
		if (prefs.reduce) expect(prefs.scroll).toBe('auto');
		if (info.project.name === 'forced-colors') expect(prefs.forced).toBe(true);
		if (info.project.name === 'reduced-motion') expect(prefs.reduce).toBe(true);
		// No transition or animation runs longer than 100 ms under reduced motion.
		if (prefs.reduce) {
			const long = await page.evaluate(() =>
				[...document.querySelectorAll('*')].filter((e) => {
					const s = getComputedStyle(e);
					return parseFloat(s.transitionDuration) > 0.1 || parseFloat(s.animationDuration) > 0.1;
				}).length
			);
			expect(long).toBe(0);
		}
	});
});

// A fresh binary's first-run admin signs in with a recovery code (never colliding
// with TOTP counters) and the session is then ended in a specific way.
async function sessionState(server: TestServer, recovery: string[], n: number) {
	return signInWithRecoveryCode(server, recovery[n]);
}

test.describe('Signed-out reasons come from the 401 code', () => {
	test.beforeEach(({}, info) => {
		test.skip(!['desktop', 'webkit'].includes(info.project.name), 'server-side behaviour, checked in two projects');
	});
	test.use({ storageState: { cookies: [], origins: [] } });

	async function expectReason(browser: import('@playwright/test').Browser, server: TestServer, state: Awaited<ReturnType<typeof sessionState>>, reason: string, text: string) {
		const ctx = await browser.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true, storageState: state });
		const page = await ctx.newPage();
		await page.goto('/');
		await expect(page).toHaveURL(new RegExp(`/signed-out\\?reason=${reason}$`));
		await expect(page.locator('#why')).toContainText(text);
		await expectNoAxeViolations(page, `signed out (${reason})`);
		await ctx.close();
	}

	test('idle expiry', async ({ browser, server, admin }) => {
		const state = await sessionState(server, admin.recoveryCodes, 0);
		server.sql('UPDATE sessions SET idle_expires_at = 1');
		await expectReason(browser, server, state, 'idle', '12 hours');
	});

	test('absolute expiry', async ({ browser, server, admin }) => {
		const state = await sessionState(server, admin.recoveryCodes, 0);
		server.sql('UPDATE sessions SET abs_expires_at = 1');
		await expectReason(browser, server, state, 'expired', '7-day');
	});

	test('revoked from another session', async ({ browser, server, admin }) => {
		const a = await sessionState(server, admin.recoveryCodes, 0);
		const b = await sessionState(server, admin.recoveryCodes, 1);
		const ctx = await request.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true, storageState: b, extraHTTPHeaders: { Origin: server.baseURL } });
		const sess = await (await ctx.get('/api/v1/auth/session')).json();
		const res = await ctx.post('/api/v1/actions/auth.sessions_revoke', { data: { all_others: true }, headers: { 'X-SD-CSRF': sess.csrf } });
		expect(res.ok(), await res.text()).toBe(true);
		await ctx.dispose();
		await expectReason(browser, server, a, 'revoked', 'another device');
	});

	test('no cookie at all is "ended"', async ({ browser, server, admin }) => {
		void admin;
		await expectReason(browser, server, { cookies: [], origins: [] }, 'ended', 'not signed in');
	});
});

test.describe('the footer poll maps the 401 code too', () => {
	for (const [code, reason] of [['idle_expired', 'idle'], ['absolute_expired', 'expired'], ['revoked', 'revoked'], ['mystery', 'ended']]) {
		test(`${code} -> ${reason}`, async ({ page, problems }) => {
			await page.clock.install();
			await openOverview(page);
			await page.route(INFO, (r) => r.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code, message: 'x', request_id: 'r' } }) }));
			await page.clock.runFor(15_000);
			await expect(page).toHaveURL(new RegExp(`/signed-out\\?reason=${reason}$`));
			problems.splice(0, problems.length, ...problems.filter((p) => !p.includes('status of 401')));
		});
	}
});

test.describe('error page', () => {
	test('a server error: title matches the heading and Try again works', async ({ page, problems }) => {
		await page.route('**/api/v1/auth/session', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"could not check the session","request_id":"r"}}' }));
		await page.goto('/');
		await expect(page.getByRole('heading', { level: 1, name: 'Something went wrong' })).toBeVisible();
		await expect(page).toHaveTitle('Something went wrong · ServerDock');
		await expectNoAxeViolations(page, 'error page (500)');
		const retry = page.getByRole('button', { name: 'Try again' });
		const box = (await retry.boundingBox())!;
		expect(box.height).toBeGreaterThanOrEqual(44);
		const link = (await page.getByRole('link', { name: 'Go to the Overview' }).boundingBox())!;
		expect(link.height).toBeGreaterThanOrEqual(44);
		await page.unroute('**/api/v1/auth/session');
		await retry.click();
		await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible();
		problems.splice(0, problems.length, ...problems.filter((p) => !p.includes('status of 500')));
	});

	test('not found: title matches the heading, no Try again', async ({ page }) => {
		await page.goto('/no/such/page');
		await expect(page).toHaveTitle('Page not found · ServerDock');
		await expect(page.getByRole('button', { name: 'Try again' })).toHaveCount(0);
	});
});

test.describe('route announcer and headings', () => {
	test('the announcer has no inline style, is at most 1x1, and says the new title', async ({ page }) => {
		await openOverview(page);
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Settings' }).click();
		await expect(page).toHaveTitle('Security · ServerDock');
		const a = page.locator('#svelte-announcer');
		await expect(a).toHaveClass(/sd-announcer/);
		await expect(a).not.toHaveAttribute('style', /.*/);
		await expect(a).toHaveText('Security · ServerDock');
		const box = (await a.boundingBox())!;
		expect(box.width).toBeLessThanOrEqual(1);
		expect(box.height).toBeLessThanOrEqual(1);
	});

	test('every route has exactly one focusable h1 and a title ending in ServerDock', async ({ page }) => {
		for (const [path, title] of [['/', 'Overview'], ['/settings/security', 'Security'], ['/signed-out', 'Signed out'], ['/no/such/page', 'Page not found']]) {
			await page.goto(path);
			const h1 = page.getByRole('heading', { level: 1 });
			await expect(h1).toHaveCount(1);
			await expect(h1).toHaveText(title);
			await expect(h1).toHaveAttribute('tabindex', '-1');
			await expect(page).toHaveTitle(`${title} · ServerDock`);
		}
	});
});

test.describe('targets, zoom and reflow', () => {
	test('the Signed-out button is at least 44 px', async ({ page }) => {
		await page.goto('/signed-out');
		const box = (await page.getByRole('link', { name: 'Sign in again' }).boundingBox())!;
		expect(box.height).toBeGreaterThanOrEqual(44);
		expect(box.width).toBeGreaterThanOrEqual(44);
	});

	test('the sticky bar never covers a focused element after a real scroll', async ({ page }) => {
		test.skip(!isPhone(page), 'the sticky bar is a phone layout');
		await openOverview(page);
		await page.evaluate(() => {
			const spacer = document.createElement('div');
			spacer.id = 'spacer';
			spacer.style.height = '200vh';
			document.querySelector('main')!.insertBefore(spacer, document.querySelector('.empty'));
		});
		await page.evaluate(() => scrollTo(0, 0));
		const link = page.getByRole('link', { name: /Read the documentation/ });
		await link.focus();
		await expect.poll(() => page.evaluate(() => scrollY)).toBeGreaterThan(100); // it really scrolled (smooth scrolling takes a moment)
		await expect.poll(async () => (await link.boundingBox())!.y).toBeGreaterThanOrEqual(0);
		await page.waitForTimeout(600); // let a smooth scroll settle
		const bar = (await page.getByRole('banner').boundingBox())!;
		const top = (await link.boundingBox())!.y;
		expect(top).toBeGreaterThanOrEqual(bar.y + bar.height);
	});

	test('a root font size of 200% keeps everything reachable', async ({ page }) => {
		await openOverview(page);
		await page.evaluate(() => (document.documentElement.style.fontSize = '200%'));
		if (isPhone(page)) await page.getByRole('button', { name: 'Menu' }).click();
		await expect(page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Settings' })).toBeVisible();
		expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
		const overflow = await page.evaluate(() =>
			[...document.querySelectorAll('header, nav, main, footer')].filter((e) => e.scrollHeight > e.clientHeight + 1).map((e) => e.tagName)
		);
		expect(overflow).toEqual([]);
		await expectNoAxeViolations(page, 'root font size 200%');
	});

	test('400% zoom (320 x 256) reflows without two-dimensional scrolling', async ({ page }) => {
		await page.setViewportSize({ width: 320, height: 256 });
		await openOverview(page);
		expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
		await page.getByRole('button', { name: 'Menu' }).click();
		await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible();
		await expectNoAxeViolations(page, '320 x 256');
	});
});
