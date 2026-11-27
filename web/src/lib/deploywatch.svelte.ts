// The Deploy detail page's state: one deploy, loaded, and polled while it runs and the
// page is visible (plan 0004 step 14). At most ONE timer exists: the wait before the
// next poll. It is cleared when the deploy finishes, when the page is hidden and when the
// page is destroyed; a request in flight is simply ignored then (a generation counter).
// A poll never moves focus (the one exception: when the control that had focus is
// removed by the change, focus goes to the heading); what it announces is decided by
// announcementFor, plus one line when the status becomes unknown and one when it is
// current again.
import { goto } from '$app/navigation';
import { tick } from 'svelte';
import { ApiError, signedOutReason } from './api';
import {
	STALE_AFTER_MS,
	announcementFor,
	currentStage,
	getDeploy,
	getDeployLog,
	isUnfinished,
	listDeploys,
	loadErrorMessage,
	pollDelay,
	previousLive,
	type DeployGet,
	type DeployLog
} from './services';

/** The deploy log as the page shows it: fetched once, when the deploy has finished. */
export type LogState =
	| { kind: 'none' }
	| { kind: 'loading' }
	| { kind: 'text'; log: DeployLog }
	| { kind: 'message'; message: string }
	| { kind: 'busy'; message: string }
	| { kind: 'error'; message: string };

const REQUEST_TIMEOUT_MS = 8000;
/** After consecutive failures the page waits 2, 4, 8, ... seconds, at most this long. */
export const POLL_MAX_BACKOFF_MS = 30_000;
export const UNKNOWN_SAID = 'Status unknown: could not reach the server.';

export class DeployWatch {
	ref = '';
	number = 0;
	data: DeployGet | undefined = $state();
	loading = $state(true);
	notFound = $state(false);
	/** The last poll failed or the data is older than 10 s: the page marks what it shows as possibly out of date. */
	unknown = $state(false);
	/** The server refused this page (403): polling stopped, this is its message. */
	refused: string | null = $state(null);
	/** The role="status" text (a stage transition or success) and the role="alert" text (a failure). */
	status = $state('');
	alert: string | null = $state(null);
	/** The number of the deploy that was live before this one, for "#M kept serving". */
	previous: number | undefined = $state();
	log: LogState = $state({ kind: 'none' });
	/** A cancel was accepted for a running deploy: it stops at its next safe point. */
	cancelAsked = $state(false);
	/** Set by the page: puts focus on the heading when the focused control was removed. */
	focusHeading: (() => void) | undefined;

	private timer: ReturnType<typeof setTimeout> | undefined;
	private gen = 0;
	private fetchedAt = 0;
	private pullSince: number | undefined;
	private opened = 0;
	private visible = true;
	private destroyed = false;
	private errors = 0;
	/** "Status unknown" was spoken (so "current again" is too). */
	private unknownSaid = false;

	/** Opens another deploy (also the first one): everything of the previous one is dropped. */
	open(ref: string, number: number) {
		this.clear();
		this.gen++;
		this.opened++;
		this.ref = ref;
		this.number = number;
		this.data = undefined;
		this.loading = true;
		this.notFound = false;
		this.unknown = false;
		this.refused = null;
		this.status = '';
		this.alert = null;
		this.previous = undefined;
		this.log = { kind: 'none' };
		this.cancelAsked = false;
		this.pullSince = undefined;
		this.fetchedAt = 0;
		this.errors = 0;
		this.unknownSaid = false;
		this.destroyed = false;
		this.visible = typeof document === 'undefined' || document.visibilityState !== 'hidden';
		void this.refresh();
	}

	destroy() {
		this.destroyed = true;
		this.gen++;
		this.clear();
	}

	/** Says a line in the status region; the same line twice in a row is still announced. */
	async announce(text: string) {
		if (this.status === text) {
			this.status = '';
			await tick();
		}
		this.status = text;
	}

	/** The page became hidden or visible. Hidden: no timer, no announcement. Visible: refresh first, announce after. */
	setVisible(visible: boolean) {
		if (visible === this.visible || this.destroyed) return;
		this.visible = visible;
		if (!visible) {
			this.clear();
			this.gen++; // a response that arrives while hidden is dropped
			return;
		}
		if (!this.ref) return;
		// Old data must not look current while the refresh runs (said only if the refresh fails).
		if (this.fetchedAt && Date.now() - this.fetchedAt > STALE_AFTER_MS) this.unknown = true;
		void this.refresh();
	}

	private clear() {
		if (this.timer !== undefined) clearTimeout(this.timer);
		this.timer = undefined;
	}

	/** Runs `change`; when the focused control is one that the change removes, focus goes to the heading. */
	async keepFocus(change: () => void) {
		const el = typeof document === 'undefined' ? null : document.activeElement;
		const inside = !!el && el !== document.body && !!el.closest('[data-vanishes]');
		change();
		if (!inside) return;
		await tick();
		if (!el.isConnected) this.focusHeading?.();
	}

	private setUnknown(on: boolean, say = true) {
		if (on === this.unknown && (!on || this.unknownSaid || !say)) return;
		this.unknown = on;
		if (on && say) {
			this.unknownSaid = true;
			void this.announce(UNKNOWN_SAID);
		}
	}

