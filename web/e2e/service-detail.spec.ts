import type { Page } from '@playwright/test';
import { countApiRequests, expect, expectNoAxeViolations, expireStepUp, nextCode, signIn, test } from './fixtures';
import { deployFixture, mockActionError, mockServiceDetail, seedHistory, seedService, serviceFixture } from './seed';

// Plan 0004 step 13: the service detail page, its Deploys and Settings sub-pages and
// their dialogs. The implementer's smoke tests; the tester extends them to every
// listed screen state in all 8 projects.
test.use({ storageState: { cookies: [], origins: [] } });

const dialog = (page: Page, name: string | RegExp) => page.getByRole('dialog', { name });
const isPhone = (page: Page) => (page.viewportSize()?.width ?? 1280) < 768;

/** Opens a service's page; `sub` is "" (Deploys) or "/settings". */
async function open(page: Page, baseURL: string, ref: string, sub = '') {
	await page.goto(`${baseURL}/services/${ref}${sub}`);
	await expect(page.getByRole('heading', { level: 1 })).toContainText(ref.split('/')[1]);
}

test.describe('Service detail: seeded history', () => {
	test('shows the header, the three cards, the sub-navigation and every result kind', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		const h1 = page.getByRole('heading', { level: 1 });
		await expect(h1).toContainText('api');
		await expect(h1).toContainText('Unknown'); // no Docker socket: the state is unknown, said in words
		for (const b of ['Deploy', 'Redeploy', 'Restart', 'Refresh']) await expect(page.getByRole('button', { name: b, exact: true })).toBeVisible();
		await expect(page.getByTestId('card-running')).toContainText('#8');
		await expect(page.getByTestId('card-traffic')).toContainText(':8081');
		await expect(page.getByTestId('card-traffic')).toContainText('same address as the panel');
		await expect(page.getByTestId('card-strategy')).toContainText('Blue-green');
		const nav = page.getByRole('navigation', { name: 'Service sections' });
		await expect(nav.getByRole('link', { name: 'Deploys' })).toHaveAttribute('aria-current', 'page');
		await expect(nav.getByRole('link', { name: 'Settings' })).not.toHaveAttribute('aria-current', 'page');
		const row = (n: number) => page.locator(`[data-row="${n}"]:visible`);
		await expect(row(1)).toContainText('○ Replaced');
		await expect(row(2)).toContainText('▲ Failed · app safe');
		await expect(row(3)).toContainText('▲ Failed · app not safe');
		await expect(row(4)).toContainText('◌ Interrupted');
		await expect(row(5)).toContainText('⊘ Cancelled');
		await expect(row(6)).toContainText('⊘ Superseded by #7');
		await expect(row(7)).toContainText('○ Replaced');
		await expect(row(8)).toContainText('● Live');
		await expect(row(9)).toContainText('○ Queued');
		await expect(row(7)).toContainText('API token');
		await expect(row(8)).toContainText('ServerDock itself');
		await expect(row(1)).toContainText('2026-11-11 09:00 UTC');
		// Roll back only where a deploy was once live and is not live now.
		for (const n of [1, 7]) await expect(row(n).getByRole('button', { name: `Roll back to #${n}` })).toBeVisible();
		for (const n of [2, 3, 4, 5, 6, 8, 9]) await expect(row(n).getByRole('button', { name: /Roll back/ })).toHaveCount(0);
		await expect(row(8).getByRole('link', { name: 'Details of deploy #8' })).toHaveAttribute('href', /\/services\/shop\/api\/deploys\/8$/);
		if (isPhone(page)) await expect(page.locator('table:visible')).toHaveCount(0);
		else await expect(page.getByRole('table', { name: /Deploys of shop\/api/ })).toBeVisible();
		await expectNoAxeViolations(page, 'service detail, history');
	});

	test('loads on navigation only, asks again on Refresh, and sends nothing while idle', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.locator('[data-row="8"]:visible')).toBeVisible();
		const reqs = countApiRequests(page);
		await page.waitForTimeout(3000);
		expect(reqs.seen.filter((p) => !p.endsWith('/actions/system.info'))).toEqual([]);
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Updated.' })).toBeVisible();
		expect(reqs.seen.filter((p) => p.endsWith('/actions/services.get')).length).toBe(1);
		expect(reqs.seen.filter((p) => p.endsWith('/actions/deploys.list')).length).toBe(1);
	});

	test('the page reflows: no horizontal scrolling on Deploys or Settings, and the 44 px targets hold', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		for (const sub of ['', '/settings']) {
			await open(page, server.baseURL, 'shop/api', sub);
			await expect(page.getByRole('navigation', { name: 'Service sections' })).toBeVisible();
			const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
			expect(over, `horizontal overflow on ${sub || 'Deploys'}`).toBeLessThanOrEqual(0);
			const small = await page.evaluate(() =>
				[...document.querySelectorAll<HTMLElement>('main button, main a[href], nav[aria-label="Service sections"] a')]
					.filter((e) => e.getBoundingClientRect().width > 0 && !e.closest('p, li > p, .hint, .note') )
					.filter((e) => e.getBoundingClientRect().height < 43.5)
					.map((e) => `${e.tagName} ${e.textContent?.trim().slice(0, 30)} ${Math.round(e.getBoundingClientRect().height)}`)
			);
			expect(small, `targets under 44 px on ${sub || 'Deploys'}`).toEqual([]);
		}
	});

	test('an unknown service says so', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/shop/nope`);
		await expect(page.getByRole('heading', { name: 'Service not found', level: 1 })).toBeVisible();
		await expectNoAxeViolations(page, 'service not found');
	});
});

test.describe('Service detail: dialogs', () => {
	test('Restart states the downtime; Cancel has the focus, Escape closes, focus returns to the opener', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		const opener = page.getByRole('button', { name: 'Restart', exact: true });
		await opener.click();
		const d = dialog(page, 'Restart shop/api?');
		await expect(d).toContainText('down while its container restarts');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await expect(d.getByRole('button', { name: 'Restart now' })).toBeVisible();
		await expectNoAxeViolations(page, 'restart dialog');
		await page.keyboard.press('Escape');
		await expect(d).toBeHidden();
		await expect(opener).toBeFocused();
	});

	test('Deploy asks for a tag or digest of the current repository; Redeploy and Roll back name their target', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Deploy', exact: true }).click();
		const dd = dialog(page, 'Deploy shop/api');
		await expect(dd.getByLabel('Tag or digest')).toHaveValue('registry.example.com/shop/api:2.4.1');
		await expect(dd).toContainText('The repository stays registry.example.com/shop/api');
		await expect(dd.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await expectNoAxeViolations(page, 'deploy dialog');
		await dd.getByRole('button', { name: 'Cancel' }).click();
		await expect(dd).toBeHidden();
		await page.getByRole('button', { name: 'Redeploy', exact: true }).click();
		const rd = dialog(page, 'Redeploy shop/api?');
		await expect(rd).toContainText('live deploy #8');
		await expect(rd.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await page.keyboard.press('Escape');
		await expect(rd).toBeHidden();
		await page.locator('[data-row="1"]:visible').getByRole('button', { name: 'Roll back to #1' }).click();
		const bd = dialog(page, 'Roll back shop/api to #1?');
		await expect(bd).toContainText('registry.example.com/shop/api:2.3.0');
		await expect(bd.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await expect(bd.getByRole('button', { name: 'Roll back to #1' })).toBeVisible();
		await expectNoAxeViolations(page, 'roll back dialog');
		await page.keyboard.press('Escape');
		await expect(bd).toBeHidden();
	});
});

test.describe('Service detail: cards for states that need no containers', () => {
	const detail = (over: Record<string, unknown> = {}) =>
		serviceFixture({ project: 'shop', name: 'api', public_port: 8081, active_deploy: 2, ...over });
	const history = () => [deployFixture({ number: 2, active: true, was_live: true }), deployFixture({ number: 1, was_live: true })];

	test('a route error, the SEC6 warning and a missing container', async ({ page, server, admin }) => {
		await mockServiceDetail(
			page,
			detail({ state: 'missing', route_error: 'port 8081 is in use by another program', warnings: [{ code: 'shares_panel_address', message: 'This app is reachable on the same address as the panel (203.0.113.5:8081).' }] }),
			history()
		);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByTestId('route-error')).toContainText('port 8081 is in use by another program');
		await expect(page.getByTestId('sec6')).toContainText('This panel was opened on 127.0.0.1');
		await expect(page.getByTestId('sec6').getByRole('link', { name: /Read about this/ })).toHaveAttribute('href', /security\.md/);
		await expect(page.getByTestId('state-missing')).toContainText('cannot find the container');
		await expect(page.getByRole('heading', { level: 1 })).toContainText('Missing');
		await expectNoAxeViolations(page, 'service detail, route error, SEC6, missing');
	});

	test('a failed load says the status is unknown', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		seedHistory(server);
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/services.get', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"boom"}}' }));
		await page.goto(`${server.baseURL}/services/shop/api`);
		await expect(page.getByText('Status unknown: could not load this service')).toBeVisible();
		await expect(page.getByTestId('card-running')).toHaveCount(0);
		await expectNoAxeViolations(page, 'service detail, load failed');
	});
});

test.describe('Service detail: Settings', () => {
	test('saves the settings and the image repository: "Saved. Deploy to apply."', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await expect(page.getByRole('navigation', { name: 'Service sections' }).getByRole('link', { name: 'Settings' })).toHaveAttribute('aria-current', 'page');
		await page.getByLabel('Drain time (seconds)').fill('20');
		await page.getByRole('button', { name: 'Save settings' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Saved. Deploy to apply.' }).first()).toBeVisible();
		expect(server.query<{ drain_s: number }>(`SELECT drain_s FROM services WHERE name = 'api'`)[0].drain_s).toBe(20);
		await page.getByLabel('Drain time (seconds)').fill('5');
		await page.getByRole('button', { name: 'Save settings' }).click();
		await expect(page.getByRole('alert').filter({ hasText: 'needs fixing' })).toBeVisible();
		await expect(page.getByLabel('Drain time (seconds)')).toHaveValue('5'); // typed input kept
		await page.getByLabel('Image', { exact: true }).fill('registry.example.com/shop/api-v2:1');
		await page.getByRole('button', { name: 'Change image repository' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Saved. Deploy to apply.' }).last()).toBeVisible();
		await expectNoAxeViolations(page, 'settings, saved');
	});

	test('the public-port dialogs state what is applied now; Cancel changes nothing', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('textbox', { name: 'Public port' }).fill('8090');
		await page.getByRole('button', { name: 'Change public port' }).click();
		const d = dialog(page, 'Change the public port of shop/api to 8090?');
		await expect(d).toContainText("Applied now: exposes the app on the internet at http://127.0.0.1:8090, unless your host's firewall blocks the port.");
		await expect(d).toContainText('no longer reachable at http://127.0.0.1:8081');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await expectNoAxeViolations(page, 'public port dialog, change');
		await d.getByRole('button', { name: 'Cancel' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'The public port was not changed' })).toBeVisible();
		expect(server.query<{ public_port: number }>(`SELECT public_port FROM services WHERE name = 'api'`)[0].public_port).toBe(8081);
		await page.getByRole('button', { name: 'Stop exposing' }).click();
		const c = dialog(page, 'Stop exposing shop/api?');
		await expect(c).toContainText('Applied now: the app is no longer reachable at http://127.0.0.1:8081');
		await expectNoAxeViolations(page, 'public port dialog, clear');
		await page.keyboard.press('Escape');
		expect(server.query<{ public_port: number }>(`SELECT public_port FROM services WHERE name = 'api'`)[0].public_port).toBe(8081);
	});

	test('"Try again" is offered for a route error and calls services.retry_port', async ({ page, server, admin }) => {
		await mockServiceDetail(page, serviceFixture({ project: 'shop', name: 'api', public_port: 8081, route_error: 'port 8081 is in use by another program' }), [deployFixture({ number: 1, active: true, was_live: true })]);
		await signIn(page, server, admin);
		let asked = 0;
		await page.route('**/api/v1/actions/services.retry_port', (route) => {
			asked++;
			return route.fulfill({
				status: 200,
				contentType: 'application/json',
				body: JSON.stringify({ audit: 'not_audited', data: { service: serviceFixture({ project: 'shop', name: 'api', public_port: 8081 }), applied: false, notice: 'Saved, but port 8081 is in use by another program. Use Try again once it is free.' } })
			});
		});
		await open(page, server.baseURL, 'shop/api', '/settings');
		await expect(page.getByTestId('port-route-error')).toContainText('in use by another program');
		await page.getByRole('button', { name: 'Try again' }).click();
		await expect(page.getByTestId('port-notice')).toContainText('Use Try again once it is free');
		expect(asked).toBe(1);
		await expectNoAxeViolations(page, 'settings, route error');
	});

	test('Delete needs the exact target, then the step-up; "confirm again" offers Delete again', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		expireStepUp(server);
		// The server refuses both tries: the window passed again. (The real job-level refusal is a unit test in plan 0004 step 9.)
		await mockActionError(page, 'services.delete', 403, 'mfa_required', 'fresh two-factor confirmation required');
		await open(page, server.baseURL, 'shop/api', '/settings');
		const opener = page.getByRole('button', { name: 'Delete service' });
		await opener.click();
		const d = dialog(page, 'Delete shop/api?');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await expect(d).toContainText('cannot be undone');
		await d.getByLabel('Type shop/api to confirm').fill('shop/ap');
		await d.getByRole('button', { name: 'Delete service' }).click({ force: true }); // aria-disabled until the target matches, but it still answers
		await expect(d).toContainText('Type shop/api exactly');
		await expect(d).toBeVisible(); // a wrong target does not close it
		await expectNoAxeViolations(page, 'delete dialog, wrong target');
		await d.getByLabel('Type shop/api to confirm').fill('shop/api');
		await d.getByRole('button', { name: 'Delete service' }).click();
		const step = dialog(page, 'Confirm it is you');
		await expect(step).toBeVisible();
		await expectNoAxeViolations(page, 'delete, step-up');
		await step.getByLabel('Code from your authenticator').fill(nextCode(admin));
		await step.getByRole('button', { name: 'Confirm' }).click();
		await expect(page.getByRole('alert').filter({ hasText: 'Confirming again is needed' })).toBeVisible();
		await expect(page.getByRole('button', { name: 'Delete again' })).toBeVisible();
		expect(server.query(`SELECT 1 FROM services WHERE name = 'api'`).length).toBe(1);
		await expectNoAxeViolations(page, 'delete, confirm again');
	});

	test('a service whose delete did not finish offers Delete again', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', containerPort: 8080, needsAttention: 'delete_interrupted', deploys: [{ number: 1, live: true }] });
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await expect(page.getByRole('button', { name: 'Delete again' })).toBeVisible();
		await expect(page.getByTestId('needs-attention')).toContainText('did not finish');
	});
});



// ---------------------------------------------------------------------------------
// Step 13 tests (the tester's extension): every screen state, every dialog, the
// keyboard-only paths and the real-Docker flows.
// ---------------------------------------------------------------------------------

/** The width the page and an open dialog overflow by, and the dialog's controls under 44 px. */
async function dialogGeometry(page: Page) {
	return page.evaluate(() => {
		const d = document.querySelector<HTMLDialogElement>('dialog[open]')!;
		const r = d.getBoundingClientRect();
		const small = [...d.querySelectorAll<HTMLElement>('button, input, a[href]')]
			.filter((e) => e.getBoundingClientRect().width > 0 && e.getBoundingClientRect().height < 43.5)
			.map((e) => `${e.tagName} ${e.textContent?.trim().slice(0, 20)} ${Math.round(e.getBoundingClientRect().height)}`);
		return {
			pageOver: document.documentElement.scrollWidth - document.documentElement.clientWidth,
			dialogOver: d.scrollWidth - d.clientWidth,
			left: r.left,
			right: r.right,
			vw: document.documentElement.clientWidth,
			small
		};
	});
}

const running = (over: Record<string, unknown> = {}) =>
	serviceFixture({ project: 'shop', name: 'api', image_ref: 'registry.example.com/shop/api:2.4.1', container_port: 8080, public_port: 8081, active_deploy: 8, state: 'running', ...over });
const runningHistory = () => [
	deployFixture({ number: 8, active: true, was_live: true, image_ref: 'registry.example.com/shop/api:2.4.1', triggered_by: 'system' }),
	deployFixture({ number: 7, kind: 'redeploy', was_live: true, triggered_by: 'token' }),
	deployFixture({ number: 3, status: 'failed', stage: 'health', reason_code: 'health_timeout', reason: 'the new version did not become healthy', app_safe: 'yes' }),
	deployFixture({ number: 1, was_live: true })
];

test.describe('Service detail: each screen state, with zero axe violations', () => {
	test('a running service with history: desktop table or phone cards', async ({ page, server, admin }) => {
		await mockServiceDetail(page, running(), runningHistory());
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByRole('heading', { level: 1 })).toContainText('Running');
		await expect(page.locator('[data-row="8"]:visible')).toContainText('● Live');
		await expect(page.locator('[data-row="3"]:visible')).toContainText('▲ Failed · app safe');
		await expect(page.locator('[data-row="3"]:visible').getByRole('button', { name: /Roll back/ })).toHaveCount(0);
		await expect(page.locator('[data-row="7"]:visible').getByRole('button', { name: 'Roll back to #7' })).toBeVisible();
		// The table shows at 768 px and up, the cards below (A9): never both.
		if (isPhone(page)) {
			await expect(page.locator('table:visible')).toHaveCount(0);
			await expect(page.locator('[data-row]:visible')).toHaveCount(4);
		} else {
			await expect(page.getByRole('table', { name: /Deploys of shop\/api/ })).toBeVisible();
			await expect(page.locator('[data-row]:visible')).toHaveCount(4);
		}
		await expectNoAxeViolations(page, 'running service with history');
	});

	test('a route error', async ({ page, server, admin }) => {
		await mockServiceDetail(page, running({ route_error: 'port 8081 is in use by another program' }), runningHistory());
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByTestId('route-error')).toContainText('port 8081 is in use by another program');
		await expectNoAxeViolations(page, 'route error');
	});

	test('the SEC6 warning names the address and links the explanation', async ({ page, server, admin }) => {
		await mockServiceDetail(
			page,
			running({ warnings: [{ code: 'shares_panel_address', message: 'This app is reachable on the same address as the panel (203.0.113.5:8081).' }] }),
			runningHistory()
		);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByTestId('sec6')).toContainText('same address as the panel');
		await expect(page.getByTestId('sec6').getByRole('link', { name: /Read about this/ })).toHaveAttribute('href', /security\.md/);
		await expectNoAxeViolations(page, 'SEC6 warning');
	});

	test('a missing container says what ServerDock cannot find', async ({ page, server, admin }) => {
		await mockServiceDetail(page, running({ state: 'missing' }), runningHistory());
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByRole('heading', { level: 1 })).toContainText('Missing');
		await expect(page.getByTestId('state-missing')).toContainText('cannot find the container');
		await expectNoAxeViolations(page, 'missing container');
	});

	test('the Settings page of a service with a route error', async ({ page, server, admin }) => {
		await mockServiceDetail(page, running({ route_error: 'port 8081 is in use by another program' }), runningHistory());
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await expect(page.getByTestId('port-route-error')).toBeVisible();
		await expectNoAxeViolations(page, 'settings with a route error');
	});

	test('the Delete dialog with the right target, before the step-up', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('button', { name: 'Delete service' }).click();
		const d = dialog(page, 'Delete shop/api?');
		await d.getByLabel('Type shop/api to confirm').fill('shop/api');
		await expect(d.getByRole('button', { name: 'Delete service' })).not.toHaveAttribute('aria-disabled', 'true');
		await expectNoAxeViolations(page, 'delete dialog, right target');
	});
});

test.describe('Service detail: every dialog', () => {
	// Each row: where it is, how it opens, what its title and body must say, what the opener is.
	type Case = { name: string; sub: string; seed?: 'history' | 'plain'; open: (p: Page) => ReturnType<Page['getByRole']>; title: string; says: RegExp[]; confirm: string };
	const cases: Case[] = [
		{ name: 'Deploy', sub: '', open: (p) => p.getByRole('button', { name: 'Deploy', exact: true }), title: 'Deploy shop/api', says: [/pulls the image below again/, /Blue-green|blue-green|brief|old version/i], confirm: 'Deploy' },
		{ name: 'Redeploy', sub: '', open: (p) => p.getByRole('button', { name: 'Redeploy', exact: true }), title: 'Redeploy shop/api?', says: [/live deploy #8/, /Nothing is pulled again/], confirm: 'Redeploy' },
		{ name: 'Restart', sub: '', open: (p) => p.getByRole('button', { name: 'Restart', exact: true }), title: 'Restart shop/api?', says: [/down while its container restarts/], confirm: 'Restart now' },
		{ name: 'Roll back', sub: '', open: (p) => p.locator('[data-row="7"]:visible').getByRole('button', { name: 'Roll back to #7' }), title: 'Roll back shop/api to #7?', says: [/deploy #7/, /registry\.example\.com\/shop\/api:2\.4\.1/, /once live/], confirm: 'Roll back to #7' },
		{ name: 'Public port, change', sub: '/settings', open: (p) => p.getByRole('button', { name: 'Change public port' }), title: 'Change the public port of shop/api to 8090?', says: [/Applied now: exposes the app on the internet at http:\/\/127\.0\.0\.1:8090/, /no longer reachable at http:\/\/127\.0\.0\.1:8081/], confirm: 'Change to port 8090' },
		{ name: 'Public port, expose', sub: '/settings', seed: 'plain', open: (p) => p.getByRole('button', { name: 'Expose on this port' }), title: 'Expose shop/api on port 8090?', says: [/Applied now: exposes the app on the internet at http:\/\/127\.0\.0\.1:8090, unless your host's firewall blocks the port/], confirm: 'Expose on port 8090' },
		{ name: 'Public port, stop exposing', sub: '/settings', open: (p) => p.getByRole('button', { name: 'Stop exposing' }), title: 'Stop exposing shop/api?', says: [/Applied now: the app is no longer reachable at http:\/\/127\.0\.0\.1:8081/], confirm: 'Stop exposing' },
		{ name: 'Delete', sub: '/settings', open: (p) => p.getByRole('button', { name: 'Delete service' }), title: 'Delete shop/api?', says: [/cannot be undone/, /stops and removes the containers of shop\/api/, /fresh authenticator code/], confirm: 'Delete service' }
	];
	for (const c of cases) {
		test(`${c.name}: names the target and the consequence, Cancel has the focus, Escape and Cancel give focus back to the opener`, async ({ page, server, admin }) => {
			if (c.seed === 'plain') seedService(server, { project: 'shop', name: 'api', image: 'registry.example.com/shop/api:2.4.1', containerPort: 8080, deploys: [{ number: 1, live: true }] });
			else seedHistory(server);
			await signIn(page, server, admin);
			await open(page, server.baseURL, 'shop/api', c.sub);
			if (c.name.startsWith('Public port') && c.name !== 'Public port, stop exposing') await page.getByRole('textbox', { name: 'Public port' }).fill('8090');
			const opener = c.open(page);
			const d = dialog(page, c.title);
			for (const how of ['Escape', 'Cancel'] as const) {
				await opener.click();
				await expect(d).toBeVisible();
				for (const re of c.says) await expect(d).toContainText(re);
				await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
				await expect(d.getByRole('button', { name: c.confirm, exact: true })).toBeVisible();
				if (how === 'Escape') {
					await expectNoAxeViolations(page, `${c.name} dialog`);
					const g = await dialogGeometry(page);
					expect(g.pageOver, 'page overflow with the dialog open').toBeLessThanOrEqual(0);
					expect(g.dialogOver, 'dialog overflow').toBeLessThanOrEqual(0);
					expect(g.left, 'dialog off the left edge').toBeGreaterThanOrEqual(0);
					expect(g.right, 'dialog off the right edge').toBeLessThanOrEqual(g.vw);
					expect(g.small, 'dialog targets under 44 px').toEqual([]);
					await page.keyboard.press('Escape');
				} else await d.getByRole('button', { name: 'Cancel' }).click();
				await expect(d).toBeHidden();
				await expect(opener).toBeFocused();
			}
		});
	}

	test('nothing was started by a cancelled Deploy, Redeploy, Restart or Roll back', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		const reqs = countApiRequests(page);
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		await page.keyboard.press('Escape');
		await page.getByRole('button', { name: 'Redeploy', exact: true }).click();
		await dialog(page, 'Redeploy shop/api?').getByRole('button', { name: 'Cancel' }).click();
		await page.getByRole('button', { name: 'Deploy', exact: true }).click();
		await dialog(page, 'Deploy shop/api').getByRole('button', { name: 'Cancel' }).click();
		await page.locator('[data-row="1"]:visible').getByRole('button', { name: 'Roll back to #1' }).click();
		await page.keyboard.press('Escape');
		await page.waitForTimeout(300);
		expect(reqs.seen.filter((p) => /services\.(deploy|redeploy|restart|rollback)$/.test(p))).toEqual([]);
		expect(server.query(`SELECT 1 FROM deploys WHERE service_id = (SELECT id FROM services WHERE name = 'api')`).length).toBe(9);
		expect(server.query(`SELECT 1 FROM jobs`).length).toBe(0);
	});

	test('the Deploy dialog refuses an empty image and keeps the dialog open', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Deploy', exact: true }).click();
		const d = dialog(page, 'Deploy shop/api');
		await d.getByLabel('Tag or digest').fill('');
		await d.getByRole('button', { name: 'Deploy', exact: true }).click();
		await expect(d).toBeVisible();
		await expect(d).toContainText('Enter the image to deploy');
		expect(server.query(`SELECT 1 FROM jobs`).length).toBe(0);
		await expectNoAxeViolations(page, 'deploy dialog, empty image');
	});

	test('a deploy the server refuses shows its reason inside the dialog with the typed text kept', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await mockActionError(page, 'services.deploy', 400, 'invalid_input', 'image_ref: not a valid image reference');
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Deploy', exact: true }).click();
		const d = dialog(page, 'Deploy shop/api');
		await d.getByLabel('Tag or digest').fill('registry.example.com/shop/api:bad tag');
		await d.getByRole('button', { name: 'Deploy', exact: true }).click();
		await expect(d).toBeVisible();
		await expect(d).toContainText('not a valid image reference');
		await expect(d.getByLabel('Tag or digest')).toHaveValue('registry.example.com/shop/api:bad tag');
		await expectNoAxeViolations(page, 'deploy dialog, refused');
	});
});

test.describe('Service detail: refusals and errors', () => {
	test('versions_kept: a real refusal says to wait once, and the service is untouched', async ({ page, server, admin }) => {
		seedHistory(server, 'shop', 'api', { needsAttention: 'versions_kept' });
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		await dialog(page, 'Restart shop/api?').getByRole('button', { name: 'Restart now' }).click();
		const alert = page.getByRole('alert').filter({ hasText: 'Try again once the service has settled.' });
		await expect(alert).toBeVisible();
		await expect(alert).not.toContainText('afterwards'); // the server's "try again afterwards" is not repeated
		expect((await alert.innerText()).match(/try again/gi)?.length).toBe(1);
		await expect(page.getByRole('status').filter({ hasText: 'Restart queued' })).toHaveCount(0);
		await expectNoAxeViolations(page, 'restart refused (versions_kept)');
		// "Try again" really is another go: the next restart is asked for again.
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		await expect(dialog(page, 'Restart shop/api?')).toBeVisible();
	});

	test('retry_port: "Try again" shows the saved state and the notice when the port is still blocked', async ({ page, server, admin }) => {
		await mockServiceDetail(page, running({ route_error: 'port 8081 is in use by another program' }), runningHistory());
		await signIn(page, server, admin);
		let asked = 0;
		await page.route('**/api/v1/actions/services.retry_port', (route) => {
			asked++;
			const ok = asked > 1;
			return route.fulfill({
				status: 200,
				contentType: 'application/json',
				body: JSON.stringify({ audit: 'not_audited', data: { service: running(), applied: ok, ...(ok ? {} : { notice: 'Saved, but port 8081 is in use by another program. Use Try again once it is free.' }) } })
			});
		});
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('button', { name: 'Try again' }).click();
		await expect(page.getByTestId('port-notice')).toContainText('Use Try again once it is free');
		await expectNoAxeViolations(page, 'retry_port, still blocked');
		await page.getByRole('button', { name: 'Try again' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Applied now: the proxy matches the saved routes.' })).toBeVisible();
		await expect(page.getByTestId('port-notice')).toHaveCount(0);
		expect(asked).toBe(2);
		await expectNoAxeViolations(page, 'retry_port, applied');
	});

	test('a Redeploy of a service with no live version is not offered as an action that works', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', containerPort: 8080, deploys: [{ number: 1, status: 'failed', reasonCode: 'health_timeout', reason: 'x', appSafe: 'yes' }] });
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.locator('[data-row="1"]:visible').getByRole('button', { name: /Roll back/ })).toHaveCount(0);
		await page.getByRole('button', { name: 'Redeploy', exact: true }).click({ force: true });
		await expect(dialog(page, /Redeploy/)).toHaveCount(0); // no live deploy: no dialog, no job
		expect(server.query(`SELECT 1 FROM jobs`).length).toBe(0);
	});
});

test.describe('Service detail: keyboard only', () => {
	test('Restart: open with Enter, Tab to the verb button, Enter queues it', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		let restarts = 0;
		await page.route('**/api/v1/actions/services.restart', (r) => {
			restarts++;
			return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'audited', data: { status: 'queued' } }) });
		});
		await open(page, server.baseURL, 'shop/api');
		const opener = page.getByRole('button', { name: 'Restart', exact: true });
		await opener.focus();
		await page.keyboard.press('Enter');
		const d = dialog(page, 'Restart shop/api?');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await page.keyboard.press('Tab');
		await expect(d.getByRole('button', { name: 'Restart now' })).toBeFocused();
		expect(restarts).toBe(0);
		await page.keyboard.press('Enter');
		await expect(d).toBeHidden();
		await expect(page.getByRole('status').filter({ hasText: 'Restart queued' })).toBeVisible();
		expect(restarts).toBe(1);
	});

	test('Tab never reaches the page behind a modal dialog', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		const d = dialog(page, 'Restart shop/api?');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		for (let i = 0; i < 6; i++) {
			await page.keyboard.press('Tab');
			// Past the last control the browser may take focus to its own chrome, but never to the inert page.
			const outside = await page.evaluate(() => {
				const a = document.activeElement;
				return !!a && a !== document.body && !a.closest('dialog');
			});
			expect(outside, `focus escaped the dialog on Tab ${i + 1}`).toBe(false);
		}
	});

	test('Deploy: type a tag, Tab to the button and press Enter; the request carries the typed image', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		let body: Record<string, unknown> | undefined;
		await page.route('**/api/v1/actions/services.deploy', (r) => {
			body = r.request().postDataJSON();
			return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'audited', data: { deploy: 10 } }) });
		});
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Deploy', exact: true }).focus();
		await page.keyboard.press('Space');
		const d = dialog(page, 'Deploy shop/api');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await page.keyboard.press('Shift+Tab'); // from Cancel back to the field
		await expect(d.getByLabel('Tag or digest')).toBeFocused();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.type('registry.example.com/shop/api:2.5.0');
		await page.keyboard.press('Enter'); // the form submits
		await expect(d).toBeHidden();
		expect(body).toMatchObject({ service: 'shop/api', image_ref: 'registry.example.com/shop/api:2.5.0' });
		await expect(page.getByRole('status').filter({ hasText: /queued as deploy #10/ })).toBeVisible();
	});

	test('Delete: the whole path with the keyboard, the step-up retry carries the exact target', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		expireStepUp(server);
		const seen: { confirm: string | undefined }[] = [];
		await page.route('**/api/v1/actions/services.delete', (r) => {
			seen.push({ confirm: r.request().headers()['sd-confirm'] });
			if (seen.length === 1) return r.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ error: { code: 'mfa_required', message: 'fresh two-factor confirmation required' } }) });
			return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'audited', data: { cancelled: 0, flagged: 0 } }) });
		});
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('button', { name: 'Delete service' }).focus();
		await page.keyboard.press('Enter');
		const d = dialog(page, 'Delete shop/api?');
		await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
		await page.keyboard.press('Shift+Tab');
		await expect(d.getByLabel('Type shop/api to confirm')).toBeFocused();
		await page.keyboard.type('shop/api');
		await page.keyboard.press('Enter');
		const step = dialog(page, 'Confirm it is you');
		await expect(step).toBeVisible();
		await expect(step.getByLabel('Code from your authenticator')).toBeFocused();
		await expectNoAxeViolations(page, 'step-up prompt');
		await page.keyboard.type(nextCode(admin));
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(/\/services$/);
		await expect(page.getByRole('status').filter({ hasText: 'Delete of shop/api queued' })).toBeVisible();
		expect(seen.length).toBe(2);
		expect(seen.map((s) => s.confirm)).toEqual(['shop/api', 'shop/api']);
		await expectNoAxeViolations(page, 'services list after a delete was queued');
	});

	test('Delete: Cancel at the first dialog and at the step-up delete nothing', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		expireStepUp(server);
		const reqs = countApiRequests(page);
		await open(page, server.baseURL, 'shop/api', '/settings');
		const opener = page.getByRole('button', { name: 'Delete service' });
		await opener.click();
		await page.keyboard.press('Escape');
		await expect(page.getByRole('status').filter({ hasText: 'Cancelled. Nothing was deleted.' })).toBeVisible();
		await expect(opener).toBeFocused();
		await opener.click();
		const d = dialog(page, 'Delete shop/api?');
		await d.getByLabel('Type shop/api to confirm').fill('shop/api');
		await d.getByRole('button', { name: 'Delete service' }).click();
		const step = dialog(page, 'Confirm it is you');
		await expect(step).toBeVisible();
		await expect(step.getByRole('button', { name: 'Cancel' })).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(step).toBeHidden();
		await expect(page.getByRole('status').filter({ hasText: 'Cancelled. Nothing was deleted.' })).toBeVisible();
		expect(server.query(`SELECT 1 FROM services WHERE name = 'api'`).length).toBe(1);
		expect(server.query(`SELECT 1 FROM jobs`).length).toBe(0);
		expect(reqs.seen.filter((p) => p.endsWith('services.delete')).length).toBe(1); // the one the server refused for the missing step-up
	});

	test('a wrong target does not send the delete at all', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		const reqs = countApiRequests(page);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('button', { name: 'Delete service' }).click();
		const d = dialog(page, 'Delete shop/api?');
		for (const wrong of ['', 'shop/ap', 'Shop/api', 'shop/api2', 'api']) {
			await d.getByLabel('Type shop/api to confirm').fill(wrong);
			await d.getByRole('button', { name: 'Delete service' }).click({ force: true });
			await expect(d).toContainText('Type shop/api exactly');
			await expect(d).toBeVisible();
		}
		expect(reqs.seen.filter((p) => p.endsWith('services.delete'))).toEqual([]);
		expect(server.query(`SELECT 1 FROM services WHERE name = 'api'`).length).toBe(1);
	});
});


// ---- step 13 review fixes ---------------------------------------------------------------

test.describe('Service detail: review fixes', () => {
	test('settings compare with what is saved now: 8080 to 9090 and back to 8080 is really saved', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', containerPort: 8080, deploys: [{ number: 1, live: true }] });
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		const port = () => server.query<{ container_port: number }>(`SELECT container_port FROM services WHERE name = 'api'`)[0].container_port;
		const field = page.getByLabel('Port your app listens on');
		await field.fill('9090');
		await page.getByRole('button', { name: 'Save settings' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Saved. Deploy to apply.' })).toBeVisible();
		expect(port()).toBe(9090);
		await field.fill('8080');
		await page.getByRole('button', { name: 'Save settings' }).click();
		await expect.poll(port).toBe(8080);
	});

	test('the Roll back dialog states the strategy of the target, not the service', async ({ page, server, admin }) => {
		seedService(server, {
			project: 'shop',
			name: 'api',
			containerPort: 8080,
			deploys: [
				{ number: 1, wasLive: true, strategy: 'stop_first', stage: 'done' },
				{ number: 2, live: true, stage: 'done' }
			]
		});
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await page.locator('[data-row="1"]:visible').getByRole('button', { name: 'Roll back to #1' }).click();
		const d = dialog(page, 'Roll back shop/api to #1?');
		await expect(d).toContainText('keeps the strategy deploy #1 recorded, stop-first');
		await expect(d).not.toContainText('blue-green');
		await page.keyboard.press('Escape');
		// Redeploy uses the service's strategy (blue-green here).
		await page.getByRole('button', { name: 'Redeploy', exact: true }).click();
		await expect(dialog(page, 'Redeploy shop/api?')).toContainText('blue-green');
	});

	test('a delete that needed a fresh confirmation shows its banner and offers Delete again', async ({ page, server, admin }) => {
		seedHistory(server, 'shop', 'api', { needsAttention: 'delete_confirm_again' });
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page.getByTestId('needs-attention')).toContainText('Nothing was deleted');
		await expect(page.getByTestId('needs-attention')).toContainText('left as it was');
		await expect(page.getByTestId('needs-attention')).toContainText('were cancelled');
		await expectNoAxeViolations(page, 'service detail, delete_confirm_again');
		await page.getByRole('navigation', { name: 'Service sections' }).getByRole('link', { name: 'Settings' }).click();
		await expect(page.getByRole('button', { name: 'Delete again' })).toBeVisible();
		await page.getByRole('button', { name: 'Delete again' }).click();
		const d = dialog(page, 'Delete shop/api?');
		await expect(d.getByRole('button', { name: 'Delete service again' })).toHaveAccessibleDescription(/works once the name above matches/);
		await page.keyboard.press('Escape');
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByTestId('needs-attention')).toContainText('shop/api');
	});

	test('Escape and Cancel do nothing while a deploy request is in flight; the refusal then shows in the dialog', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/services.deploy', async (route) => {
			await new Promise((r) => setTimeout(r, 1500));
			await route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: { code: 'invalid_input', message: 'image_ref: not a valid image reference' } }) });
		});
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Deploy', exact: true }).click();
		const d = dialog(page, 'Deploy shop/api');
		await d.getByRole('button', { name: 'Deploy', exact: true }).click();
		await expect(d.getByRole('status')).toContainText('Deploy request sent, waiting for the answer');
		await expect(d.getByRole('button', { name: 'Cancel' })).toHaveAttribute('aria-disabled', 'true');
		await page.keyboard.press('Escape');
		await d.getByRole('button', { name: 'Cancel' }).click({ force: true });
		await expect(d).toBeVisible();
		await expect(d).toContainText('not a valid image reference');
		await expect(d.getByRole('button', { name: 'Cancel' })).not.toHaveAttribute('aria-disabled', 'true');
		await page.keyboard.press('Escape');
		await expect(d).toBeHidden();
	});

	test('a dialog open at browser Back is closed and acts on nothing', async ({ page, server, admin }) => {
		seedHistory(server, 'shop', 'api');
		seedService(server, { project: 'shop', name: 'worker', deploys: [{ number: 1, live: true }] });
		await signIn(page, server, admin);
		let restarts = 0;
		await page.route('**/api/v1/actions/services.restart', (r) => {
			restarts++;
			return r.continue();
		});
		await page.goto(`${server.baseURL}/services`);
		await page.locator('[data-row="shop/api"]:visible').getByRole('link', { name: 'api' }).click();
		await expect(page.getByRole('heading', { level: 1 })).toContainText('api');
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		await expect(dialog(page, 'Restart shop/api?')).toBeVisible();
		await page.goBack();
		await expect(page.getByRole('heading', { name: 'Services', level: 1 })).toBeVisible();
		await expect(page.getByRole('dialog')).toHaveCount(0);
		expect(restarts).toBe(0);
		// And forward again: the service page opens, nothing was queued.
		await page.goForward();
		await expect(page.getByRole('heading', { level: 1 })).toContainText('api');
		expect(restarts).toBe(0);
	});

	test('focus stays in the port section after Stop exposing and Try again remove their button', async ({ page, server, admin }) => {
		const base = { project: 'shop', name: 'api', public_port: 8081 };
		await mockServiceDetail(page, serviceFixture({ ...base, route_error: 'port 8081 is in use by another program' }), [deployFixture({ number: 1, active: true, was_live: true })]);
		await signIn(page, server, admin);
		const answer = (service: object) => ({ audit: 'not_audited', data: { service, applied: true } });
		await page.route('**/api/v1/actions/services.retry_port', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(answer(serviceFixture(base))) }));
		await page.route('**/api/v1/actions/services.set_public_port', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(answer(serviceFixture({ ...base, public_port: null }))) }));
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('button', { name: 'Try again' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Applied now' })).toBeVisible();
		await expect(page.getByRole('textbox', { name: 'Public port' })).toBeFocused();
		await page.getByRole('button', { name: 'Stop exposing' }).click();
		await dialog(page, 'Stop exposing shop/api?').getByRole('button', { name: 'Stop exposing' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'no longer reachable' })).toBeVisible();
		await expect(page.getByRole('textbox', { name: 'Public port' })).toBeFocused();
	});

	test('a dropped answer reports what ServerDock reads, once, in the focused alert', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/services.set_public_port', (r) => r.abort('connectionreset'));
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByRole('textbox', { name: 'Public port' }).fill('8090');
		await page.getByRole('button', { name: 'Change public port' }).click();
		await dialog(page, 'Change the public port of shop/api to 8090?').getByRole('button', { name: 'Change to port 8090' }).click();
		const alert = page.getByRole('alert').filter({ hasText: 'dropped' });
		await expect(alert).toBeFocused();
		await expect(alert).toContainText('still reads the public port as http://127.0.0.1:8081');
		await expect(alert).not.toContainText('below');
		await expect(page.getByText('still reads the public port')).toHaveCount(1);
		await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible();
	});

	test('sub-pages: titles, the section nav, focus on the section heading, #port, and no reload between them', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api');
		await expect(page).toHaveTitle('Deploys · api · shop · ServerDock');
		const nav = page.getByRole('navigation', { name: 'Service sections' });
		const reqs = countApiRequests(page);
		await nav.getByRole('link', { name: 'Settings' }).click();
		await expect(page).toHaveTitle('Settings · api · shop · ServerDock');
		await expect(page.getByRole('heading', { name: 'Service settings', level: 2 })).toBeFocused();
		expect(reqs.seen.filter((p) => /services\.get|deploys\.list/.test(p))).toEqual([]);
		await nav.getByRole('link', { name: 'Deploys' }).click();
		await expect(page.getByRole('heading', { name: 'Deploys', level: 2 })).toBeFocused();
		expect(reqs.seen.filter((p) => /services\.get|deploys\.list/.test(p))).toEqual([]);
		// The route error's link lands on the port section.
		await page.goto(`${server.baseURL}/services/shop/api/settings#port`);
		await expect(page.getByRole('heading', { name: 'Public port', level: 2 })).toBeFocused();
	});

	test('a failed Refresh keeps the Settings forms and what was typed, and says the status is unknown', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		seedHistory(server);
		await signIn(page, server, admin);
		await open(page, server.baseURL, 'shop/api', '/settings');
		await page.getByLabel('Drain time (seconds)').fill('42');
		await page.route('**/api/v1/actions/services.get', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"boom"}}' }));
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.getByTestId('status-unknown')).toBeVisible();
		await expect(page.getByTestId('card-running')).toHaveCount(0);
		await expect(page.getByLabel('Drain time (seconds)')).toHaveValue('42');
		await expectNoAxeViolations(page, 'settings, refresh failed');
	});

	test('while one action runs another says why nothing happened', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/services.redeploy', async (route) => {
			await new Promise((r) => setTimeout(r, 1500));
			await route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ error: { code: 'conflict', message: 'not now' } }) });
		});
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Redeploy', exact: true }).click();
		await dialog(page, 'Redeploy shop/api?').getByRole('button', { name: 'Redeploy', exact: true }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Redeploying…' })).toBeVisible();
		await page.getByRole('button', { name: 'Restart', exact: true }).click({ force: true });
		await expect(page.getByRole('status').filter({ hasText: 'still running' })).toBeVisible();
		await expect(page.getByRole('dialog')).toHaveCount(0);
	});
});
