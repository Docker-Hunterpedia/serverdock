import type { Page } from '@playwright/test';
import { expect, expectNoAxeViolations, signIn, test } from './fixtures';
import { deployGetFixture, deployStates, mockAction, mockDeployGet, mockDeployLog, logText, seedService } from './seed';

// Plan 0004 step 14, review round 1: honest app safety for cancelled and superseded
// deploys, the superseded wording, focus when a control disappears, "Status unknown"
// keeping the last known state, Check again, the Redeploy refresh and moving to another
// deploy in place (Revision 18). Running states are faked; see deploy-detail-live.spec.ts.
test.use({ storageState: { cookies: [], origins: [] } });

const url = (base: string, n = 3) => `${base}/services/shop/api/deploys/${n}`;
const seed = (server: Parameters<typeof seedService>[0]) => seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });

async function freeze(page: Page) {
	const t0 = new Date('2026-11-27T10:00:00Z');
	await page.clock.install({ time: t0 });
	await page.clock.pauseAt(new Date(t0.getTime() + 1));
}
async function pollOnce(page: Page, calls: number[], ms = 2000) {
	const before = calls.length;
	await page.clock.runFor(ms);
	await expect.poll(() => calls.length).toBeGreaterThan(before);
	await page.waitForTimeout(150);
}
/** Records every distinct non-empty text the status and alert regions held. */
async function record(page: Page) {
	await page.addInitScript(() => {
		const said: { status: string[]; alert: string[] } = { status: [], alert: [] };
		const last = { status: '', alert: '' };
		(window as unknown as { __said: typeof said }).__said = said;
		new MutationObserver(() => {
			for (const k of ['status', 'alert'] as const) {
				const text = (document.querySelector(`main [role=${k}], [role=${k}]`)?.textContent ?? '').trim();
				if (text && text !== last[k]) said[k].push(text);
				last[k] = text;
			}
		}).observe(document, { subtree: true, childList: true, characterData: true });
	});
}
const said = (page: Page) => page.evaluate(() => (window as unknown as { __said: { status: string[]; alert: string[] } }).__said);

test.describe('app safety of cancelled and superseded deploys is the server\'s verdict', () => {
	test('a cancelled first deploy with app_safe "unknown" never says the previous version kept serving', async ({ page, server, admin }) => {
		seed(server);
		await mockAction(page, 'deploys.get', deployStates.cancelled());
		await mockDeployLog(page, { message: 'No log' });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		const line = page.getByTestId('app-safety');
		await expect(line).toContainText('unknown');
		await expect(line).toHaveText(/^◌ unknown: /);
		await expect(line).not.toContainText('safe,');
		await expect(line).not.toContainText('not changed');
	});

	test('a deploy that was running when it was superseded says "Stopped and replaced", and one replaced in the queue says so', async ({ page, server, admin }) => {
		seed(server);
		await mockAction(page, 'deploys.get', deployStates.supersededStarted());
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('deploy-reason')).toHaveText('Stopped and replaced by #4.');
		await expect(page.getByTestId('app-safety')).not.toContainText('not changed');
	});

	test('a cancel that leaves the app not safe is announced through the alert, a plain one through the status', async ({ page, server, admin }) => {
		seed(server);
		await record(page);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.pulling() : deployStates.cancelledNotSafe()));
		await mockDeployLog(page, { message: 'No log' });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await pollOnce(page, calls);
		const a = await said(page);
		expect(a.alert).toHaveLength(1);
		expect(a.alert[0]).toMatch(/Deploy #3 was cancelled\. Your app is not safe: /);
		expect(a.status).toEqual([]);
	});

	test('a superseded deploy that had started is announced as stopped and replaced', async ({ page, server, admin }) => {
		seed(server);
		await record(page);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.pulling() : deployStates.supersededStarted()));
		await mockDeployLog(page, { message: 'No log' });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await pollOnce(page, calls);
		expect((await said(page)).status).toEqual(['Deploy #3: stopped and replaced by #4.']);
	});
});

