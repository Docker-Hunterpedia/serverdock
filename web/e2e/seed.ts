import type { Page } from '@playwright/test';
import { chmodSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { gzipSync } from 'node:zlib';
import type { TestServer } from './server';

// Seeded rows for the states that need no containers (plan 0004 step 12). The rows
// are written straight into the server's database through node:sqlite, after the
// server started (the boot pass has run by then and never sees them).
//
// What a seeded service shows depends on Docker:
//  - A server with no Docker socket (the default) is "unknown" for every service
//    that has a live deploy: the events stream is down.
//  - A server with a real Docker socket (startServer({ dockerSocket: DOCKER_SOCKET })),
//    once its first resync has finished, shows "missing" for a service with a live
//    deploy and no container.
//  - Not seedable: running, restarting, exited (they need a container). Deploy for
//    real and `docker stop` the container, or fake the API answer with mockServices().

export type SeedDeploy = {
	number: number;
	status?: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled' | 'superseded';
	kind?: 'deploy' | 'redeploy' | 'rollback';
	/** the image the deploy ran (kept in its snapshot) */
	image?: string;
	reasonCode?: string;
	reason?: string;
	appSafe?: 'yes' | 'no' | 'unknown';
	ip?: string;
	/** ISO time; default: a fixed time in 2026 */
	createdAt?: string;
	finishedAt?: string;
	/** make this the service's live deploy (it also gets committed_at) */
	live?: boolean;
	/** committed once and not live now ("Replaced": a rollback target); `live` implies it */
	wasLive?: boolean;
	/** the stage the deploy got to */
	stage?: string;
	/** a superseded deploy: the NUMBER of the deploy that replaced it (same service) */
	supersededBy?: number;
	/** user (default), token, mcp, webhook or system */
	actorKind?: string;
	/** the strategy recorded in the deploy's settings (a rollback keeps it) */
	strategy?: 'blue_green' | 'stop_first';
	/** the deploy's stage rows (deploy_stages), in order */
	stages?: SeedStage[];
};

export type SeedStage = { stage: string; state: 'pending' | 'running' | 'ok' | 'failed' | 'skipped'; detail?: string };

export type SeedService = {
	project?: string;
	name: string;
	image?: string;
	containerPort?: number | null;
	publicPort?: number | null;
	needsAttention?: 'delete_interrupted' | 'delete_confirm_again' | 'versions_kept' | 'previous_version_down' | null;
	deploys?: SeedDeploy[];
};

const AT = '2026-11-20T14:02:00.000Z';

/** Inserts a project (if new), a service and its deploys. Returns the service id. */
export function seedService(server: TestServer, s: SeedService): number {
	const project = s.project ?? 'default';
	const image = s.image ?? 'nginx:1.27';
	server.sql(`INSERT OR IGNORE INTO projects(name, created_at) VALUES (?, ?)`, project, AT);
	const [p] = server.query<{ id: number }>(`SELECT id FROM projects WHERE name = ?`, project);
	server.sql(
		`INSERT INTO services(project_id, name, image_ref, container_port, public_port, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, needs_attention, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 60, 3, 'blue_green', 10, 10, ?, ?, ?)`,
		p.id,
		s.name,
		image,
		s.containerPort ?? null,
		s.publicPort ?? null,
		s.containerPort ? 'tcp' : 'running',
		s.needsAttention ?? null,
		AT,
		AT
	);
	const [svc] = server.query<{ id: number }>(`SELECT id FROM services WHERE project_id = ? AND name = ?`, p.id, s.name);
	for (const d of s.deploys ?? []) {
		const status = d.status ?? 'succeeded';
		const created = d.createdAt ?? AT;
		const finished = d.finishedAt ?? (status === 'queued' || status === 'running' ? null : created);
		server.sql(
			`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, stage, reason_code, reason, app_safe, committed_at, actor_kind, actor_id, created_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '1', ?, ?)`,
			svc.id,
			d.number,
			d.kind ?? 'deploy',
			JSON.stringify({ image_ref: d.image ?? image, ...(d.strategy ? { strategy: d.strategy } : {}) }),
			d.ip ?? (d.live ? `10.250.0.${1 + d.number}` : null),
			status,
			d.stage ?? null,
			d.reasonCode ?? null,
			d.reason ?? null,
			d.appSafe ?? null,
			d.live || d.wasLive ? created : null,
			d.actorKind ?? 'user',
			created,
			finished
		);
		for (const st of d.stages ?? []) {
			server.sql(
				`INSERT INTO deploy_stages(deploy_id, stage, state, started_at, finished_at, detail) VALUES ((SELECT id FROM deploys WHERE service_id = ? AND number = ?), ?, ?, ?, ?, ?)`,
				svc.id,
				d.number,
				st.stage,
				st.state,
				st.state === 'pending' ? null : created,
				st.state === 'ok' || st.state === 'failed' || st.state === 'skipped' ? created : null,
				st.detail ?? null
			);
		}
		if (d.live) {
			const [row] = server.query<{ id: number }>(`SELECT id FROM deploys WHERE service_id = ? AND number = ?`, svc.id, d.number);
			server.sql(`UPDATE services SET active_deploy_id = ? WHERE id = ?`, row.id, svc.id);
		}
	}
	// "Superseded by #N" points at a deploy row, so it is set once every row exists.
	for (const d of s.deploys ?? []) {
		if (!d.supersededBy) continue;
		const [by] = server.query<{ id: number }>(`SELECT id FROM deploys WHERE service_id = ? AND number = ?`, svc.id, d.supersededBy);
		server.sql(`UPDATE deploys SET superseded_by = ? WHERE service_id = ? AND number = ?`, by.id, svc.id, d.number);
	}
	return svc.id;
}

/**
 * A service whose history shows every result kind (plan 0004 step 13): #1 Replaced,
 * #2 Failed app safe, #3 Failed app not safe, #4 Interrupted, #5 Cancelled, #6 Superseded
 * by #7, #7 Replaced (a redeploy), #8 Live, #9 Queued. Rollback is offered on #1 and #7.
 * Without Docker the state is "unknown"; with a Docker server and no container, "missing".
 */
export function seedHistory(server: TestServer, project = 'shop', name = 'api', extra: Partial<SeedService> = {}) {
	const t = (n: number) => `2026-11-${String(10 + n).padStart(2, '0')}T09:00:00.000Z`;
	return seedService(server, {
		...extra,
		project,
		name,
		image: 'registry.example.com/shop/api:2.4.1',
		containerPort: 8080,
		publicPort: 8081,
		deploys: [
			{ number: 1, wasLive: true, image: 'registry.example.com/shop/api:2.3.0', createdAt: t(1), stage: 'done' },
			{ number: 2, status: 'failed', reasonCode: 'health_timeout', reason: 'the new version did not become healthy', appSafe: 'yes', stage: 'health', createdAt: t(2) },
			{ number: 3, status: 'failed', reasonCode: 'start_failed', reason: 'the old version did not come back', appSafe: 'no', stage: 'maintenance', createdAt: t(3) },
			{ number: 4, status: 'failed', reasonCode: 'interrupted', reason: 'interrupted by restart', appSafe: 'unknown', stage: 'start', createdAt: t(4) },
			{ number: 5, status: 'cancelled', reasonCode: 'cancelled', stage: 'pull', createdAt: t(5) },
			{ number: 6, status: 'superseded', reasonCode: 'superseded', stage: 'pull', createdAt: t(6), supersededBy: 7 },
			{ number: 7, kind: 'redeploy', wasLive: true, stage: 'done', createdAt: t(7), actorKind: 'token' },
			{ number: 8, live: true, stage: 'done', createdAt: t(8), actorKind: 'system' },
			{ number: 9, status: 'queued', createdAt: t(9) }
		]
	});
}

/** The three-services-in-two-projects set of the plan's "Services with 3 services" screen state. */
export function seedThreeServices(server: TestServer) {
	seedService(server, {
		project: 'shop',
		name: 'api',
		image: 'registry.example.com/shop/api:2.4.1',
		containerPort: 8080,
		publicPort: 8081,
		deploys: [
			{ number: 1, status: 'succeeded', createdAt: '2026-11-18T09:00:00.000Z' },
			{ number: 2, status: 'succeeded', live: true, createdAt: '2026-11-20T14:02:00.000Z' }
		]
	});
	seedService(server, {
		project: 'shop',
		name: 'worker',
		image: 'registry.example.com/shop/worker:2.4.1',
		deploys: [{ number: 1, status: 'failed', reasonCode: 'health', reason: 'the new version did not become healthy', appSafe: 'yes', createdAt: '2026-11-21T08:30:00.000Z' }]
	});
	seedService(server, { project: 'blog', name: 'web', image: 'ghost:5', containerPort: 2368 });
}

// ---- faking API answers for what cannot be seeded --------------------------------

/** Answers an action's call with `data` (a successful, unaudited read) for this page. Register before navigating. */
export async function mockAction(page: Page, name: string, data: unknown) {
	await page.route(`**/api/v1/actions/${name}`, (route) =>
		route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data }) })
	);
}

