import type { Page } from '@playwright/test';
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
};

export type SeedService = {
	project?: string;
	name: string;
	image?: string;
	containerPort?: number | null;
	publicPort?: number | null;
	needsAttention?: 'delete_interrupted' | 'versions_kept' | 'previous_version_down' | null;
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
			`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, reason_code, reason, app_safe, committed_at, actor_kind, actor_id, created_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'user', '1', ?, ?)`,
			svc.id,
			d.number,
			d.kind ?? 'deploy',
			JSON.stringify({ image_ref: d.image ?? image }),
			d.ip ?? (d.live ? `10.250.0.${1 + d.number}` : null),
			status,
			d.reasonCode ?? null,
			d.reason ?? null,
			d.appSafe ?? null,
			d.live ? created : null,
			created,
			finished
		);
		if (d.live) {
			const [row] = server.query<{ id: number }>(`SELECT id FROM deploys WHERE service_id = ? AND number = ?`, svc.id, d.number);
			server.sql(`UPDATE services SET active_deploy_id = ? WHERE id = ?`, row.id, svc.id);
		}
	}
	return svc.id;
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
