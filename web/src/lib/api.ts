// The client API layer: same-origin fetch, JSON, the CSRF header on unsafe
// methods, the error envelope as typed errors. A 401 means the session is gone
// (the caller shows the Signed-out screen); a 403 "mfa_required" is surfaced as a
// typed error for the step-up dialog (step 13). Nothing here logs request bodies.

export type SystemInfo = {
	version: string;
	uptime_seconds: number;
	rss_bytes: number;
	cpu_seconds: number;
	goroutines: number;
	audit_head: string;
	audit_rows: number;
};

export type Session = { user: { id: number; username: string }; csrf: string; public_id: string };

export const NETWORK_MESSAGE = 'Could not reach the server. Nothing was changed.';

export class ApiError extends Error {
	constructor(
		readonly status: number,
		readonly code: string,
		message: string,
		readonly retryAfter?: number,
		readonly triesLeft?: number
	) {
		super(message);
		this.name = 'ApiError';
	}
	get signedOut(): boolean {
		return this.status === 401;
	}
	get needsStepUp(): boolean {
		return this.status === 403 && this.code === 'mfa_required';
	}
}

/** The Signed-out screen's reason for a 401 error code (anything unknown is "ended"). */
export function signedOutReason(code: string | undefined): string {
	switch (code) {
		case 'idle_expired':
			return 'idle';
		case 'absolute_expired':
			return 'expired';
		case 'revoked':
			return 'revoked';
		default:
			return 'ended';
	}
}

let csrf = '';

type Options = { signal?: AbortSignal; body?: unknown };

async function call<T>(method: string, path: string, opts: Options = {}): Promise<T> {
	const headers: Record<string, string> = { Accept: 'application/json' };
	if (method !== 'GET') {
		headers['Content-Type'] = 'application/json';
		if (csrf) headers['X-SD-CSRF'] = csrf;
	}
	let res: Response;
	try {
		res = await fetch(path, {
			method,
			headers,
			credentials: 'same-origin',
			signal: opts.signal,
			body: method === 'GET' ? undefined : JSON.stringify(opts.body ?? {})
		});
	} catch (e) {
		if (e instanceof DOMException && e.name === 'AbortError') throw e;
		throw new ApiError(0, 'network', NETWORK_MESSAGE);
	}
	if (!res.ok) {
		let code = 'error';
		let message = res.statusText;
		let triesLeft: number | undefined;
		try {
			const env = (await res.json()) as { error?: { code?: string; message?: string }; tries_left?: number };
			code = env.error?.code ?? code;
			message = env.error?.message ?? message;
			triesLeft = typeof env.tries_left === 'number' ? env.tries_left : undefined;
		} catch {
			/* not JSON: keep the status text */
		}
		const ra = Number(res.headers.get('Retry-After'));
		throw new ApiError(res.status, code, message, Number.isFinite(ra) && ra > 0 ? ra : undefined, triesLeft);
	}
	return (await res.json()) as T;
}

/** The signed-in session; also learns the CSRF token. Throws ApiError (401 = signed out). */
export async function getSession(): Promise<Session> {
	const s = await call<Session>('GET', '/api/v1/auth/session');
	csrf = s.csrf;
	return s;
}

/** Runs a registered action (POST /api/v1/actions/{name}). */
export async function runAction<T>(name: string, input: object = {}, signal?: AbortSignal): Promise<T> {
	const res = await call<{ data: T }>('POST', `/api/v1/actions/${encodeURIComponent(name)}`, { body: input, signal });
	return res.data;
}

export const systemInfo = (signal?: AbortSignal) => runAction<SystemInfo>('system.info', {}, signal);

// ---- first run and sign-in (public routes) ------------------------------------

export type SetupStatus = {
	setup_required: boolean;
	server_time: string;
	token_file?: string;
	token_expires_at?: string;
};
export type Enrollment = { enrollment_id: string; secret: string; otpauth_uri: string };
export type Notice = { totp_failures?: number; last_failure_at?: number; last_failure_ip?: string };
export type SignedIn = { username: string; csrf: string; notice?: Notice; audit?: string; recovery_codes?: string[] };

export const setupStatus = () => call<SetupStatus>('GET', '/api/v1/setup/status');

export const setupEnroll = (token: string, username: string) =>
	call<Enrollment>('POST', '/api/v1/setup/enroll', { body: { token, username } });

export async function setupComplete(body: { token: string; enrollment_id: string; username: string; password: string; code: string }) {
	const r = await call<SignedIn>('POST', '/api/v1/setup/complete', { body });
	csrf = r.csrf;
	return r;
}

export const login = (username: string, password: string) =>
	call<{ two_factor_required: boolean; expires_in: number }>('POST', '/api/v1/auth/login', { body: { username, password } });

export async function twoFactor(body: { code: string } | { recovery_code: string }) {
	const r = await call<SignedIn>('POST', '/api/v1/auth/2fa', { body });
	csrf = r.csrf;
	return r;
}
