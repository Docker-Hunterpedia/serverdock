// The Deploy detail page's state: one deploy, loaded, and polled while it runs and the
// page is visible (plan 0004 step 14). At most ONE timer exists: the wait before the
// next poll. It is cleared when the deploy finishes, when the page is hidden and when the
// page is destroyed; a request in flight is simply ignored then (a generation counter).
// A poll never moves focus; what it announces is decided by announcementFor.
import { goto } from '$app/navigation';
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

export class DeployWatch {
	ref = '';
	number = 0;
	data: DeployGet | undefined = $state();
	loading = $state(true);
	notFound = $state(false);
	/** The last poll failed or the data is older than 10 s: the page says "Status unknown" instead of the stale state. */
	unknown = $state(false);
	/** The role="status" text (a stage transition or success) and the role="alert" text (a failure). */
	status = $state('');
	alert: string | null = $state(null);
	/** The number of the deploy that was live before this one, for "#M kept serving". */
	previous: number | undefined = $state();
	log: LogState = $state({ kind: 'none' });
	/** A cancel was accepted for a running deploy: it stops at its next safe point. */
	cancelAsked = $state(false);

	private timer: ReturnType<typeof setTimeout> | undefined;
	private gen = 0;
	private fetchedAt = 0;
	private pullSince: number | undefined;
	private opened = 0;
	private visible = true;
	private destroyed = false;

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
		this.status = '';
		this.alert = null;
		this.previous = undefined;
		this.log = { kind: 'none' };
		this.cancelAsked = false;
		this.pullSince = undefined;
		this.fetchedAt = 0;
		this.destroyed = false;
		this.visible = typeof document === 'undefined' || document.visibilityState !== 'hidden';
		void this.refresh();
	}

	destroy() {
		this.destroyed = true;
		this.gen++;
		this.clear();
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
		if (this.ref) void this.refresh();
	}

	private clear() {
		if (this.timer !== undefined) clearTimeout(this.timer);
		this.timer = undefined;
	}

	/** Asks now (the first load, a visible page again, "Check again", after an action). */
	async refresh() {
		this.clear();
		const gen = ++this.gen;
		const ref = this.ref;
		const number = this.number;
		try {
			const d = await getDeploy(ref, number, AbortSignal.timeout(REQUEST_TIMEOUT_MS));
			if (gen !== this.gen) return;
			this.apply(d);
		} catch (e) {
			if (gen !== this.gen) return;
			this.loading = false;
			if (e instanceof ApiError && e.status === 404) {
				this.notFound = true;
				this.data = undefined;
				return;
			}
			if (e instanceof ApiError && e.status === 401) {
				await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
				return;
			}
			if (!(e instanceof ApiError) && !(e instanceof DOMException)) throw e;
			this.unknown = true;
		}
		if (gen === this.gen) this.schedule();
	}

	private apply(d: DeployGet) {
		const prev = this.data;
		const wasOpen = prev ? isUnfinished(prev.deploy.status) : undefined;
		this.fetchedAt = Date.now();
		this.unknown = false;
		this.loading = false;
		// "Once a pull has run for 2 minutes": counted from when this page first saw the pull stage.
		if (currentStage(d) === 'pull' && isUnfinished(d.deploy.status)) this.pullSince ??= Date.now();
		else this.pullSince = undefined;
		this.data = d;
		const finished = !isUnfinished(d.deploy.status);
		if (finished && wasOpen !== false) {
			// What "kept serving" names, and the log: asked once, when the deploy has finished (never by the timer).
			void this.loadExtras(d);
		}
		const say = announcementFor(prev, d, this.previous);
		if (say.alert) {
			this.status = '';
			this.alert = say.alert;
		} else if (say.status) {
			this.status = say.status;
		}
		if (!finished) this.cancelAsked = this.cancelAsked && !d.cancellable;
	}

	private async loadExtras(d: DeployGet) {
		const opened = this.opened;
		if (d.deploy.status === 'failed' && d.deploy.app_safe === 'yes') {
			try {
				const list = await listDeploys(this.ref);
				if (opened === this.opened) this.previous = previousLive(list, d.deploy.number);
			} catch {
				/* the sentence then says "the previous version" */
			}
		}
		await this.loadLog();
	}

	/** Fetches the deploy log (also the "Try again" of the 503). */
	async loadLog() {
		const opened = this.opened;
		this.log = { kind: 'loading' };
		try {
			const out = await getDeployLog(this.ref, this.number);
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
			const msg = await loadErrorMessage(e);
			this.log = { kind: 'error', message: msg ?? 'The log could not be loaded.' };
		}
	}

	/** One timer, only while the deploy is unfinished (or unknown) and the page is visible. */
	private schedule() {
		this.clear();
		if (this.destroyed || !this.visible || this.notFound) return;
		if (this.data && !isUnfinished(this.data.deploy.status)) return;
		this.timer = setTimeout(() => {
			this.timer = undefined;
			// A late timer (a throttled tab, a sleeping laptop) must not leave old data looking current.
			if (this.fetchedAt && Date.now() - this.fetchedAt > STALE_AFTER_MS) this.unknown = true;
			void this.refresh();
		}, pollDelay(this.pullSince, Date.now()));
	}
}
