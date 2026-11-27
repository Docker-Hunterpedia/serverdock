import type { Page } from '@playwright/test';
import { callAction, countApiRequests, expect, expectNoAxeViolations, expireStepUp, letTheAccountAge, nextCode, signIn, test, waitForDeploy } from './fixtures';
import { deployFixture, mockActionError, mockServiceDetail, seedHistory, seedService, serviceFixture } from './seed';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer, uniqueProject } from './server';

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
		const nav = page.getByRole('navigation', { name: 'Service' });
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
			await expect(page.getByRole('navigation', { name: 'Service' })).toBeVisible();
			const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
			expect(over, `horizontal overflow on ${sub || 'Deploys'}`).toBeLessThanOrEqual(0);
			const small = await page.evaluate(() =>
				[...document.querySelectorAll<HTMLElement>('main button, main a[href], nav[aria-label="Service"] a')]
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

	test('a refused restart (versions_kept) says to try again once the service has settled', async ({ page, server, admin }) => {
		seedHistory(server);
		await signIn(page, server, admin);
		await mockActionError(page, 'services.restart', 409, 'conflict', 'two versions of this service may be running after an unfinished stop-first deploy; not now');
		await open(page, server.baseURL, 'shop/api');
		await page.getByRole('button', { name: 'Restart', exact: true }).click();
		await dialog(page, 'Restart shop/api?').getByRole('button', { name: 'Restart now' }).click();
		await expect(page.getByRole('alert')).toContainText('Try again once the service has settled.');
		await expectNoAxeViolations(page, 'restart refused');
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
		await expect(page.getByRole('navigation', { name: 'Service' }).getByRole('link', { name: 'Settings' })).toHaveAttribute('aria-current', 'page');
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

test.describe('Service detail with real Docker', () => {
	test('redeploy, roll back, restart, public port and delete work end to end', async ({ page, allow5xx }) => {
		test.setTimeout(480_000);
		allow5xx();
		const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
		try {
			const admin = await server.firstRun();
			await letTheAccountAge();
			await signIn(page, server, admin);
			const project = uniqueProject();
			const ref = `${project}/web`;
			await callAction(page, server, 'services.create', { project, name: 'web', image_ref: TEST_APP_IMAGE, container_port: 8080, deploy: true, drain_s: 10, stop_grace_s: 2 });
			await waitForDeploy(server, ref, 1);
			await open(page, server.baseURL, ref);
			await expect(page.getByRole('heading', { level: 1 })).toContainText('Running');
			await expect(page.locator('[data-row="1"]:visible')).toContainText('● Live');
			await expect(page.locator('[data-row="1"]:visible').getByRole('button', { name: /Roll back/ })).toHaveCount(0);

			// Redeploy: a new deploy of the same image; #1 becomes Replaced and can be rolled back to.
			await page.getByRole('button', { name: 'Redeploy', exact: true }).click();
			await dialog(page, `Redeploy ${ref}?`).getByRole('button', { name: 'Redeploy', exact: true }).click();
			await expect(page.getByRole('status').filter({ hasText: 'Redeploy queued as deploy #2' })).toBeVisible();
			await waitForDeploy(server, ref, 2);
			await page.getByRole('button', { name: 'Refresh' }).click();
			await expect(page.locator('[data-row="2"]:visible')).toContainText('● Live');
			await expect(page.locator('[data-row="1"]:visible')).toContainText('○ Replaced');

			// Roll back to #1: a new rollback deploy.
			await page.locator('[data-row="1"]:visible').getByRole('button', { name: 'Roll back to #1' }).click();
			await dialog(page, `Roll back ${ref} to #1?`).getByRole('button', { name: 'Roll back to #1' }).click();
			await expect(page.getByRole('status').filter({ hasText: 'Rollback to #1 queued as deploy #3' })).toBeVisible();
			await waitForDeploy(server, ref, 3);
			await page.getByRole('button', { name: 'Refresh' }).click();
			await expect(page.locator('[data-row="3"]:visible')).toContainText('● Live');
			await expect(page.locator('[data-row="3"]:visible')).toContainText('(Rollback)');

			// Restart: queued, then done.
			await page.getByRole('button', { name: 'Restart', exact: true }).click();
			await dialog(page, `Restart ${ref}?`).getByRole('button', { name: 'Restart now' }).click();
			await expect(page.getByRole('status').filter({ hasText: 'Restart queued' })).toBeVisible();
			await expect.poll(() => server.query<{ state: string }>(`SELECT state FROM jobs WHERE kind = 'restart'`)[0]?.state, { timeout: 60_000 }).toBe('succeeded');

			// Public port: Cancel changes nothing; Confirm applies at once and the app answers.
			const port = 30000 + Math.floor(Math.random() * 20000);
			await page.getByRole('navigation', { name: 'Service' }).getByRole('link', { name: 'Settings' }).click();
			await page.getByRole('textbox', { name: 'Public port' }).fill(String(port));
			await page.getByRole('button', { name: 'Expose on this port' }).click();
			await dialog(page, `Expose ${ref} on port ${port}?`).getByRole('button', { name: 'Cancel' }).click();
			expect(server.query<{ public_port: number | null }>(`SELECT public_port FROM services WHERE name = 'web'`)[0].public_port).toBeNull();
			await page.getByRole('button', { name: 'Expose on this port' }).click();
			await dialog(page, `Expose ${ref} on port ${port}?`).getByRole('button', { name: `Expose on port ${port}` }).click();
			// The proxy that serves the panel reloads for a port change, so the answer may be lost (WebKit does): the page then reads the saved state back.
			await expect(page.getByRole('status').filter({ hasText: 'Applied now' }).or(page.getByTestId('port-notice')).first()).toBeVisible({ timeout: 30_000 });
			expect(server.query<{ public_port: number | null }>(`SELECT public_port FROM services WHERE name = 'web'`)[0].public_port).toBe(port);
			await expect.poll(async () => (await page.request.get(`http://127.0.0.1:${port}/`)).status(), { timeout: 30_000 }).toBeLessThan(500);
			await page.getByRole('button', { name: 'Stop exposing' }).click();
			await dialog(page, `Stop exposing ${ref}?`).getByRole('button', { name: 'Stop exposing' }).click();
			await expect(page.getByRole('status').filter({ hasText: 'no longer reachable' }).or(page.getByTestId('port-notice')).first()).toBeVisible({ timeout: 30_000 });
			expect(server.query<{ public_port: number | null }>(`SELECT public_port FROM services WHERE name = 'web'`)[0].public_port).toBeNull();

			// Delete: the exact target, then a fresh code; the Services list says it was queued.
			expireStepUp(server);
			await page.getByRole('button', { name: 'Delete service' }).click();
			const d = dialog(page, `Delete ${ref}?`);
			await d.getByLabel(`Type ${ref} to confirm`).fill(ref);
			await d.getByRole('button', { name: 'Delete service' }).click();
			const step = dialog(page, 'Confirm it is you');
			await step.getByLabel('Code from your authenticator').fill(nextCode(admin));
			await step.getByRole('button', { name: 'Confirm' }).click();
			await expect(page).toHaveURL(/\/services$/);
			await expect(page.getByRole('status').filter({ hasText: `Delete of ${ref} queued` })).toBeVisible();
			await expect.poll(() => server.query(`SELECT 1 FROM services WHERE name = 'web'`).length, { timeout: 90_000 }).toBe(0);
			await expect(async () => {
				await page.getByRole('button', { name: 'Refresh' }).click();
				await expect(page.locator(`[data-row="${ref}"]`)).toHaveCount(0, { timeout: 2000 });
			}).toPass({ timeout: 30_000 });
		} finally {
			await server.stop();
		}
	});
});
