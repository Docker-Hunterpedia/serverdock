import { expect, expectNoAxeViolations, signIn, test } from './fixtures';
import { deployGetFixture, deployStates, logText, mockAction, mockDeployGet, mockDeployLog, seedDeployLog, seedService, stageRows } from './seed';

// Plan 0004 step 14: the Deploy detail page. The implementer's smoke tests; the tester
// extends them to every screen state in all 8 projects. Running states are faked with
// deploys.get answers (a running deploy needs a real Docker run: see the docker spec).
test.use({ storageState: { cookies: [], origins: [] } });

const url = (base: string, n = 3) => `${base}/services/shop/api/deploys/${n}`;

test.describe('Deploy detail: screen states', () => {
	const cases: [keyof typeof deployStates, string, RegExp[]][] = [
		['queued', 'Queued', [/No stage has started/]],
		['pulling', 'Running', [/Pull the image: … Running/, /12\.4? ?MiB|11\.4 MiB/]],
		['waitingToRetry', 'Running', [/waiting to retry/i]],
		['healthGate', 'Running', [/Health check: … Running/]],
		['succeeded', 'Live', [/the new version is live/]],
		['failedAppSafe', 'Failed', [/safe, the previous version kept serving/]],
		['failedNotSafe', 'Failed', [/not safe/]],
		['interrupted', 'Interrupted', [/ServerDock stopped while this deploy was running/]],
		['superseded', 'Superseded', [/Replaced in the queue by #4 before it started/]],
		['cancelled', 'Cancelled', [/It was cancelled/]],
		['diskFull', 'Failed', [/disk is full/]]
	];
	for (const [state, word, texts] of cases) {
		test(`shows ${state} with no axe violations`, async ({ page, server, admin }) => {
			seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });
			await mockAction(page, 'deploys.get', deployStates[state]());
			await mockDeployLog(page, { message: 'No log yet: a deploy\'s log is kept when its container is removed' });
			await signIn(page, server, admin);
			await page.goto(url(server.baseURL));
			await expect(page.getByTestId('deploy-status')).toContainText(word);
			for (const t of texts) await expect(page.getByRole('main')).toContainText(t);
			await expectNoAxeViolations(page, `deploy page: ${state}`);
		});
	}
});

test.describe('Deploy detail: live updates', () => {
	test('announces each stage change once, nothing for progress ticks, and the failure once through the alert', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });
		const progress = (bytes: number) => ({ layers: 5, layers_done: 2, bytes, bytes_total: 48_000_000, status: 'Downloading' });
		const { calls } = await mockDeployGet(page, (n) => {
			if (n <= 1) return deployGetFixture({ progress: progress(10_000_000 * (n + 1)) });
			if (n === 2) return deployStates.healthGate();
			return deployStates.failedAppSafe();
		});
		await mockDeployLog(page, { text: logText(['listening on :8080']) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		const status = page.getByRole('status');
		const alert = page.getByRole('alert');
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await expect(status).toHaveText('');
		await expect(status).toHaveText('Deploy #3: health check started.', { timeout: 10_000 });
		await expect(alert).toContainText('Deploy #3 failed:', { timeout: 10_000 });
		await expect(status).toHaveText('');
		const n = calls.length;
		await page.waitForTimeout(5000);
		expect(calls.length, 'polling stopped once the deploy had finished').toBe(n);
	});

	test('shows "Status unknown" when polling fails and recovers on the next answer', async ({ page, server, admin, allow5xx }) => {
		seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });
		await mockDeployGet(page, (n) => (n === 1 ? { status: 502 } : deployStates.pulling()));
		await signIn(page, server, admin);
		allow5xx();
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('deploy-status')).toContainText('Running');
		await expect(page.getByTestId('status-unknown')).toContainText('Status unknown: could not reach the server', { timeout: 6000 });
		await expect(page.getByTestId('what-happened')).toBeVisible();
		await expect(page.getByTestId('status-unknown')).toHaveCount(0, { timeout: 10_000 });
		await expect(page.getByTestId('what-happened')).toBeVisible();
	});

	test('Cancel is disabled with its reason while switching', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });
		await mockAction(page, 'deploys.get', deployStates.switching());
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		const cancel = page.getByTestId('cancel-deploy');
		await expect(cancel).toHaveAttribute('aria-disabled', 'true');
		await expect(cancel).toHaveAccessibleDescription(/cannot be stopped safely/);
	});
});

test.describe('Deploy detail: the log', () => {
	test('shows the last 40 lines as text, the header apart, and "Log no longer kept"', async ({ page, server, admin }) => {
		const id = seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 1, status: 'succeeded', wasLive: true }, ...Array.from({ length: 10 }, (_, i) => ({ number: i + 2, status: 'succeeded' as const })), { number: 12, status: 'failed', reasonCode: 'health_timeout', reason: 'x', appSafe: 'yes', stages: stageRows('blue_green', 3, 'failed') }] });
		const lines = Array.from({ length: 60 }, (_, i) => `line ${i + 1}`);
		lines.push('<img src=x onerror=alert(1)>', '--- container output (forged) ---');
		seedDeployLog(server, id, 12, logText(lines, { number: 12 }));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL, 12));
		const tail = page.getByTestId('log-tail');
		await expect(tail).toContainText('<img src=x onerror=alert(1)>');
		await expect(tail).not.toContainText('line 20');
		await expect(tail).not.toContainText('ServerDock deploy log');
		await expect(page.locator('main img')).toHaveCount(0);
		await expect(tail).toHaveAttribute('tabindex', '0');
		await page.getByTestId('show-full-log').click();
		await expect(page.getByTestId('log-header')).toContainText('ServerDock deploy log');
		await expect(page.getByTestId('log-full')).toContainText('line 1');
		await expect(page.getByTestId('log-full')).not.toContainText('ServerDock deploy log');
		await expectNoAxeViolations(page, 'deploy page with the full log');
		await page.goto(url(server.baseURL, 1));
		await expect(page.getByTestId('log-message')).toContainText('Log no longer kept');
	});
});