	/** Asks now (the first load, a visible page again, "Check again", after an action). Resolves true when the server answered. */
	async refresh(manual = false): Promise<boolean> {
		this.clear();
		const gen = ++this.gen;
		const ref = this.ref;
		const number = this.number;
		if (manual) void this.announce('Checking…');
		let ok = false;
		try {
			const d = await getDeploy(ref, number, AbortSignal.timeout(REQUEST_TIMEOUT_MS));
			if (gen !== this.gen) return false;
			await this.apply(d, manual);
			ok = true;
		} catch (e) {
			if (gen !== this.gen) return false;
			this.loading = false;
			if (e instanceof ApiError && e.status === 404) {
				this.notFound = true;
				this.data = undefined;
				return false;
			}
			if (e instanceof ApiError && e.status === 401) {
				await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
				return false;
			}
			if (e instanceof ApiError && e.status === 403) {
				// Not a connection problem: the server says no. Polling again would only repeat it.
				this.refused = e.message;
				this.alert = e.message;
				this.setUnknown(true, false);
				return false;
			}
			// Any other failure (no answer, a 5xx, an answer that is not JSON) is "unknown", and polling goes on.
			this.errors++;
			const wasSaid = this.unknownSaid;
			this.setUnknown(true);
			if (manual && wasSaid) void this.announce('Still could not reach the server.');
		}
		if (gen === this.gen) this.schedule();
		return ok;
	}

	private async apply(d: DeployGet, manual: boolean) {
		const prev = this.data;
		const wasOpen = prev ? isUnfinished(prev.deploy.status) : undefined;
		const wasUnknown = this.unknown;
		this.errors = 0;
		this.fetchedAt = Date.now();
		this.loading = false;
		// "Once a pull has run for 2 minutes": counted from when this page first saw the pull stage.
		if (currentStage(d) === 'pull' && isUnfinished(d.deploy.status)) this.pullSince ??= Date.now();
		else this.pullSince = undefined;
		await this.keepFocus(() => {
			this.unknown = false;
			this.refused = null;
			this.data = d;
		});
		if (wasUnknown && manual) {
			this.focusHeading?.();
		}
		const finished = !isUnfinished(d.deploy.status);
		if (finished && wasOpen !== false) {
			// What "kept serving" names, and the log: asked once, when the deploy has finished (never by the timer).
			void this.loadExtras(d);
		}
		const say = announcementFor(prev, d, undefined);
		if (say.alert) {
			this.status = '';
			this.alert = say.alert;
		} else if (say.status) {
			void this.announce(say.status);
		} else if (wasUnknown && this.unknownSaid) {
			void this.announce(manual ? 'Status updated.' : 'Status is current again.');
		}
		if (wasUnknown) this.unknownSaid = false;
		if (!finished) this.cancelAsked = this.cancelAsked && !d.cancellable;
	}

	private async loadExtras(d: DeployGet) {
		const opened = this.opened;
		if (d.deploy.status === 'failed' && d.deploy.app_safe === 'yes') {
			try {
				const list = await listDeploys(this.ref, AbortSignal.timeout(REQUEST_TIMEOUT_MS));
				if (opened === this.opened) this.previous = previousLive(list, d.deploy.number);
			} catch {
				/* the sentence then says "the previous version" */
			}
		}
		if (opened !== this.opened) return;
		await this.loadLog();
	}

	/** Fetches the deploy log (also the "Try again" of the 503). */
	async loadLog() {
		const opened = this.opened;
		await this.keepFocus(() => (this.log = { kind: 'loading' }));
		try {
			const out = await getDeployLog(this.ref, this.number, AbortSignal.timeout(REQUEST_TIMEOUT_MS));
			if (opened !== this.opened) return;
			this.log = out.kept && typeof out.text === 'string' ? { kind: 'text', log: out } : { kind: 'message', message: out.message || 'No log was kept for this deploy.' };
		} catch (e) {
			if (opened !== this.opened) return;
			if (e instanceof ApiError && e.status === 503) {
				const m = e.message.replace(/[.\s]+$/, '');
				this.log = { kind: 'busy', message: `The log could not be read yet: ${m}.` };
				return;
			}
			if (e instanceof ApiError && e.status === 401) {
				await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
				return;
			}
			const msg = await loadErrorMessage(e).catch(() => undefined);
			this.log = { kind: 'error', message: msg ?? 'The log could not be loaded.' };
		}
	}

	/** One timer, only while the deploy is unfinished (or unknown) and the page is visible. */
	private schedule() {
		this.clear();
		if (this.destroyed || !this.visible || this.notFound || this.refused) return;
		if (this.data && !isUnfinished(this.data.deploy.status)) return;
		const base = pollDelay(this.pullSince, Date.now());
		// Consecutive failures back off: 2, 4, 8, ... seconds, at most 30.
		const delay = this.errors > 0 ? Math.min(POLL_MAX_BACKOFF_MS, Math.max(base, 2000 * 2 ** (this.errors - 1))) : base;
		this.timer = setTimeout(() => {
			this.timer = undefined;
			// A late timer (a throttled tab, a sleeping laptop) must not leave old data looking current.
			if (this.fetchedAt && Date.now() - this.fetchedAt > STALE_AFTER_MS) this.setUnknown(true);
			void this.refresh();
		}, delay);
	}
}