/** The services.list answer with the given services' fields merged over a complete service. */
export function serviceFixture(over: Record<string, unknown> & { project: string; name: string }) {
	return {
		service: `${over.project}/${over.name}`,
		image_ref: 'nginx:1.27',
		container_port: 80,
		public_port: null,
		health: { kind: 'tcp', timeout_s: 60, passes: 3 },
		strategy: 'blue_green',
		drain_s: 10,
		stop_grace_s: 10,
		state: 'running',
		active_deploy: 1,
		last_deploy: { number: 1, kind: 'deploy', status: 'succeeded', created_at: AT, finished_at: AT },
		routes_stale: false,
		routes_ready: true,
		warnings: [],
		created_at: AT,
		updated_at: AT,
		...over
	};
}

/** The deploys.list item with the given fields merged over a succeeded deploy. */
export function deployFixture(over: Record<string, unknown> & { number: number }) {
	return {
		kind: 'deploy',
		status: 'succeeded',
		stage: 'done',
		image_ref: 'nginx:1.27',
		active: false,
		was_live: false,
		log_kept: false,
		triggered_by: 'user',
		created_at: AT,
		finished_at: AT,
		...over
	};
}

/** Fakes services.get (and, when given, deploys.list) for the detail page: route error, missing container, versions_kept and so on. Register before navigating. */
export async function mockServiceDetail(page: Page, service: ReturnType<typeof serviceFixture>, deploys: ReturnType<typeof deployFixture>[] = []) {
	await mockAction(page, 'services.get', service);
	await mockAction(page, 'deploys.list', { service: service.service, deploys });
}

