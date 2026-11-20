import { request, type FullConfig } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { BASE_URL, HTTPS_PORT, STORAGE } from '../playwright.config';

const HTTP_PORT = Number(process.env.E2E_HTTP_PORT ?? HTTPS_PORT + 1);
export const PASSWORD = 'correct horse battery staple 42';

function base32(s: string): Buffer {
	const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
	let bits = '';
	for (const ch of s.replace(/[\s=]/g, '').toUpperCase()) bits += alphabet.indexOf(ch).toString(2).padStart(5, '0');
	const out: number[] = [];
	for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
	return Buffer.from(out);
}

function totp(secret: string, at = Date.now()): string {
	const counter = Buffer.alloc(8);
	counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)));
	const h = createHmac('sha1', base32(secret)).update(counter).digest();
	const o = h[h.length - 1] & 0xf;
	const n = ((h[o] & 0x7f) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3];
	return (n % 1_000_000).toString().padStart(6, '0');
}

export default async function globalSetup(_config: FullConfig) {
	const bin = resolve(process.env.SERVERDOCK_BIN ?? '../bin/serverdock');
	const v = spawnSync(bin, ['version'], { encoding: 'utf8' });
	if (v.status !== 0 || !v.stdout.includes('ui=spa')) {
		throw new Error(`${bin} is missing or not built with -tags spa (version said: ${v.stdout || v.stderr}). Run 'make web build' first.`);
	}

	const dir = mkdtempSync(join(tmpdir(), 'sdpw-'));
	const data = join(dir, 'data');
	const child: ChildProcess = spawn(
		bin,
		['server', '--data', data, '--control-socket', join(dir, 'c.sock'), '--listen', `127.0.0.1:${HTTP_PORT}`, '--https-listen', `127.0.0.1:${HTTPS_PORT}`],
		{ stdio: ['ignore', 'ignore', 'pipe'] }
	);
	let log = '';
	child.stderr?.on('data', (d) => (log += d));
	const teardown = async () => {
		child.kill('SIGTERM');
		await new Promise((r) => setTimeout(r, 300));
		rmSync(dir, { recursive: true, force: true });
	};

	const api = await request.newContext({ baseURL: BASE_URL, ignoreHTTPSErrors: true, extraHTTPHeaders: { Origin: BASE_URL } });
	try {
		let ready = false;
		for (let i = 0; i < 120 && !ready; i++) {
			try {
				ready = (await api.get('/api/v1/setup/status')).ok();
			} catch {
				await new Promise((r) => setTimeout(r, 250));
			}
		}
		if (!ready) throw new Error('the server did not come up:\n' + log.slice(-2000));

		const token = readFileSync(join(data, 'setup-token'), 'utf8').trim();
		const enroll = await api.post('/api/v1/setup/enroll', { data: { token, username: 'admin' } });
		if (!enroll.ok()) throw new Error(`setup/enroll ${enroll.status()}: ${await enroll.text()}`);
		const { enrollment_id, secret } = await enroll.json();
		const done = await api.post('/api/v1/setup/complete', {
			data: { token, enrollment_id, username: 'admin', password: PASSWORD, code: totp(secret) }
		});
		if (!done.ok()) throw new Error(`setup/complete ${done.status()}: ${await done.text()}`);
		await api.storageState({ path: STORAGE }); // the session cookie of the new admin
	} catch (e) {
		await teardown();
		throw e;
	} finally {
		await api.dispose();
	}
	return teardown;
}
