import type { Page } from '@playwright/test';
import { callAction, expect, expectNoAxeViolations, expireStepUp, letTheAccountAge, nextCode, signIn, test, waitForDeploy } from './fixtures';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer, uniqueProject } from './server';

// Plan 0004 step 13: the flows that deploy for real (rollback, restart, public port,
// delete). They run in the docker-* projects, one worker at a time: a Docker network or
// container appearing changes the host's network interfaces, and Chromium aborts
// in-flight requests of every other test when that happens (net::ERR_NETWORK_CHANGED).
test.use({ storageState: { cookies: [], origins: [] } });

const dialog = (page: Page, name: string | RegExp) => page.getByRole('dialog', { name });

/** Opens a service's page; `sub` is "" (Deploys) or "/settings". */
async function open(page: Page, baseURL: string, ref: string, sub = '') {
	await page.goto(`${baseURL}/services/${ref}${sub}`);
	await expect(page.getByRole('heading', { level: 1 })).toContainText(ref.split('/')[1]);
}

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
			await page.getByRole('navigation', { name: 'Service sections' }).getByRole('link', { name: 'Settings' }).click();
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

// ---- real Docker ----------------------------------------------------------------

type Svc = { public_port: number | null; state: string; active_deploy: number | null };

/** Starts a Docker server, signs the page in and deploys the test app as <project>/web (#1, no public port). */
async function realService(page: Page, container_port = 8080) {
	const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
	const admin = await server.firstRun();
	await letTheAccountAge();
	await signIn(page, server, admin);
	const project = uniqueProject();
	const ref = `${project}/web`;
	await callAction(page, server, 'services.create', { project, name: 'web', image_ref: TEST_APP_IMAGE, container_port, deploy: true, drain_s: 10, stop_grace_s: 2 });
	await waitForDeploy(server, ref, 1);
	return { server, admin, project, ref };
}

