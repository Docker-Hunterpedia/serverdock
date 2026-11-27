import type { Page } from '@playwright/test';
import { expect, expectNoAxeViolations, signIn, test } from './fixtures';
import { deployGetFixture, deployStates, mockAction, mockDeployGet, mockDeployLog, logText, seedService, stageRows } from './seed';

// Plan 0004 step 14, the tester's tests for the live Deploy detail page: screen states
// (what each shows and that it has no axe violations), the announcements, the polling
// (hidden, finished, 5 s back-off, a failed or stale poll), Cancel, and the sizes.
// Running states are faked with deploys.get answers: a running deploy with a job cannot be
// seeded. page.clock moves the page's timers (the 2 s polls) but not AbortSignal.timeout.
test.use({ storageState: { cookies: [], origins: [] } });

const url = (base: string, n = 3) => `${base}/services/shop/api/deploys/${n}`;
const seed = (server: Parameters<typeof seedService>[0]) => seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 3, status: 'queued' }] });

/** Records every distinct non-empty text the page's status and alert regions held (the announcements). */
async function recordAnnouncements(page: Page) {
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
const announced = (page: Page) => page.evaluate(() => (window as unknown as { __said: { status: string[]; alert: string[] } }).__said);

/** The page's clock stands still (from a fixed time) until a test moves it: no timer fires by itself. */
async function freeze(page: Page) {
	const t0 = new Date('2026-11-27T10:00:00Z');
	await page.clock.install({ time: t0 });
	await page.clock.pauseAt(new Date(t0.getTime() + 1));
}

/** Lets the page's clock run `ms` and waits for the poll that this triggers (the answer is rendered after a short real wait). */
async function pollOnce(page: Page, calls: number[], ms = 2000) {
	const before = calls.length;
	await page.clock.runFor(ms);
	await expect.poll(() => calls.length, { message: 'a poll after the timer fired' }).toBeGreaterThan(before);
	await page.waitForTimeout(150);
}

test.describe('Deploy detail: what each screen state shows', () => {
	type Check = (page: Page) => Promise<void>;
	const visible = (...ids: string[]): Check => async (page) => {
		for (const id of ids) await expect(page.getByTestId(id), id).toBeVisible();
	};
	const absent = (...ids: string[]): Check => async (page) => {
		for (const id of ids) await expect(page.getByTestId(id), id).toHaveCount(0);
	};
	const all = (...c: Check[]): Check => async (page) => {
		for (const f of c) await f(page);
	};
	const stage = (name: string, state: string): Check => async (page) => {
		await expect(page.getByTestId(`stage-${name}`)).toHaveAttribute('data-state', state);
	};
	const text = (id: string, re: RegExp): Check => async (page) => {
		await expect(page.getByTestId(id)).toContainText(re);
	};
	const cancelEnabled: Check = async (page) => {
		await expect(page.getByTestId('cancel-deploy')).toBeVisible();
		await expect(page.getByTestId('cancel-deploy')).not.toHaveAttribute('aria-disabled', 'true');
		await expect(page.getByTestId('cancel-reason')).toHaveCount(0);
	};

	const cases: [keyof typeof deployStates, string, Check][] = [
		['queued', 'Queued', all(visible('no-stages', 'what-happened', 'this-deploy', 'log-section'), absent('stages', 'pull-progress'), cancelEnabled, text('log-message', /no log while the deploy runs/))],
		['pulling', 'Running', all(visible('stages', 'pull-progress', 'pull-text'), stage('pull', 'running'), text('pull-text', /25%.*2 of 5 layers/), cancelEnabled)],
		['pullingUnknownSize', 'Running', all(visible('pull-progress'), text('pull-text', /Size not known yet/), async (page) => {
			// No bar without a size (an indeterminate bar animates and cannot be paused): the text says it.
			await expect(page.locator('progress')).toHaveCount(0);
		})],
		['waitingToRetry', 'Running', all(visible('waiting-retry'), text('waiting-retry', /waiting to retry/), absent('pull-progress'), stage('pull', 'pending'), text('stage-pull', /attempt 2 of 3/))],
		['healthGate', 'Running', all(stage('pull', 'ok'), stage('check', 'ok'), stage('start', 'ok'), stage('health', 'running'), absent('pull-progress'), cancelEnabled)],
		['switching', 'Running', all(visible('cancel-reason'), text('cancel-reason', /cannot be stopped safely/), stage('switch', 'running'))],
		['succeeded', 'Live', all(visible('redeploy', 'log-section'), absent('rollback', 'cancel-deploy', 'pull-progress', 'status-unknown'), text('app-safety', /the new version is live/), text('deploy-result', /Live/))],
		['failedAppSafe', 'Failed · app safe', all(stage('health', 'failed'), text('stage-health', /no answer on port 8080/), text('deploy-reason', /did not become healthy within 60 s/), absent('cancel-deploy', 'redeploy', 'rollback', 'disk-full', 'interrupted'))],
		['failedNotSafe', 'Failed · app not safe', all(text('app-safety', /not safe/), text('deploy-reason', /did not come back/), text('this-deploy', /Stop-first|stop-first/i), absent('cancel-deploy'))],
		['interrupted', 'Interrupted', all(visible('interrupted'), text('interrupted', /cannot say how far it got/), absent('disk-full', 'cancel-deploy'))],
		['superseded', 'Superseded by #4', all(text('deploy-reason', /^Replaced in the queue by #4 before it started\.$/), text('app-safety', /not changed: this deploy never started/), text('no-stages', /No stage ran/), absent('cancel-deploy', 'stages'))],
		['supersededStarted', 'Superseded by #4', all(text('deploy-reason', /^Stopped and replaced by #4\.$/), text('app-safety', /unknown: ServerDock could not confirm/), stage('start', 'skipped'), absent('cancel-deploy'))],
		['cancelled', 'Cancelled', all(text('deploy-reason', /It was cancelled\./), text('app-safety', /unknown: ServerDock could not confirm/), stage('pull', 'skipped'), absent('cancel-deploy'))],
		['cancelledNotSafe', 'Cancelled', all(text('app-safety', /not safe: the previous version is not serving/), absent('cancel-deploy'))],
		['diskFull', 'Failed', all(visible('disk-full'), text('disk-full', /Free some space/), text('stage-pull', /no space left on device/), stage('pull', 'failed'), absent('interrupted'))]
	];
	for (const [state, word, check] of cases) {
		test(`${state}: shows what it should, with its stages and actions`, async ({ page, server, admin }) => {
			seed(server);
			await mockAction(page, 'deploys.get', deployStates[state]());
			await mockDeployLog(page, { text: logText(['hello from the app'], { number: 3 }) });
			await signIn(page, server, admin);
			await page.goto(url(server.baseURL));
			await expect(page.getByRole('heading', { level: 1 })).toContainText('Deploy #3');
			await expect(page.getByTestId('deploy-status')).toContainText(word);
			await check(page);
			// The pull's bar and text are never a live region (plan 0004 accessibility rules).
			await expect(page.locator('[role=status] progress, [role=alert] progress, [aria-live] progress, [aria-live] [data-testid=pull-text]')).toHaveCount(0);
			await expect(page.getByRole('status')).toHaveText('');
			await expect(page.getByRole('alert')).toHaveText('');
			await expectNoAxeViolations(page, `deploy page: ${state}`);
		});
	}

	test('a replaced deploy that was once live offers Roll back (not Redeploy), with no axe violations', async ({ page, server, admin }) => {
		seed(server);
		const live = deployStates.succeeded();
		await mockAction(page, 'deploys.get', { ...live, deploy: { ...live.deploy, active: false, was_live: true } });
		await mockDeployLog(page, { text: logText(['x'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('deploy-status')).toContainText('Replaced');
		await expect(page.getByTestId('rollback')).toHaveText('Roll back to #3');
		await expect(page.getByTestId('redeploy')).toHaveCount(0);
		await expect(page.getByTestId('app-safety')).toContainText('has since been replaced');
		await expectNoAxeViolations(page, 'deploy page: replaced');
	});

	test('a cancelled deploy as the server stores it (reason "cancelled") says "It was cancelled.", not the bare code', async ({ page, server, admin }) => {
		seed(server);
		const c = deployStates.cancelled();
		await mockAction(page, 'deploys.get', { ...c, deploy: { ...c.deploy, reason: 'cancelled' } });
		await mockDeployLog(page, { message: 'No log' });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('deploy-reason')).toHaveText('It was cancelled.');
	});

	test('the pull progress bar has a name, a description and the bytes', async ({ page, server, admin }) => {
		seed(server);
		await mockAction(page, 'deploys.get', deployStates.pulling());
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		const bar = page.getByRole('progressbar');
		await expect(bar).toHaveAccessibleName('Image pull progress');
		await expect(bar).toHaveAccessibleDescription(/of 45\.8 MiB/);
		await expect(bar).toHaveAttribute('max', '48000000');
		await expect(bar).toHaveAttribute('value', '12000000');
	});
});

test.describe('Deploy detail: unknown status', () => {
	test('a failing first load shows "Status unknown" with Check again, honestly and without axe violations', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		let up = false;
		await mockDeployGet(page, () => (up ? deployStates.healthGate() : { status: 502 }));
		await signIn(page, server, admin);
		allow5xx();
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('status-unknown')).toContainText('Status unknown: could not reach the server');
		await expect(page.getByTestId('deploy-status')).toContainText('Unknown');
		// Nothing about the deploy is claimed.
		for (const id of ['what-happened', 'stages', 'this-deploy', 'cancel-deploy', 'redeploy', 'deploy-result']) await expect(page.getByTestId(id)).toHaveCount(0);
		await expectNoAxeViolations(page, 'deploy page: status unknown');
		// Check again asks now, and says so again while the server is still down.
		await page.getByTestId('check-again').click();
		await expect(page.getByTestId('status-unknown')).toBeVisible();
		up = true;
		await page.getByTestId('check-again').click();
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
		await expect(page.getByTestId('deploy-status')).toContainText('Running');
	});

	test('a poll that gets no answer within 8 s counts as failed', async ({ page, server, admin }) => {
		test.setTimeout(60_000);
		seed(server);
		let n = 0;
		const held: (() => void)[] = [];
		await page.route('**/api/v1/actions/deploys.get', (route) => {
			if (n++ === 0) return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: deployStates.healthGate() }) });
			// never answered: the page's own 8 s limit has to end the wait
			held.push(() => route.abort());
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toBeVisible();
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		// 2 s timer + 8 s limit: unknown by about 10 s, not before the limit.
		await expect(page.getByTestId('status-unknown')).toBeVisible({ timeout: 20_000 });
		// The last known state stays, marked as possibly out of date, and nothing can be acted on.
		await expect(page.getByTestId('what-happened')).toBeVisible();
		await expect(page.getByTestId('status-unknown')).toContainText('may be out of date');
		await expect(page.getByTestId('cancel-deploy')).toHaveAttribute('aria-disabled', 'true');
		held.forEach((f) => f());
	});

	test('data older than 10 s (a late timer) is replaced by "Status unknown" until the next answer', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		let n = 0;
		let release: (() => void) | undefined;
		await page.route('**/api/v1/actions/deploys.get', async (route) => {
			const body = JSON.stringify({ audit: 'not_audited', data: deployStates.healthGate() });
			if (n++ >= 1) await new Promise<void>((r) => (release = r));
			await route.fulfill({ status: 200, contentType: 'application/json', body });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('what-happened')).toBeVisible();
		// The tab slept for 30 s: the timer fires late, the old state must not look current.
		await page.clock.fastForward(30_000);
		await expect(page.getByTestId('status-unknown')).toBeVisible();
		await expect(page.getByTestId('what-happened')).toBeVisible();
		await expect(page.getByTestId('cancel-deploy')).toHaveAttribute('aria-disabled', 'true');
		release?.();
		await expect(page.getByTestId('status-unknown')).toHaveCount(0);
		await expect(page.getByTestId('what-happened')).toBeVisible();
	});
});

test.describe('Deploy detail: announcements', () => {
	test('each stage change is announced once through the status region, progress ticks never', async ({ page, server, admin }) => {
		seed(server);
		await recordAnnouncements(page);
		await freeze(page);
		const progress = (bytes: number, done: number) => ({ layers: 5, layers_done: done, bytes, bytes_total: 48_000_000, status: 'Downloading' });
		const { calls } = await mockDeployGet(page, (n) => {
			if (n <= 3) return deployGetFixture({ progress: progress(8_000_000 * (n + 1), n) }); // four pull answers, the bar moves each time
			if (n <= 6) return deployStates.healthGate(); // the same stage three times
			if (n === 7) return deployStates.switching();
			return deployStates.succeeded();
		});
		await mockDeployLog(page, { text: logText(['up'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-text')).toContainText('(16%)');
		const texts: string[] = [];
		const seenText = async () => texts.push((await page.getByTestId('pull-text').textContent()) ?? '');
		await seenText();
		for (let i = 0; i < 3; i++) {
			await pollOnce(page, calls);
			await seenText();
		}
		expect(new Set(texts).size, 'the progress text changed on every poll').toBe(4);
		expect((await announced(page)).status, 'no announcement for progress ticks').toEqual([]);
		await pollOnce(page, calls); // health gate
		await pollOnce(page, calls); // the same stage again
		await pollOnce(page, calls); // the same stage again
		await pollOnce(page, calls); // switching
		await pollOnce(page, calls); // succeeded
		await expect(page.getByTestId('deploy-status')).toContainText('Live');
		const said = await announced(page);
		expect(said.status, 'one line per change: the stage, the stage, success').toHaveLength(3);
		expect(said.status[0]).toBe('Deploy #3: health check started.');
		expect(said.status[1]).toBe('Deploy #3: traffic switch started.');
		expect(said.status[2]).toBe('Deploy #3 succeeded. The new version is live.');
		expect(said.alert).toEqual([]);
		await expect(page.getByRole('status')).toHaveText('Deploy #3 succeeded. The new version is live.');
	});

	test('"waiting to retry" is announced once with its attempt, and the next try is announced again', async ({ page, server, admin }) => {
		seed(server);
		await recordAnnouncements(page);
		await freeze(page);
		const retry = (k: number) => deployGetFixture({ stages: stageRows('blue_green', 0, 'pending', `waiting to retry (attempt ${k} of 3): the registry did not answer`), progress: null });
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.pulling() : n <= 2 ? retry(2) : n === 3 ? deployStates.pulling() : retry(3)));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		for (let i = 0; i < 4; i++) await pollOnce(page, calls);
		const said = await announced(page);
		expect(said.status).toEqual(['Deploy #3: image pull did not succeed. Waiting to retry, attempt 2 of 3.', 'Deploy #3: image pull started.', 'Deploy #3: image pull did not succeed. Waiting to retry, attempt 3 of 3.']);
	});

	for (const [state, re] of [
		['failedAppSafe', /Deploy #3 failed: The new version did not become healthy within 60 s\. Your app is safe: the previous version kept serving\./],
		['failedNotSafe', /Deploy #3 failed: .*did not come back\. Your app is not safe: /],
		['interrupted', /Deploy #3 was interrupted: ServerDock stopped while the deploy was running\. Your app status is unknown: /],
		['diskFull', /Deploy #3 failed: The server's disk is full: no space left on device\. Your app is safe: /]
	] as const) {
		test(`a ${state} deploy is announced once, through the alert only, and focus does not move`, async ({ page, server, admin }) => {
			seed(server);
			await recordAnnouncements(page);
			await freeze(page);
			const { calls } = await mockDeployGet(page, (n) => (n < 2 ? deployStates.pulling() : deployStates[state]()));
			await mockDeployLog(page, { text: logText(['boom'], { number: 3 }) });
			await signIn(page, server, admin);
			await page.goto(url(server.baseURL));
			await expect(page.getByTestId('pull-progress')).toBeVisible();
			await page.getByRole('main').getByRole('link', { name: 'Services', exact: true }).first().focus();
			await page.evaluate(() => ((window as unknown as { __f: Element | null }).__f = document.activeElement));
			await pollOnce(page, calls);
			await pollOnce(page, calls);
			await expect(page.getByRole('alert')).toContainText(re);
			// No more polls, so nothing can announce it again; the status region stays silent.
			const n = calls.length;
			await page.clock.runFor(60_000);
			await page.waitForTimeout(300);
			expect(calls.length, 'polling stopped').toBe(n);
			const said = await announced(page);
			expect(said.alert).toHaveLength(1);
			expect(said.status).toEqual([]);
			await expect(page.getByRole('alert')).toBeVisible();
			expect(await page.evaluate(() => document.activeElement === (window as unknown as { __f: Element | null }).__f), 'no focus movement on polls').toBe(true);
		});
	}

	test('success is announced once and Cancel, in a finished deploy, is gone', async ({ page, server, admin }) => {
		seed(server);
		await recordAnnouncements(page);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : deployStates.succeeded()));
		await mockDeployLog(page, { text: logText(['up'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('cancel-deploy')).toBeVisible();
		await pollOnce(page, calls);
		await expect(page.getByTestId('cancel-deploy')).toHaveCount(0);
		await expect(page.getByTestId('redeploy')).toBeVisible();
		expect((await announced(page)).status).toEqual(['Deploy #3 succeeded. The new version is live.']);
	});
});

test.describe('Deploy detail: polling', () => {
	test('asks every 2 s while the deploy runs, and keeps focus where it is', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		const { calls } = await mockDeployGet(page, () => deployStates.healthGate());
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toBeVisible();
		const cancel = page.getByTestId('cancel-deploy');
		await cancel.focus();
		const start = calls.length;
		// 1.9 s: no poll yet. 2 s: exactly one more.
		await page.clock.runFor(1900);
		await page.waitForTimeout(300);
		expect(calls.length).toBe(start);
		await pollOnce(page, calls, 100);
		await pollOnce(page, calls);
		await pollOnce(page, calls);
		expect(calls.length).toBe(start + 3);
		await expect(cancel).toBeFocused();
	});

	test('stops while the page is hidden, refreshes first on return, and announces after that', async ({ page, server, admin }) => {
		seed(server);
		await recordAnnouncements(page);
		await page.addInitScript(() => {
			let state = 'visible';
			Object.defineProperty(document, 'visibilityState', { get: () => state, configurable: true });
			Object.defineProperty(document, 'hidden', { get: () => state === 'hidden', configurable: true });
			(window as unknown as { __vis: (s: string) => void }).__vis = (s) => {
				state = s;
				document.dispatchEvent(new Event('visibilitychange'));
			};
		});
		await freeze(page);
		let stage: 'pull' | 'health' = 'pull';
		const { calls } = await mockDeployGet(page, () => (stage === 'pull' ? deployStates.pulling() : deployStates.healthGate()));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await pollOnce(page, calls);
		await page.evaluate(() => (window as unknown as { __vis: (s: string) => void }).__vis('hidden'));
		stage = 'health'; // the deploy moves on while the page cannot be seen
		const n = calls.length;
		await page.clock.runFor(30_000);
		await page.waitForTimeout(400);
		expect(calls.length, 'no request while hidden').toBe(n);
		expect((await announced(page)).status, 'no announcement while hidden').toEqual([]);
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		// Back: asks at once (no timer needed), then announces the new stage.
		await page.evaluate(() => (window as unknown as { __vis: (s: string) => void }).__vis('visible'));
		await expect.poll(() => calls.length, { message: 'a refresh when visible again' }).toBe(n + 1);
		await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
		await expect(page.getByRole('status')).toHaveText('Deploy #3: health check started.');
		// ...and polling goes on.
		await pollOnce(page, calls);
		expect(calls.length).toBe(n + 2);
	});

	test('hiding the page during a request drops its answer, and a finished deploy stays finished (no more requests)', async ({ page, server, admin }) => {
		seed(server);
		await freeze(page);
		const { calls } = await mockDeployGet(page, (n) => (n === 0 ? deployStates.healthGate() : deployStates.failedAppSafe()));
		await mockDeployLog(page, { text: logText(['x'], { number: 3 }) });
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
		await pollOnce(page, calls);
		await expect(page.getByTestId('deploy-status')).toContainText('Failed');
		const n = calls.length;
		for (const ms of [2000, 5000, 60_000, 600_000]) {
			await page.clock.runFor(ms);
			await page.waitForTimeout(150);
		}
		expect(calls.length, 'no request once the deploy has finished').toBe(n);
	});

	test('backs off from 2 s to 5 s once the page has seen the pull for 2 minutes', async ({ page, server, admin }) => {
		test.setTimeout(120_000);
		seed(server);
		await freeze(page);
		const { calls } = await mockDeployGet(page, () => deployStates.pulling());
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		let elapsed = 0;
		let backedOff = false;
		for (let i = 0; i < 75 && !backedOff; i++) {
			const before = calls.length;
			await page.clock.runFor(2000);
			await page.waitForTimeout(120);
			if (calls.length === before) backedOff = true;
			else elapsed += 2000;
		}
		expect(backedOff, 'a 2 s step stopped producing a poll').toBe(true);
		expect(elapsed, 'the back-off starts after about 2 minutes of pulling, not before').toBeGreaterThanOrEqual(110_000);
		expect(elapsed).toBeLessThanOrEqual(130_000);
		// The poll at 5 s: 2 s have run; 2.9 s more is still early, 0.2 s more is on time.
		const before = calls.length;
		await page.clock.runFor(2800);
		await page.waitForTimeout(200);
		expect(calls.length, 'not at 4.8 s').toBe(before);
		await pollOnce(page, calls, 400);
		expect(calls.length).toBe(before + 1);
		// And again every 5 s: nothing at 2 s, 4.5 s, one at 5 s.
		await page.clock.runFor(2000);
		await page.clock.runFor(2500);
		await page.waitForTimeout(200);
		expect(calls.length).toBe(before + 1);
		await pollOnce(page, calls, 600);
		expect(calls.length).toBe(before + 2);
		// The pull is still on screen the whole time.
		await expect(page.getByTestId('pull-progress')).toBeVisible();
	});

	test('a deploy that left the pull stage is back to 2 s', async ({ page, server, admin }) => {
		test.setTimeout(120_000);
		seed(server);
		await freeze(page);
		let late = false;
		const { calls } = await mockDeployGet(page, () => (late ? deployStates.healthGate() : deployStates.pulling()));
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		// jump the page 3 minutes at 5 s polls: the pull has been on for longer than 2 minutes
		for (let i = 0; i < 70; i++) {
			const before = calls.length;
			await page.clock.runFor(2000);
			await page.waitForTimeout(100);
			if (calls.length === before) break;
		}
		late = true;
		await pollOnce(page, calls, 5000);
		await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
		const n = calls.length;
		await pollOnce(page, calls, 2000);
		expect(calls.length).toBe(n + 1);
	});
});

test.describe('Deploy detail: Cancel', () => {
	test('while pulling, Cancel asks the server, says so, and the page ends at Cancelled', async ({ page, server, admin }) => {
		seed(server);
		let cancelled = false;
		const bodies: unknown[] = [];
		await mockDeployGet(page, () => (cancelled ? deployStates.cancelled() : deployStates.pulling()));
		await page.route('**/api/v1/actions/deploys.cancel', async (route) => {
			bodies.push(route.request().postDataJSON());
			cancelled = true;
			await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'recorded', data: { service: 'shop/api', number: 3, status: 'cancelling' } }) });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		await page.getByTestId('cancel-deploy').click();
		await expect(page.getByTestId('deploy-status')).toContainText('Cancelled');
		expect(bodies).toEqual([{ service: 'shop/api', number: 3 }]);
		await expect(page.getByRole('status')).toHaveText('Deploy #3 was cancelled.');
		await expect(page.getByTestId('cancel-deploy')).toHaveCount(0);
		await expect(page.getByTestId('pull-progress')).toHaveCount(0);
		await expect(page.getByTestId('deploy-reason')).toContainText('It was cancelled');
		await expect(page.getByRole('alert')).toHaveText('');
	});

	test('a cancel the server accepts but has not finished says it stops at the next safe point, and Cancel is then not offered twice', async ({ page, server, admin }) => {
		seed(server);
		let asked = false;
		await mockDeployGet(page, () => (asked ? deployGetFixture({ cancellable: false }) : deployStates.pulling()));
		let n = 0;
		await page.route('**/api/v1/actions/deploys.cancel', async (route) => {
			n++;
			asked = true;
			await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'recorded', data: { service: 'shop/api', number: 3, status: 'cancelling' } }) });
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await page.getByTestId('cancel-deploy').click();
		await expect(page.getByTestId('cancel-reason')).toBeVisible();
		await expect(page.getByTestId('cancel-deploy')).toHaveAttribute('aria-disabled', 'true');
		await expect(page.getByRole('status')).toContainText('Cancel requested');
		await page.getByTestId('cancel-deploy').click({ force: true });
		expect(n, 'a second click sends nothing').toBe(1);
		await expectNoAxeViolations(page, 'deploy page: cancel requested');
	});

	test('a refused cancel (409) is shown through the alert, which takes focus, and the deploy keeps running', async ({ page, server, admin, allow5xx }) => {
		seed(server);
		allow5xx();
		await mockDeployGet(page, () => deployStates.pulling());
		await page.route('**/api/v1/actions/deploys.cancel', (route) =>
			route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ error: { code: 'conflict', message: 'the deploy is switching traffic and cannot be cancelled now' } }) })
		);
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		await page.getByTestId('cancel-deploy').click();
		await expect(page.getByRole('alert')).toContainText(/switching traffic|cannot be cancelled/);
		await expect(page.getByRole('alert')).toBeFocused();
		await expect(page.getByTestId('deploy-status')).toContainText('Running');
		await expect(page.getByTestId('cancel-deploy')).toBeVisible();
	});

	test('during switch Cancel is aria-disabled with its reason, stays focusable, and a click sends nothing', async ({ page, server, admin }) => {
		seed(server);
		await mockAction(page, 'deploys.get', deployStates.switching());
		let sent = 0;
		await page.route('**/api/v1/actions/deploys.cancel', (route) => {
			sent++;
			return route.abort();
		});
		await signIn(page, server, admin);
		await page.goto(url(server.baseURL));
		const cancel = page.getByTestId('cancel-deploy');
		await expect(cancel).toHaveAttribute('aria-disabled', 'true');
		await expect(cancel, 'aria-disabled, not disabled: it stays reachable').not.toHaveAttribute('disabled');
		await expect(cancel).toHaveAccessibleDescription(/cannot be stopped safely/);
		await expect(page.getByTestId('cancel-reason')).toContainText('Traffic is being switched to the new version. This cannot be stopped safely');
		await cancel.focus();
		await expect(cancel).toBeFocused();
		await cancel.click({ force: true }); // Playwright treats aria-disabled as disabled: force it, as a real user's click would land
		await page.keyboard.press('Enter');
		await page.waitForTimeout(300);
		expect(sent, 'no cancel request during switch').toBe(0);
		await expectNoAxeViolations(page, 'deploy page: switching, cancel disabled');
	});

	for (const stage of ['commit', 'drain', 'stop_old'] as const) {
		test(`Cancel is also refused with a reason at ${stage}`, async ({ page, server, admin }) => {
			seed(server);
			await mockAction(page, 'deploys.get', deployGetFixture({ deploy: { stage }, stages: stageRows('blue_green', 5), progress: null }));
			await signIn(page, server, admin);
			await page.goto(url(server.baseURL));
			await expect(page.getByTestId('cancel-deploy')).toHaveAttribute('aria-disabled', 'true');
			await expect(page.getByTestId('cancel-reason')).not.toBeEmpty();
		});
	}
});

test.describe('Deploy detail: sizes', () => {
	const states = ['queued', 'pulling', 'switching', 'succeeded', 'failedAppSafe', 'interrupted', 'diskFull'] as const;
	for (const state of states) {
		test(`${state}: every button is 44 px, nothing overflows at 320 px`, async ({ page, server, admin }) => {
			seed(server);
			await mockAction(page, 'deploys.get', deployStates[state]());
			await mockDeployLog(page, { text: logText([`${'a very long line of container output '.repeat(12)}end`, 'short'], { number: 3 }) });
			await signIn(page, server, admin);
			await page.setViewportSize({ width: 320, height: 568 });
			await page.goto(url(server.baseURL));
			await expect(page.getByTestId('deploy-status')).toBeVisible();
			if (['succeeded', 'failedAppSafe', 'interrupted', 'diskFull'].includes(state)) await expect(page.getByTestId('log-tail')).toBeVisible();
			const targets = page.getByRole('main').locator('button, a.button');
			for (const t of await targets.all()) {
				if (!(await t.isVisible())) continue;
				const box = (await t.boundingBox())!;
				const label = (await t.textContent())?.trim();
				expect(box.height, `${label} is at least 44 px high`).toBeGreaterThanOrEqual(43.5);
				expect(box.width, `${label} is at least 44 px wide`).toBeGreaterThanOrEqual(43.5);
			}
			const over = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, inner: window.innerWidth }));
			expect(over.scroll, 'no sideways scrolling of the page (a long log line scrolls inside its box)').toBeLessThanOrEqual(over.inner);
			await expectNoAxeViolations(page, `deploy page at 320 px: ${state}`);
		});
	}
});
