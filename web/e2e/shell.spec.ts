import type { Page } from '@playwright/test';
import { expect, expectNoAxeViolations, test } from './fixtures';

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
		await expect(page.getByRole('link', { name: 'Read the documentation' })).toBeVisible();
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
		await page.getByRole('link', { name: 'Read the documentation' }).focus();
		const top = (await page.getByRole('link', { name: 'Read the documentation' }).boundingBox())!.y;
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