/**
 * Answers an action with an error envelope (the REST shape): for the refusals a
 * test cannot provoke for real, such as versions_kept or "confirm again". `times`
 * limits how many calls get the error (then the call goes on to the server).
 */
export async function mockActionError(page: Page, name: string, status: number, code: string, message: string, times = Infinity) {
	let n = 0;
	await page.route(`**/api/v1/actions/${name}`, (route) => {
		if (n++ >= times) return route.continue();
		return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ error: { code, message } }) });
	});
}

export const mockServices = (page: Page, services: ReturnType<typeof serviceFixture>[]) => mockAction(page, 'services.list', { services });

/**
 * The system.exposure answer: foreign containers that publish host ports on the panel's
 * address. A port is [hostPort, proto, containerPort?, hostIP?]: the host port is what the
 * banner must show, so the container port defaults to 80 (never equal to a test's host port).
 * The same host port on 0.0.0.0 and on :: is two entries, as Docker reports it.
 */
export const mockExposure = (page: Page, containers: { name: string; ports: [number, string, number?, string?][] }[]) =>
	mockAction(page, 'system.exposure', {
		checked_at: AT,
		containers: containers.map((c) => ({
			name: c.name,
			ports: c.ports.map(([host, proto, container, ip]) => ({ host_ip: ip ?? '0.0.0.0', host_port: host, container_port: container ?? 80, proto }))
		}))
	});

