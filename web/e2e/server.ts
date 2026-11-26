import { request, type APIRequestContext } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

export const PASSWORD = 'correct horse battery staple 42';

export function base32decode(s: string): Buffer {
	const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
	let bits = '';
	for (const ch of s.replace(/[\s=]/g, '').toUpperCase()) bits += alphabet.indexOf(ch).toString(2).padStart(5, '0');
	const out: number[] = [];
	for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
	return Buffer.from(out);
}

/** A TOTP code (SHA-1, 6 digits, 30 s) for `at` (ms since the epoch). */
export function totp(secret: string, at = Date.now()): string {
	const counter = Buffer.alloc(8);
	counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)));
	const h = createHmac('sha1', base32decode(secret)).update(counter).digest();
	const o = h[h.length - 1] & 0xf;
	const n = ((h[o] & 0x7f) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3];
	return (n % 1_000_000).toString().padStart(6, '0');
}

async function freePort(): Promise<number> {
	return new Promise((res, rej) => {
		const s = createServer();
		s.once('error', rej);
		s.listen(0, '127.0.0.1', () => {
			const { port } = s.address() as { port: number };
			s.close(() => res(port));
		});
	});
}

export type Admin = { secret: string; recoveryCodes: string[]; cookies: string; username: string };

export type TestServer = {
	baseURL: string;
	dataDir: string;
	tokenFile: string;
	token: () => string;
	api: APIRequestContext;
	/** Runs the first run through the API; returns the TOTP secret and recovery codes. */
	firstRun: () => Promise<Admin>;
	/** Runs SQL against the server's database (tests expire or revoke sessions this way). */
	sql: (statement: string, ...params: (string | number | null)[]) => void;
	/** Reads rows from the server's database (seeded rows, deploy status). */
	query: <T = Record<string, unknown>>(statement: string, ...params: (string | number | null)[]) => T[];
	/** The install id (meta.install_id): the label every container of this server carries. */
	installId: () => string;
	stop: () => Promise<void>;
};

/** The machine's Docker socket, for the tests that deploy for real (they need Docker and the local registry). */
export const DOCKER_SOCKET = process.env.E2E_DOCKER_SOCKET ?? '/var/run/docker.sock';
/** The test app image the e2e job pushes to its local registry (see .github/workflows/ci.yml). */
export const TEST_APP_IMAGE = process.env.SD_TEST_APP_IMAGE ?? '127.0.0.1:5000/sd-testapp:1';

export type ServerOptions = {
	https?: number;
	/** A Docker socket path. Default: one that does not exist (no Docker). Use DOCKER_SOCKET to deploy for real. */
	dockerSocket?: string;
	/** --allow-loopback-registry: image references may name the local registry (127.0.0.1:5000), as in the integration tests. */
	allowLoopbackRegistry?: boolean;
	/** --app-subnet. Default with a real Docker socket: a random /24 in 10.233.0.0/16, so servers started side by side never share a subnet. */
	appSubnet?: string;
};

/**
 * Starts the real binary (-tags spa) on free high ports with a temp data directory.
 *
 * The server creates its Docker app network at start (network_ensure), and every
 * test server has a fresh data directory, so a fresh install id and network name.
 * Against the machine's real Docker that would create (and leak) a bridge network
 * per server, and a changed host interface makes Chromium abort navigations in
 * flight (net::ERR_NETWORK_CHANGED). So the server gets a Docker socket that does
 * not exist, unless a test opts in with `dockerSocket`.
 */