test.describe('focus and "Status unknown"', () => {
	test('Cancel has focus when the poll sees the deploy finish: focus goes to the heading, not to the page', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : deployStates.succeeded()));
		await mockDeployLog(page, { text: logText(['up'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await page.getByTestId('cancel-deploy').focus();
		await pollOnce(page, calls);
		await expect(page.getByTestId('cancel-deploy')).toHaveCount(0);
		await expect(page.getByRole('heading', { level: 1 })).toBeFocused();
	});

	test('focus elsewhere does not move when the deploy finishes', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : deployStates.succeeded()));
		await mockDeployLog(page, { text: logText(['up'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await page.getByRole('main').getByRole('link', { name: 'Services', exact: true }).first().focus();
		await pollOnce(page, calls);
		await expect(page.getByRole('main').getByRole('link', { name: 'Services', exact: true }).first()).toBeFocused();
	});

	test('"Try again" for a busy log has focus: it goes to the heading when the log loads', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		seed(server);
		await mockAction(page, 'deploys.get', deployStates.failedAppSafe());
		let n = 0;
		await page.route('**/api/v1/actions/deploys.log', (route) =>
			n++ === 0
				? route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unavailable', message: 'deploy logs are being read right now; try again in a moment' } }) })
				: route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: { service: 'shop/api', number: 3, kept: true, text: logText(['hello'], { number: 3 }) } }) })
		);
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('log-message')).toContainText('try again in a moment');
		await page.getByTestId('log-retry').click();
		await expect(page.getByTestId('log-tail')).toContainText('hello');
		await expect(page.getByRole('heading', { level: 1 })).toBeFocused();
	});

	test('"Status unknown" keeps the last known state, disables the actions, and is announced once in and once out', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		allow5xx();
		await record(page);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 1 ? { status: 502 } : deployStates.healthGate()));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await pollOnce(page, calls); // the 502
		await expect(page.getByTestId('status-unknown')).toContainText('What is shown below may be out of date');
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await expect(page.getByTestId('cancel-deploy')).toHaveAttribute('aria-disabled', 'true');
		await expect(page.getByTestId('cancel-deploy')).toHaveAccessibleDescription(/status is unknown/);
		await pollOnce(page, calls, 2000); // back off 2 s, then an answer
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		const a = await said(page);
		expect(a.status).toEqual(['Status unknown: could not reach the server.', 'Status is current again.']);
		expect(a.alert).toEqual([]);
	});

	test('the page with the last state kept and the unknown note has no axe violations', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		allow5xx();
		await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : { status: 502 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await expect(page.getByTestId('status-unknown')).toBeVisible({ timeout: 10_000 });
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await expectNoAxeViolations(page, 'deploy page: unknown, last state kept');
	});

	test('Check again says Checking, then Still could not reach the server, then Status updated with focus on the heading', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		allow5xx();
		await record(page);
		let up = false;
		await mockDeployGet(page, () => (up ? deployStates.healthGate() : { status: 502 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('status-unknown')).toBeVisible();
		await page.getByTestId('check-again').click();
		await expect(page.getByRole('status')).toHaveText('Still could not reach the server.');
		up = true;
		await page.getByTestId('check-again').click();
		await expect(page.getByRole('status')).toHaveText('Status updated.');
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		await expect(page.getByRole('heading', { level: 1 })).toBeFocused();
		const a = await said(page);
		expect(a.status).toContain('Checking…');
		expect(a.status.filter((t) => t === 'Status unknown: could not reach the server.')).toHaveLength(1);
	});

	test('polling backs off after consecutive failures: 2, 4, 8 seconds', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		allow5xx();
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : { status: 502 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await pollOnce(page, calls, 2000); // failure 1 -> next in 2 s
		await pollOnce(page, calls, 2000); // failure 2 -> next in 4 s
		const n = calls.length;
		await page.clock.runFor(3500);
		await page.waitForTimeout(200);
		expect(calls.length, 'not yet at 3.5 s').toBe(n);
		await pollOnce(page, calls, 500); // failure 3 at 4 s -> next in 8 s
		const m = calls.length;
		await page.clock.runFor(7500);
		await page.waitForTimeout(200);
		expect(calls.length, 'not yet at 7.5 s').toBe(m);
		await pollOnce(page, calls, 600);
	});

	test('a 403 stops polling and shows the server\'s message, not "could not reach the server"', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		let n = 0;
		await page.route('**/api/v1/actions/deploys.get', (route) => {
			n++;
			return route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ error: { code: 'forbidden', message: 'this token cannot read deploys' } }) });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('status-refused')).toContainText('this token cannot read deploys');
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		const before = n;
		await page.clock.runFor(60_000);
		await page.waitForTimeout(300);
		expect(n, 'no polling after a 403').toBe(before);
	});

	test('an answer that is not JSON is "unknown", and polling goes on', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		let n = 0;
		await page.route('**/api/v1/actions/deploys.get', (route) => {
			n++;
			return route.fulfill({ status: 200, contentType: 'application/json', body: n === 1 ? 'not json' : JSON.stringify({ audit: 'not_audited', data: deployStates.healthGate() }) });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('status-unknown')).toBeVisible();
		await page.clock.runFor(2500);
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
	});
});

