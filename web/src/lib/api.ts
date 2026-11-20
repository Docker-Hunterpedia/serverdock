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
		readonly retryAfter?: number
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
		try {
			const env = (await res.json()) as { error?: { code?: string; message?: string } };
			code = env.error?.code ?? code;
			message = env.error?.message ?? message;
		} catch {
			/* not JSON: keep the status text */
		}
		const ra = Number(res.headers.get('Retry-After'));
		throw new ApiError(res.status, code, message, Number.isFinite(ra) && ra > 0 ? ra : undefined);
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
