// The footer status: system.info polled every 15 s while the page is visible. It is
// never a live region. When a poll fails, or the last good one is older than 45 s,
// or the page was hidden (the poll paused), it reads "unknown" until the first
// fresh poll (A12): stale numbers are never shown as current.
import { systemInfo, type SystemInfo } from './api';

export const POLL_MS = 15_000;
export const STALE_MS = 45_000;
const TIMEOUT_MS = 10_000;

export type Status =
	| { state: 'unknown' }
	| { state: 'ok'; version: string; rssMB: number; cpuPercent: number | null };

export type StatusPoller = { stop: () => void };

/** Starts polling; `set` receives every state change. Call stop() on teardown. */
export function startStatusPolling(set: (s: Status) => void, onSignedOut: () => void): StatusPoller {
	let timer: ReturnType<typeof setInterval> | undefined;
	let staleTimer: ReturnType<typeof setTimeout> | undefined;
	let prev: { cpu: number; at: number } | undefined;
	let ctrl: AbortController | undefined;
	let stopped = false;

	const unknown = () => {
		prev = undefined;
		set({ state: 'unknown' });
	};

	async function poll() {
		ctrl?.abort();
		const c = (ctrl = new AbortController());
		const t = setTimeout(() => c.abort(), TIMEOUT_MS);
		try {
			const info: SystemInfo = await systemInfo(c.signal);
			if (stopped || c !== ctrl) return;
			const now = Date.now();
			let cpu: number | null = null;
			if (prev && now - prev.at >= 1000) {
				cpu = Math.max(0, ((info.cpu_seconds - prev.cpu) / ((now - prev.at) / 1000)) * 100);
			}
			prev = { cpu: info.cpu_seconds, at: now };
			set({ state: 'ok', version: info.version, rssMB: info.rss_bytes / 1048576, cpuPercent: cpu });
			clearTimeout(staleTimer);
			staleTimer = setTimeout(unknown, STALE_MS);
		} catch (e) {
			if (stopped || c !== ctrl) return;
			clearTimeout(staleTimer);
			unknown();
			if (e instanceof Error && 'signedOut' in e && (e as { signedOut: boolean }).signedOut) onSignedOut();
		} finally {
			clearTimeout(t);
		}
	}

	function start() {
		if (timer !== undefined || stopped) return;
		void poll();
		timer = setInterval(poll, POLL_MS);
	}
	function pause() {
		if (timer !== undefined) clearInterval(timer);
		timer = undefined;
		ctrl?.abort();
		clearTimeout(staleTimer);
		unknown(); // until the first fresh poll after the page is visible again
	}
	function onVisibility() {
		if (document.visibilityState === 'visible') start();
		else pause();
	}

	set({ state: 'unknown' });
	document.addEventListener('visibilitychange', onVisibility);
	if (document.visibilityState === 'visible') start();

	return {
		stop() {
			stopped = true;
			document.removeEventListener('visibilitychange', onVisibility);
			if (timer !== undefined) clearInterval(timer);
			clearTimeout(staleTimer);
			ctrl?.abort();
		}
	};
}