export async function startServer(fixed?: ServerOptions): Promise<TestServer> {
	const bin = resolve(process.env.SERVERDOCK_BIN ?? '../bin/serverdock');
	const v = spawnSync(bin, ['version'], { encoding: 'utf8' });
	if (v.status !== 0 || !v.stdout.includes('ui=spa')) {
		throw new Error(`${bin} is missing or not built with -tags spa (version said: ${v.stdout || v.stderr}). Run 'make web build' first.`);
	}
	const https = fixed?.https ?? (await freePort());
	const http = await freePort();
	const dir = mkdtempSync(join(tmpdir(), 'sdpw-'));
	const data = join(dir, 'data');
	const baseURL = `https://127.0.0.1:${https}`;
	const realDocker = fixed?.dockerSocket !== undefined && !fixed.dockerSocket.endsWith('no-docker.sock');
	const subnet = fixed?.appSubnet ?? (realDocker ? `10.233.${1 + Math.floor(Math.random() * 250)}.0/24` : undefined);
	const child: ChildProcess = spawn(
		bin,
		[
			'server',
			'--data',
			data,
			'--control-socket',
			join(dir, 'c.sock'),
			'--listen',
			`127.0.0.1:${http}`,
			'--https-listen',
			`127.0.0.1:${https}`,
			'--docker-socket',
			fixed?.dockerSocket ?? join(dir, 'no-docker.sock'),
			...(fixed?.allowLoopbackRegistry ? ['--allow-loopback-registry'] : []),
			...(subnet ? ['--app-subnet', subnet] : [])
		],
		{ stdio: ['ignore', 'ignore', 'pipe'] }
	);
	let log = '';
	child.stderr?.on('data', (d) => (log += d));
	const api = await request.newContext({ baseURL, ignoreHTTPSErrors: true, extraHTTPHeaders: { Origin: baseURL } });
	const stop = async () => {
		await api.dispose();
		child.kill('SIGTERM');
		await new Promise((r) => (child.exitCode !== null ? r(null) : child.once('exit', r)));
		if (realDocker) removeDockerLeftovers(join(data, 'serverdock.db'), fixed!.dockerSocket!);
		rmSync(dir, { recursive: true, force: true });
	};
	let ready = false;
	for (let i = 0; i < 160 && !ready; i++) {
		try {
			ready = (await api.get('/api/v1/setup/status')).ok();
		} catch {
			await new Promise((r) => setTimeout(r, 250));
		}
	}
	if (!ready) {
		await stop();
		throw new Error('the server did not come up:\n' + log.slice(-2000));
	}
	const tokenFile = join(data, 'setup-token');
	return {
		baseURL,
		dataDir: data,
		tokenFile,
		token: () => readFileSync(tokenFile, 'utf8').trim(),
		api,
		sql(statement, ...params) {
			const db = new DatabaseSync(join(data, 'serverdock.db'));
			try {
				db.exec('PRAGMA busy_timeout = 5000');
				db.prepare(statement).run(...params);
			} finally {
				db.close();
			}
		},
		query<T>(statement: string, ...params: (string | number | null)[]) {
			const db = new DatabaseSync(join(data, 'serverdock.db'), { readOnly: true });
			try {
				db.exec('PRAGMA busy_timeout = 5000');
				return db.prepare(statement).all(...params) as T[];
			} finally {
				db.close();
			}
		},
		installId() {
			const db = new DatabaseSync(join(data, 'serverdock.db'), { readOnly: true });
			try {
				return (db.prepare("SELECT value FROM meta WHERE key = 'install_id'").get() as { value: string }).value;
			} finally {
				db.close();
			}
		},
		async firstRun() {
			const token = readFileSync(tokenFile, 'utf8').trim();
			const enroll = await api.post('/api/v1/setup/enroll', { data: { token, username: 'admin' } });
			if (!enroll.ok()) throw new Error(`setup/enroll ${enroll.status()}: ${await enroll.text()}`);
			const { enrollment_id, secret } = await enroll.json();
			const done = await api.post('/api/v1/setup/complete', {
				data: { token, enrollment_id, username: 'admin', password: PASSWORD, code: totp(secret) }
			});
			if (!done.ok()) throw new Error(`setup/complete ${done.status()}: ${await done.text()}`);
			const body = await done.json();
			return { secret, recoveryCodes: body.recovery_codes as string[], cookies: '', username: 'admin' };
		},
		stop
	};
}

/** Signs in through the API with a recovery code (they never collide with TOTP counters) and returns the cookies. */
export async function signInWithRecoveryCode(server: TestServer, code: string) {
	const ctx = await request.newContext({ baseURL: server.baseURL, ignoreHTTPSErrors: true, extraHTTPHeaders: { Origin: server.baseURL } });
	const login = await ctx.post('/api/v1/auth/login', { data: { username: 'admin', password: PASSWORD } });
	if (!login.ok()) throw new Error(`login ${login.status()}: ${await login.text()}`);
	const second = await ctx.post('/api/v1/auth/2fa', { data: { recovery_code: code } });
	if (!second.ok()) throw new Error(`2fa ${second.status()}: ${await second.text()}`);
	const state = await ctx.storageState();
	await ctx.dispose();
	return state;
}

/**
 * Removes what a real-Docker test server left behind: its containers (by the
 * serverdock.instance label) and its app network (serverdock-<first 8 of the id>).
 * A server's data directory is fresh, so these never belong to anything else.
 */
function removeDockerLeftovers(dbPath: string, socket: string) {
	let id = '';
	try {
		const db = new DatabaseSync(dbPath, { readOnly: true });
		try {
			id = (db.prepare("SELECT value FROM meta WHERE key = 'install_id'").get() as { value: string } | undefined)?.value ?? '';
		} finally {
			db.close();
		}
	} catch {
		return;
	}
	if (!/^[0-9a-f]{32}$/.test(id)) return;
	const docker = (...args: string[]) => spawnSync('docker', ['-H', `unix://${socket}`, ...args], { encoding: 'utf8' });
	for (const c of docker('ps', '-aq', '--filter', `label=serverdock.instance=${id}`).stdout.split(/\s+/).filter(Boolean)) docker('rm', '-f', '-v', c);
	docker('network', 'rm', `serverdock-${id.slice(0, 8)}`);
}

/**
 * A project name no other test server uses. Container names are Docker-wide
 * (sd-<project>-<service>-d<deploy id>-a<attempt>), and the projects run servers side
 * by side against one Docker, so tests that deploy for real must not share "default".
 */
export const uniqueProject = () => `pw${Date.now().toString(36)}${Math.floor(Math.random() * 1296).toString(36)}`;