test.describe('Redeploy refreshes first and names the live version', () => {
	test('the dialog names the live deploy and image after a fresh answer', async ({ page, server, admin }) => {
		seed(server);
		let asked = 0;
		await page.route('**/api/v1/actions/deploys.get', (route) => {
			asked++;
			return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: deployStates.succeeded() }) });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('redeploy')).toBeVisible();
		const before = asked;
		await page.getByTestId('redeploy').click();
		const dialog = page.getByRole('dialog', { name: 'Redeploy shop/api?' });
		await expect(dialog).toContainText('Redeploys the live version of shop/api: deploy #3 (registry.example.com/shop/api:2.4.1)');
		expect(asked, 'it asked again before the dialog').toBeGreaterThan(before);
		await dialog.getByRole('button', { name: 'Cancel' }).click();
	});

	test('when the deploy is no longer live by then, nothing is offered and nothing starts', async ({ page, server, admin }) => {
		seed(server);
		let n = 0;
		await page.route('**/api/v1/actions/deploys.get', (route) => {
			const d = deployStates.succeeded();
			const data = n++ === 0 ? d : { ...d, deploy: { ...d.deploy, active: false } };
			return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data }) });
		});
		let queued = 0;
		await page.route('**/api/v1/actions/services.redeploy', (route) => {
			queued++;
			return route.abort();
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await page.getByTestId('redeploy').click();
		await expect(page.getByRole('alert')).toContainText('no longer the live version');
		await expect(page.getByRole('dialog')).toHaveCount(0);
		expect(queued).toBe(0);
	});
});

test.describe('moving to another deploy in place (Revision 18)', () => {
	test('the notice, the warnings, the announcements and the log of the first deploy do not follow', async ({ page, server, admin }) => {
		test.setTimeout(90_000); // a create on a server without Docker waits about 20 s for the proxy's route
		// A server without Docker: a create with a public port is "not applied", with the SEC6 warning.
		await record(page);
		await page.route('**/api/v1/actions/deploys.get', async (route) => {
			const number = JSON.parse(route.request().postData() ?? '{}').number as number;
			const data = number === 1 ? deployGetFixture({ deploy: { number: 1, status: 'failed', stage: 'health', reason_code: 'health_timeout', reason: 'first deploy failed', app_safe: 'unknown', log_kept: true, finished_at: '2026-11-20T14:02:00.000Z' } }) : deployStates.pulling();
			await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: { ...data, service: 'default/web', deploy: { ...data.deploy, number } } }) });
		});
		await page.route('**/api/v1/actions/deploys.log', (route) => {
			const n = JSON.parse(route.request().postData() ?? '{}').number as number;
			return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: { service: 'default/web', number: n, kept: true, text: logText([`output of deploy ${n}`], { number: n }) } }) });
		});
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('nginx:1.27');
		await page.getByLabel('Port your app listens on (optional)').fill('80');
		await page.getByLabel('Public port (optional)').fill('18090');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(/\/services\/default\/web\/deploys\/1$/, { timeout: 60_000 });
		await expect(page.getByTestId('not-applied')).toBeVisible();
		await expect(page.getByText('This deploy was just queued')).toBeVisible();
		await expect(page.getByTestId('log-tail')).toContainText('output of deploy 1');
		await expect(page.getByRole('alert')).toHaveText('');
		// Another deploy of the same service, reached by a client-side link: the same page, new parameters.
		await page.evaluate(() => {
			const a = document.createElement('a');
			a.href = '/services/default/web/deploys/2';
			a.id = 'to-2';
			a.textContent = 'to 2';
			document.querySelector('main')!.append(a);
		});
		await page.locator('#to-2').click();
		await expect(page).toHaveURL(/deploys\/2$/);
		await expect(page.getByRole('heading', { level: 1 })).toContainText('Deploy #2');
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await expect(page.getByTestId('not-applied')).toHaveCount(0);
		await expect(page.getByText('This deploy was just queued')).toHaveCount(0);
		await expect(page.locator('main .banner')).toHaveCount(0);
		await expect(page.getByTestId('log-tail')).toHaveCount(0);
		await expect(page.getByRole('alert')).toHaveText('');
		await expect(page.getByTestId('deploy-reason')).toHaveCount(0);
		expect((await said(page)).alert, 'nothing of deploy 1 is announced for deploy 2').toEqual([]);
		// ...and back: the notice is never shown a second time.
		await page.goBack();
		await expect(page.getByRole('heading', { level: 1 })).toContainText('Deploy #1');
		await expect(page.getByTestId('not-applied')).toHaveCount(0);
		await expect(page.getByTestId('log-tail')).toContainText('output of deploy 1');
	});
});
