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
	sql: (statement: string, ...params: (string | number)[]) => void;
	stop: () => Promise<void>;
};

/** Starts the real binary (-tags spa) on free high ports with a temp data directory. */
export async function startServer(fixed?: { https: number }): Promise<TestServer> {
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
	const child: ChildProcess = spawn(
		bin,
		['server', '--data', data, '--control-socket', join(dir, 'c.sock'), '--listen', `127.0.0.1:${http}`, '--https-listen', `127.0.0.1:${https}`],
		{ stdio: ['ignore', 'ignore', 'pipe'] }
	);
	let log = '';
	child.stderr?.on('data', (d) => (log += d));
	const api = await request.newContext({ baseURL, ignoreHTTPSErrors: true, extraHTTPHeaders: { Origin: baseURL } });
	const stop = async () => {
		await api.dispose();
		child.kill('SIGTERM');
		await new Promise((r) => (child.exitCode !== null ? r(null) : child.once('exit', r)));
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
