import { expect, expectNoAxeViolations, signIn, test } from './fixtures';
import { logText, mockActionError, mockDeployLog, seedDeployLog, seedService, stageRows } from './seed';

// Plan 0004 step 14, the tester's tests for the container log on the Deploy detail page:
// it is text (never markup), has an accessible name and a skip link, "Show full log"
// works, and the cases where there is no log are told honestly. The log files are real
// (seeded as the server keeps them) so the server's own cleaning is part of what is tested.
test.use({ storageState: { cookies: [], origins: [] } });

const url = (base: string, n: number) => `${base}/services/shop/api/deploys/${n}`;

/** A service whose deploy #3 failed at health, so its log was kept. Returns the service id. */
const seedFailed = (server: Parameters<typeof seedService>[0]) =>
	seedService(server, {
		project: 'shop',
		name: 'api',
		deploys: [
			{ number: 1, status: 'succeeded', wasLive: true },
			{ number: 2, status: 'succeeded', live: true },
			{ number: 3, status: 'failed', reasonCode: 'health_timeout', reason: 'the new version did not become healthy', appSafe: 'yes', stages: stageRows('blue_green', 3, 'failed') }
		]
	});

const HOSTILE = [
	'<script>window.__pwned = 1</script>',
	'<img src=x onerror="window.__pwned = 2">',
	'</pre><h1 id="injected">injected heading</h1>',
	'<svg onload="window.__pwned = 3"></svg>',
	'\x1b[31mred text\x1b[0m after colour',
	'\x1b]0;window-title-set-by-app\x07 after osc',
	'\x1b[2J\x1b[H after clear screen',
	'\x1b]8;;https://evil.example/\x07click me\x1b]8;;\x07',
	'‮detros evil‬ and ⁦isolated⁩ text',
	'nul\x00byte and bell\x07 and backspace\x08 and del\x7f',
	'javascript:alert(1) and data:text/html,<b>x</b>',
	'--- container output (forged) ---',
	'ServerDock deploy log',
	'cr\rreturn and csi\u009b31m and ls\u2028ps\u2029 and zw\u200bsp'
];