// ---- the Deploy detail page (plan 0004 step 14) -------------------------------------

/** The stage rows of a blue-green deploy with the first `done` stages ok, the next one in `state`, the rest pending. */
export function stageRows(strategy: 'blue_green' | 'stop_first', done: number, state: SeedStage['state'] = 'running', detail?: string): SeedStage[] {
	const names = strategy === 'stop_first' ? ['pull', 'check', 'maintenance', 'stop_old_first', 'start', 'health', 'switch', 'commit', 'stop_old'] : ['pull', 'check', 'start', 'health', 'switch', 'commit', 'drain', 'stop_old'];
	return names.map((stage, i) => ({ stage, state: i < done ? 'ok' : i === done ? state : 'pending', ...(i === done && detail ? { detail } : {}) }));
}

/** Writes a deploy log file (gzip, 0600 in a 0700 directory) for a seeded deploy, as the server keeps it. */
export function seedDeployLog(server: TestServer, serviceId: number, deployNumber: number, text: string) {
	const [row] = server.query<{ id: number }>(`SELECT id FROM deploys WHERE service_id = ? AND number = ?`, serviceId, deployNumber);
	const dir = join(server.dataDir, 'deploy-logs', String(serviceId));
	mkdirSync(dir, { recursive: true, mode: 0o700 });
	chmodSync(dir, 0o700);
	const file = join(dir, `${row.id}.log.gz`);
	writeFileSync(file, gzipSync(text), { mode: 0o600 });
}

/** A deploy log in the server's format: the header and stage lines ServerDock wrote, the separator, then what the container printed. */
export function logText(output: string[], opts: { service?: string; number?: number } = {}): string {
	const head = [
		'ServerDock deploy log',
		`service: ${opts.service ?? 'shop/api'}`,
		`deploy: #${opts.number ?? 1} (deploy)`,
		'image: registry.example.com/shop/api:2.4.1',
		'status when this log was kept: failed health',
		'reason: health_timeout the new version did not become healthy',
		'container: 0123456789ab',
		'pull: ok',
		'check: ok',
		'start: ok',
		'health: failed'
	];
	return [...head, '--- container output (the newest 2000 lines at most; 1024 KiB in all) ---', ...output, ''].join('\n');
}

type Over = Record<string, unknown>;
/** A deploys.get answer: the deploy (merged over a running blue-green pull), its stages, progress and cancellable. */
export function deployGetFixture(over: { deploy?: Over; stages?: SeedStage[]; progress?: Over | null; cancellable?: boolean } = {}) {
	const deploy = {
		number: 3,
		kind: 'deploy',
		status: 'running',
		stage: 'pull',
		image_ref: 'registry.example.com/shop/api:2.4.1',
		strategy: 'blue_green',
		active: false,
		was_live: false,
		log_kept: false,
		triggered_by: 'user',
		created_at: AT,
		started_at: AT,
		...over.deploy
	} as Over & { status: string; stage: string };
	const running = deploy.status === 'running' || deploy.status === 'queued';
	return {
		service: 'shop/api',
		deploy,
		stages: (over.stages ?? (deploy.status === 'queued' ? [] : stageRows('blue_green', 0))).map((s) => ({ ...s, started_at: AT })),
		...(over.progress === null ? {} : { progress: over.progress ?? (deploy.stage === 'pull' && running ? { layers: 5, layers_done: 2, bytes: 12_000_000, bytes_total: 48_000_000, status: 'Downloading' } : undefined) }),
		cancellable: over.cancellable ?? (running && !['switch', 'commit', 'drain', 'stop_old', 'done'].includes(deploy.stage))
	};
}

