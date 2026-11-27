// What the service detail pages share: the loaded service and its deploys, and the
// page's one error and status message. The detail layout creates it and puts it in
// context; the Deploys and Settings sub-pages read it. Loaded on navigation and by
// Refresh; nothing here polls and nothing runs on a timer.
import { goto } from '$app/navigation';
import { getContext, setContext, tick } from 'svelte';
import { ApiError, Cancelled, signedOutReason } from './api';
import { listDeploys, getService, loadErrorMessage, mutationMessage, type DeployItem, type Service } from './services';

const KEY = Symbol('service-detail');

export class ServiceCtx {
	project = $state('');
	name = $state('');
	service: Service | undefined = $state();
	deploys: DeployItem[] | undefined = $state();
	loading = $state(true);
	/** The load failed: the page says "status unknown" instead of stale data. */
	loadError: string | null = $state(null);
	notFound = $state(false);
	/** The page's alert and status text (actions report here; focus goes to the alert on a failure). */
	error: string | null = $state(null);
	links: { id: string; text: string }[] = $state([]);
	status = $state('');
	/** Set by the layout: moves focus to the alert region. */
	focusAlert: (() => void) | undefined;
	private ctrl: AbortController | undefined;

	get ref() {
		return `${this.project}/${this.name}`;
	}
	/** The live deploy, from the history. */
	get live(): DeployItem | undefined {
		return this.deploys?.find((d) => d.active);
	}

	async load(announce = false) {
		this.ctrl?.abort();
		const c = (this.ctrl = new AbortController());
		const ref = this.ref;
		this.loading = true;
		this.loadError = null;
		this.notFound = false;
		if (announce) this.status = 'Refreshing…';
		try {
			const [s, d] = await Promise.all([getService(ref, c.signal), listDeploys(ref, c.signal)]);
			if (c !== this.ctrl) return;
			this.service = s;
			this.deploys = d;
			if (announce) this.status = 'Updated.';
		} catch (e) {
			if (c !== this.ctrl) return;
			this.service = undefined;
			this.deploys = undefined;
			this.status = '';
			if (e instanceof ApiError && e.status === 404) {
				this.notFound = true;
				return;
			}
			const msg = await loadErrorMessage(e);
			if (msg === undefined) return;
			this.loadError = msg;
			this.error = msg;
			await tick();
			this.focusAlert?.();
		} finally {
			if (c === this.ctrl) this.loading = false;
		}
	}

	abort() {
		this.ctrl?.abort();
	}

	say(message: string) {
		this.error = null;
		this.links = [];
		this.status = message;
	}

	/** Shows a failure in the alert region; a 401 goes to the Signed-out screen. */
	async fail(e: unknown, links: { id: string; text: string }[] = []) {
		if (e instanceof Cancelled) {
			this.say('Cancelled. Nothing was changed.');
			return;
		}
		if (!(e instanceof ApiError)) throw e;
		if (e.status === 401) {
			await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
			return;
		}
		await this.alert(mutationMessage(e), links);
	}

	async alert(message: string, links: { id: string; text: string }[] = []) {
		this.status = '';
		this.error = message;
		this.links = links;
		await tick();
		this.focusAlert?.();
	}

	clearMessages() {
		this.error = null;
		this.links = [];
		this.status = '';
	}
}

export const provideService = (c: ServiceCtx) => setContext(KEY, c);
export const useService = () => getContext<ServiceCtx>(KEY);

/** The sentence after a deploy, redeploy or rollback was queued (it is queued, not done). */
export function queuedMessage(what: string, q: { deploy: number; superseded?: number }): string {
	return `${what} queued as deploy #${q.deploy}.${q.superseded ? ` It replaced queued deploy #${q.superseded}.` : ''} It runs in the background: use Refresh to see how it goes.`;
}