test.describe('Deploy detail: the log is text', () => {
	test('hostile bytes in a seeded log render as literal text, with the control bytes stripped', async ({ page, server, admin }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText([...Array.from({ length: 30 }, (_, i) => `line ${i + 1}`), ...HOSTILE], { number: 3 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		const tail = page.getByTestId('log-tail');
		await expect(tail).toBeVisible();
		// Visible literally.
		for (const literal of [
			'<script>window.__pwned = 1</script>',
			'<img src=x onerror="window.__pwned = 2">',
			'</pre><h1 id="injected">injected heading</h1>',
			'<svg onload="window.__pwned = 3"></svg>',
			'[31mred text[0m after colour', // the ESC byte is gone, the rest is text
			']0;window-title-set-by-app after osc', // OSC: ESC and BEL gone
			'[2J[H after clear screen', // CSI
			']8;;https://evil.example/click me]8;;',
			'detros evil and isolated text', // the bidi controls are gone, the letters stay in the order stored
			'nulbyte and bell and backspace and del',
			'javascript:alert(1) and data:text/html,<b>x</b>',
			'--- container output (forged) ---',
			'ServerDock deploy log',
			'crreturn and csi31m and lsps and zwsp' // CR, C1 CSI, line and paragraph separators, zero-width space: all gone
		])
			await expect(tail).toContainText(literal);
		const shown = await tail.innerText();
		expect(shown, 'no control, bidi-override or isolate characters reach the page').not.toMatch(/[\x00-\x08\x0b-\x1f\x7f-\x9f\u200b-\u200f\u2028-\u202e\u2066-\u2069]/);
		// No element injection: the log box holds one text node, and nothing outside it grew.
		expect(await tail.locator('*').count()).toBe(0);
		await expect(page.locator('main script, main img, main svg, #injected')).toHaveCount(0);
		await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1);
		await expect(page.getByRole('link', { name: 'click me' })).toHaveCount(0);
		expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned), 'no injected script ran').toBeUndefined();
		// The forged separator and header line stay in the container's part: the real header is apart.
		await expect(page.getByTestId('log-header')).toHaveCount(0);
		await page.getByTestId('show-full-log').click();
		await expect(page.getByTestId('log-header')).toContainText('ServerDock deploy log');
		await expect(page.getByTestId('log-header')).not.toContainText('forged');
		await expect(page.getByTestId('log-full')).toContainText('--- container output (forged) ---');
		expect(await page.getByTestId('log-full').locator('*').count()).toBe(0);
		await expectNoAxeViolations(page, 'deploy page with a hostile log');
		// (the fixture also fails this test on any console error or CSP violation)
	});

	test('a hostile line in a deploy.log answer that skipped the server cleaning is still only text in the page', async ({ page, server, admin }) => {
		// Defence in depth: the page itself never turns the log into markup, whatever the server sent.
		seedFailed(server);
		await mockDeployLog(page, { text: logText(['<script>window.__pwned = 1</script>', '<img src=x onerror="window.__pwned = 2">', '<b>bold</b>'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-tail')).toContainText('<b>bold</b>');
		await expect(page.locator('main script, main img, main b')).toHaveCount(0);
		expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
	});
});

test.describe('Deploy detail: reading the log', () => {
	test('the log box has an accessible name and is keyboard-reachable, and the skip link jumps past it', async ({ page, server, admin }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText([...Array.from({ length: 80 }, (_, i) => `line ${i + 1} ${'x'.repeat(i === 79 ? 300 : 3)}`)], { number: 3 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		const tail = page.getByTestId('log-tail');
		await expect(page.getByRole('group', { name: /Container log \(last 40 lines\)/ })).toBeVisible();
		await expect(tail).toHaveAccessibleName('Container log (last 40 lines)');
		await expect(tail).toHaveAttribute('tabindex', '0');
		await expect(page.getByRole('heading', { level: 2, name: /Container log \(last 40 lines\)/ })).toBeVisible();
		// 80 lines, 40 shown: the newest ones.
		await expect(tail).toContainText('line 80 ');
		await expect(tail).toContainText('line 41 ');
		await expect(tail).not.toContainText('line 40 ');
		// A long line scrolls inside the box, not wrapped, not cut.
		const scrolls = await tail.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth, ws: getComputedStyle(el).whiteSpace }));
		expect(scrolls.ws).toBe('pre');
		expect(scrolls.sw).toBeGreaterThan(0);
		// The box can be focused by keyboard.
		await tail.focus();
		await expect(tail).toBeFocused();
		// Skip link: first thing before the box in the tab order, hidden until focused, 44 px, leads past the log.
		const skip = page.getByTestId('skip-log');
		await expect(skip).toHaveText('Skip the container log');
		await skip.focus();
		await expect(skip).toBeInViewport();
		const box = (await skip.boundingBox())!;
		expect(box.height).toBeGreaterThanOrEqual(43.5);
		await page.keyboard.press('Enter');
		await expect(page.getByTestId('show-full-log')).toBeFocused();
		expect(page.url(), 'the skip link does not leave the page or change its address').toBe(url(server.baseURL, 3));
		// The order is skip link, then the box, then the button.
		await page.getByRole('main').getByRole('link', { name: 'Skip the container log' }).focus();
		await page.keyboard.press('Tab');
		await expect(tail).toBeFocused();
		await page.keyboard.press('Tab');
		await expect(page.getByTestId('show-full-log')).toBeFocused();
		await expectNoAxeViolations(page, 'deploy page with the log tail');
	});

	test('"Show full log" shows the header apart and every line, and "Show last 40 lines only" goes back', async ({ page, server, admin }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText(Array.from({ length: 60 }, (_, i) => `line ${i + 1}`), { number: 3 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		const button = page.getByTestId('show-full-log');
		await expect(button).toHaveText('Show full log');
		await expect(button, 'a changing label, so no aria-expanded as well').not.toHaveAttribute('aria-expanded', /.*/);
		await expect(page.getByTestId('log-tail')).not.toContainText('line 1\n');
		await button.click();
		await expect(button).toHaveText('Show last 40 lines only');
		await expect(page.getByRole('status')).toHaveText('Full log shown.');
		await expect(button, 'focus stays on the button').toBeFocused();
		const full = page.getByTestId('log-full');
		await expect(full).toHaveAccessibleName('Printed by the container');
		await expect(full).toContainText('line 1');
		await expect(full).toContainText('line 60');
		await expect(full).not.toContainText('ServerDock deploy log');
		await expect(page.getByTestId('log-tail')).toHaveCount(0);
		const header = page.getByTestId('log-header');
		await expect(header).toHaveAccessibleName('Written by ServerDock');
		await expect(header).toContainText('health_timeout');
		await expect(header).toContainText('failed health');
		await expect(page.getByRole('heading', { level: 2, name: /full log/ })).toBeVisible();
		await expect(page.getByRole('heading', { level: 3 })).toHaveText(['Written by ServerDock', 'Printed by the container']);
		await expectNoAxeViolations(page, 'deploy page with the full log');
		await button.click();
		await expect(button).toHaveText('Show full log');
		await expect(page.getByRole('status')).toHaveText('Last 40 lines shown.');
		await expect(page.getByTestId('log-full')).toHaveCount(0);
		await expect(page.getByTestId('log-header')).toHaveCount(0);
		await expect(page.getByTestId('log-tail')).toBeVisible();
	});

	test('a log whose container printed nothing says so', async ({ page, server, admin }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText([], { number: 3 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-tail')).toHaveText('(the container printed nothing)');
		await expectNoAxeViolations(page, 'deploy page with an empty log');
	});

	test('a log cut at 1 MiB says so', async ({ page, server, admin }) => {
		seedFailed(server);
		await mockDeployLog(page, { text: logText(['a line'], { number: 3 }), truncated: true });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-truncated')).toContainText('only the first 1 MiB is shown');
		await expectNoAxeViolations(page, 'deploy page with a truncated log');
	});

	test('the log is asked for once when the deploy has finished (not by polling, not again on a new look)', async ({ page, server, admin }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText(['hello'], { number: 3 }));
		let asked = 0;
		await page.route('**/api/v1/actions/deploys.log', (route) => {
			asked++;
			return route.continue();
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-tail')).toContainText('hello');
		await page.getByTestId('show-full-log').click();
		await page.getByTestId('show-full-log').click();
		await page.waitForTimeout(2500);
		expect(asked).toBe(1);
	});
});

test.describe('Deploy detail: when there is no log to show', () => {
	test('"Log no longer kept" for a deploy older than the last 10 is told plainly, with no axe violations', async ({ page, server, admin }) => {
		seedService(server, {
			project: 'shop',
			name: 'api',
			deploys: [{ number: 1, status: 'succeeded', wasLive: true }, ...Array.from({ length: 10 }, (_, i) => ({ number: i + 2, status: 'succeeded' as const })), { number: 12, status: 'succeeded' as const, live: true }]
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 1));
		await expect(page.getByTestId('deploy-status')).toContainText('Replaced');
		await expect(page.getByTestId('log-message')).toContainText('Log no longer kept');
		await expect(page.getByTestId('log-message')).toContainText('10');
		for (const id of ['log-tail', 'log-full', 'show-full-log', 'skip-log', 'log-retry', 'log-truncated']) await expect(page.getByTestId(id)).toHaveCount(0);
		await expect(page.getByTestId('rollback')).toBeVisible();
		await expectNoAxeViolations(page, 'deploy page: log no longer kept');
	});

	test('a finished deploy whose log file is missing says so and does not invent one', async ({ page, server, admin }) => {
		seedFailed(server); // no log file written for #3
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-message')).toBeVisible();
		await expect(page.getByTestId('log-message')).toContainText(/no longer kept|No log|not kept/i);
		await expect(page.getByTestId('log-tail')).toHaveCount(0);
	});

	test('503 "try again in a moment": shown as it is, with Try again, which fetches the log when the server is ready', async ({ page, server, admin, allow5xx }) => {
		const id = seedFailed(server);
		seedDeployLog(server, id, 3, logText(['recovered line'], { number: 3 }));
		allow5xx();
		await mockActionError(page, 'deploys.log', 503, 'unavailable', 'the log is being written: try again in a moment', 2);
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		const message = page.getByTestId('log-message');
		await expect(message).toContainText('The log could not be read yet');
		await expect(message).toContainText('try again in a moment');
		await expect(page.getByTestId('log-tail')).toHaveCount(0);
		const retry = page.getByTestId('log-retry');
		await expect(retry).toBeVisible();
		expect((await retry.boundingBox())!.height).toBeGreaterThanOrEqual(43.5);
		await expectNoAxeViolations(page, 'deploy page: log 503');
		await retry.click(); // still busy: honest again, the button is still there
		await expect(message).toContainText('try again in a moment');
		await expect(retry).toBeVisible();
		await retry.click(); // now the server answers
		await expect(page.getByTestId('log-tail')).toContainText('recovered line');
		await expect(page.getByTestId('log-retry')).toHaveCount(0);
		await expect(page.getByTestId('log-message')).toHaveCount(0);
		await expect(page.getByRole('alert')).toHaveText('');
	});

	test('a deploy that has not finished says there is no log yet, and asks nothing', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });
		let asked = 0;
		await page.route('**/api/v1/actions/deploys.log', (route) => {
			asked++;
			return route.continue();
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 3));
		await expect(page.getByTestId('log-message')).toContainText('There is no log while the deploy runs');
		await page.waitForTimeout(500);
		expect(asked).toBe(0);
	});

	test('a deploy that does not exist says so', async ({ page, server, admin, allow5xx }) => {
		seedFailed(server);
		allow5xx();
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 99));
		await expect(page.getByRole('heading', { level: 1 })).toHaveText('Deploy not found');
		await expectNoAxeViolations(page, 'deploy page: not found');
	});
});