/** Each screen state of the Deploy detail page as a deploys.get answer (the plan's state list). */
export const deployStates = {
	queued: () => deployGetFixture({ deploy: { status: 'queued', stage: undefined, started_at: undefined } }),
	pulling: () => deployGetFixture(),
	pullingUnknownSize: () => deployGetFixture({ progress: { layers: 0, layers_done: 0, bytes: 0, bytes_total: 0 } }),
	waitingToRetry: () => deployGetFixture({ stages: stageRows('blue_green', 0, 'pending', 'waiting to retry (attempt 2 of 3): the registry did not answer'), progress: null }),
	healthGate: () => deployGetFixture({ deploy: { stage: 'health' }, stages: stageRows('blue_green', 3), progress: null }),
	switching: () => deployGetFixture({ deploy: { stage: 'switch' }, stages: stageRows('blue_green', 4), progress: null }),
	succeeded: () => deployGetFixture({ deploy: { status: 'succeeded', stage: 'done', active: true, was_live: true, finished_at: AT, log_kept: true }, stages: stageRows('blue_green', 8, 'ok') }),
	failedAppSafe: () =>
		deployGetFixture({
			deploy: { status: 'failed', stage: 'health', reason_code: 'health_timeout', reason: 'the new version did not become healthy within 60 s', app_safe: 'yes', finished_at: AT, log_kept: true },
			stages: stageRows('blue_green', 3, 'failed', 'no answer on port 8080')
		}),
	failedNotSafe: () =>
		deployGetFixture({
			deploy: { status: 'failed', stage: 'start', strategy: 'stop_first', reason_code: 'start_failed', reason: 'the new version could not be started and the old one did not come back', app_safe: 'no', finished_at: AT, log_kept: true },
			stages: stageRows('stop_first', 4, 'failed')
		}),
	interrupted: () =>
		deployGetFixture({ deploy: { status: 'failed', stage: 'start', reason_code: 'interrupted', reason: 'ServerDock stopped while the deploy was running', app_safe: 'unknown', finished_at: AT }, stages: stageRows('blue_green', 2, 'failed') }),
	superseded: () => deployGetFixture({ deploy: { status: 'superseded', stage: undefined, reason_code: 'superseded', superseded_by: 4, finished_at: AT }, stages: [] }),
	cancelled: () => deployGetFixture({ deploy: { status: 'cancelled', stage: 'pull', reason_code: 'cancelled', finished_at: AT }, stages: stageRows('blue_green', 0, 'skipped') }),
	diskFull: () =>
		deployGetFixture({
			deploy: { status: 'failed', stage: 'pull', reason_code: 'disk_full', reason: "the server's disk is full: no space left on device", app_safe: 'yes', finished_at: AT },
			stages: stageRows('blue_green', 0, 'failed', 'no space left on device')
		})
};

/**
 * Answers deploys.get from a function of the call number (0, 1, 2, ...): the last call's
 * answer repeats. Returns the call count and the times of the calls (page clock or real).
 * Register before navigating.
 */
export async function mockDeployGet(page: Page, answer: (call: number) => unknown | { status: number }) {
	const calls: number[] = [];
	await page.route('**/api/v1/actions/deploys.get', (route) => {
		const n = calls.length;
		calls.push(Date.now());
		const a = answer(n) as { status?: number } & Record<string, unknown>;
		if (a && typeof a === 'object' && 'status' in a && Object.keys(a).length === 1) return route.fulfill({ status: a.status, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unavailable', message: 'down' } }) });
		return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ audit: 'not_audited', data: a }) });
	});
	return { calls };
}

/** Answers deploys.log with a kept log, or with `message` when none is kept. */
export const mockDeployLog = (page: Page, log: { text?: string; truncated?: boolean; message?: string }) =>
	mockAction(page, 'deploys.log', { service: 'shop/api', number: 3, kept: log.text !== undefined, ...(log.text !== undefined ? { text: log.text, truncated: log.truncated } : { message: log.message ?? 'Log no longer kept (only the last 10 deploys keep logs)' }) });