/** The HTTP status of the app's public port, or "refused". */
async function probe(page: Page, port: number): Promise<number | 'refused'> {
	try {
		return (await page.request.get(`http://127.0.0.1:${port}/`, { timeout: 5000, failOnStatusCode: false })).status();
	} catch {
		return 'refused';
	}
}
test.describe('Service detail with real Docker', () => {
	test('the public port takes effect only once the dialog is confirmed; Cancel and Escape change nothing', async ({ page, allow5xx }) => {
		test.setTimeout(300_000);
		allow5xx();
		const { server, ref } = await realService(page);
		try {
			const port = 30000 + Math.floor(Math.random() * 20000);
			const svc = () => callAction<Svc>(page, server, 'services.get', { service: ref });
			expect((await svc()).public_port).toBeNull();
			await open(page, server.baseURL, ref, '/settings');
			const field = page.getByRole('textbox', { name: 'Public port' });
			await field.fill(String(port));
			const opener = page.getByRole('button', { name: 'Expose on this port' });
			const d = dialog(page, `Expose ${ref} on port ${port}?`);

			// Open, look, and back out two ways: nothing happens on the server or on the host.
			for (const how of ['Escape', 'Cancel']) {
				await opener.click();
				await expect(d).toContainText(`Applied now: exposes the app on the internet at http://127.0.0.1:${port}`);
				await expect(d.getByRole('button', { name: 'Cancel' })).toBeFocused();
				// While the dialog is open nothing has been applied yet.
				expect((await svc()).public_port).toBeNull();
				expect(await probe(page, port)).toBe('refused');
				if (how === 'Escape') {
					await expectNoAxeViolations(page, 'public port: expose dialog');
					await page.keyboard.press('Escape');
				} else await d.getByRole('button', { name: 'Cancel' }).click();
				await expect(d).toBeHidden();
				await expect(opener).toBeFocused();
				await expect(page.getByRole('status').filter({ hasText: 'Cancelled. The public port was not changed.' })).toBeVisible();
				expect((await svc()).public_port).toBeNull();
				expect(await probe(page, port)).toBe('refused');
				expect(server.query(`SELECT 1 FROM jobs WHERE kind != 'deploy'`).length).toBe(0);
			}

			// Confirm: the change is applied at once and the app answers on the port.
			await opener.click();
			await d.getByRole('button', { name: `Expose on port ${port}` }).click();
			await expect(page.getByRole('status').filter({ hasText: 'Applied now' }).or(page.getByTestId('port-notice')).first()).toBeVisible({ timeout: 30_000 });
			expect((await svc()).public_port).toBe(port);
			await expect.poll(() => probe(page, port), { timeout: 30_000 }).toBe(200);
			await expectNoAxeViolations(page, 'public port applied');

			// Clearing: Cancel leaves the app served; Confirm stops serving it.
			const stop = page.getByRole('button', { name: 'Stop exposing' });
			await stop.click();
			const c = dialog(page, `Stop exposing ${ref}?`);
			await expect(c).toContainText(`no longer reachable at http://127.0.0.1:${port}`);
			await expectNoAxeViolations(page, 'public port: stop exposing dialog');
			await page.keyboard.press('Escape');
			await expect(stop).toBeFocused();
			expect((await svc()).public_port).toBe(port);
			expect(await probe(page, port)).toBe(200);
			await stop.click();
			await c.getByRole('button', { name: 'Stop exposing' }).click();
			await expect(page.getByRole('status').filter({ hasText: 'no longer reachable' }).or(page.getByTestId('port-notice')).first()).toBeVisible({ timeout: 30_000 });
			expect((await svc()).public_port).toBeNull();
			await expect.poll(() => probe(page, port), { timeout: 30_000 }).toBe('refused');
		} finally {
			await server.stop();
		}
	});

	test('roll back end to end: offered only on once-live deploys, and the app serves again afterwards', async ({ page, allow5xx }) => {
		test.setTimeout(420_000);
		allow5xx();
		const { server, ref } = await realService(page);
		try {
			const port = 30000 + Math.floor(Math.random() * 20000);
			const svc = () => callAction<Svc>(page, server, 'services.get', { service: ref });
			await callAction(page, server, 'services.set_public_port', { service: ref, public_port: port }).catch(() => undefined);
			await expect.poll(() => probe(page, port), { timeout: 30_000 }).toBe(200);
			const row = (n: number) => page.locator(`[data-row="${n}"]:visible`);
			const containers = () => server.query<{ n: number }>(`SELECT COUNT(*) AS n FROM deploys WHERE status = 'succeeded'`)[0].n;

			// #2 replaces #1; #3 fails at the pull (no such tag) and leaves #2 live.
			await callAction(page, server, 'services.redeploy', { service: ref });
			await waitForDeploy(server, ref, 2);
			await callAction(page, server, 'services.deploy', { service: ref, image_ref: '127.0.0.1:5000/sd-testapp:no-such-tag' });
			await waitForDeploy(server, ref, 3, 'failed', 240_000);
			expect(containers()).toBe(2);
			expect((await svc()).active_deploy).toBe(2);

			await open(page, server.baseURL, ref);
			await expect(page.getByRole('heading', { level: 1 })).toContainText('Running');
			await expect(row(1)).toContainText('○ Replaced');
			await expect(row(2)).toContainText('● Live');
			await expect(row(3)).toContainText('▲ Failed');
			await expectNoAxeViolations(page, 'real service with history (live, replaced, failed)');
			// Offered only where a deploy was once live and is not live now.
			await expect(row(1).getByRole('button', { name: 'Roll back to #1' })).toBeVisible();
			for (const n of [2, 3]) await expect(row(n).getByRole('button', { name: /Roll back/ })).toHaveCount(0);

			// Cancel first: nothing is queued.
			await row(1).getByRole('button', { name: 'Roll back to #1' }).click();
			const d = dialog(page, `Roll back ${ref} to #1?`);
			await expect(d).toContainText(TEST_APP_IMAGE);
			await d.getByRole('button', { name: 'Cancel' }).click();
			expect(server.query(`SELECT 1 FROM deploys`).length).toBe(3);

			// Then do it: a new rollback deploy (#4) goes live, #2 becomes Replaced, the app answers.
			await row(1).getByRole('button', { name: 'Roll back to #1' }).click();
			await d.getByRole('button', { name: 'Roll back to #1' }).click();
			await expect(page.getByRole('status').filter({ hasText: 'Rollback to #1 queued as deploy #4' })).toBeVisible();
			await waitForDeploy(server, ref, 4);
			await page.getByRole('button', { name: 'Refresh' }).click();
			await expect(row(4)).toContainText('● Live');
			await expect(row(4)).toContainText('(Rollback)');
			await expect(row(2)).toContainText('○ Replaced');
			// The rolled-back deploy itself can be a target later (it was live once it is replaced).
			await expect(row(2).getByRole('button', { name: 'Roll back to #2' })).toBeVisible();
			await expect(row(4).getByRole('button', { name: /Roll back/ })).toHaveCount(0);
			expect((await svc()).active_deploy).toBe(4);
			expect(server.query<{ kind: string }>(`SELECT kind FROM deploys WHERE number = 4`)[0].kind).toBe('rollback');
			// The state comes from Docker's events and the route from the proxy: both may trail the "succeeded" by a moment.
			await expect.poll(async () => (await svc()).state, { timeout: 20_000 }).toBe('running');
			await expect.poll(() => probe(page, port), { timeout: 20_000 }).toBe(200);
			await expectNoAxeViolations(page, 'real service after a rollback');
		} finally {
			await server.stop();
		}
	});

	test('delete needs the exact target and a good code, then lands on Services with a status message', async ({ page, allow5xx }) => {
		test.setTimeout(300_000);
		allow5xx();
		const { server, admin, ref } = await realService(page);
		try {
			await open(page, server.baseURL, ref, '/settings');
			expireStepUp(server);
			await page.getByRole('button', { name: 'Delete service' }).click();
			const d = dialog(page, `Delete ${ref}?`);
			await d.getByLabel(`Type ${ref} to confirm`).fill(ref.split('/')[1]); // the name alone is not the target
			await d.getByRole('button', { name: 'Delete service' }).click({ force: true });
			await expect(d).toContainText(`Type ${ref} exactly`);
			await d.getByLabel(`Type ${ref} to confirm`).fill(ref);
			await d.getByRole('button', { name: 'Delete service' }).click();
			const step = dialog(page, 'Confirm it is you');
			await expect(step).toBeVisible();
			// A wrong code keeps the prompt and deletes nothing.
			await step.getByLabel('Code from your authenticator').fill('000000');
			await step.getByRole('button', { name: 'Confirm' }).click();
			await expect(step).toBeVisible();
			await expect(step.getByRole('alert').or(step.getByRole('status')).filter({ hasText: /./ }).first()).toBeVisible();
			expect(server.query(`SELECT 1 FROM services WHERE name = 'web'`).length).toBe(1);
			expect(server.query(`SELECT 1 FROM jobs WHERE kind = 'delete'`).length).toBe(0);
			await step.getByLabel('Code from your authenticator').fill(nextCode(admin));
			await step.getByRole('button', { name: 'Confirm' }).click();
			await expect(page).toHaveURL(/\/services$/);
			await expect(page.getByRole('status').filter({ hasText: `Delete of ${ref} queued` })).toBeVisible();
			await expectNoAxeViolations(page, 'services list, delete queued');
			await expect.poll(() => server.query(`SELECT 1 FROM services WHERE name = 'web'`).length, { timeout: 90_000 }).toBe(0);
		} finally {
			await server.stop();
		}
	});
});
