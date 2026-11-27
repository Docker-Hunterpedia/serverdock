// Services in the SPA: the types of services.list and system.exposure (see
// docs/api/openapi.json), the state words with their symbols (word plus symbol,
// never colour alone), and the helpers the Services list, New service and Overview share.
import { goto } from '$app/navigation';
import { ApiError, NETWORK_MESSAGE, runAction, signedOutReason } from './api';
import type { components } from './api-types';

export const DOCS_URL = 'https://github.com/Docker-Hunterpedia/serverdock/tree/main/docs';
export const SEC6_DOCS_URL = 'https://github.com/Docker-Hunterpedia/serverdock/blob/main/docs/security.md';

export type ServiceState = 'not_deployed' | 'running' | 'restarting' | 'exited' | 'missing' | 'unknown';

// The shapes come from the generated API types (docs/api/openapi.json); only the
// state word is narrowed to the words the API documents.
type Schemas = components['schemas'];
type ServiceSchema = Schemas['ServicesListOutput']['services'][number];
export type Service = Omit<ServiceSchema, 'state'> & { state: ServiceState };
export type LastDeploy = NonNullable<ServiceSchema['last_deploy']>;
export type Warning = ServiceSchema['warnings'][number];
export type Exposure = Schemas['SystemExposureOutput'];
export type ExposedContainer = Exposure['containers'][number];
export type CreateInput = Schemas['ServicesCreateInput'];
export type CreateOutput = Omit<Schemas['ServicesCreateOutput'], 'service'> & { service: Service };

export const listServices = (signal?: AbortSignal) => runAction<{ services: Service[] }>('services.list', {}, signal).then((r) => r.services);
export const listProjects = (signal?: AbortSignal) => runAction<{ projects: { name: string }[] }>('projects.list', {}, signal).then((r) => r.projects.map((p) => p.name));
export const getExposure = (signal?: AbortSignal) => runAction<Exposure>('system.exposure', {}, signal);

const STATES: Record<ServiceState, { symbol: string; word: string }> = {
	running: { symbol: '●', word: 'Running' },
	restarting: { symbol: '▲', word: 'Restarting' },
	exited: { symbol: '▲', word: 'Exited' },
	missing: { symbol: '◆', word: 'Missing' },
	unknown: { symbol: '◌', word: 'Unknown' },
	not_deployed: { symbol: '○', word: 'Not deployed' }
};
export const STATE_ORDER: ServiceState[] = ['running', 'restarting', 'exited', 'missing', 'unknown', 'not_deployed'];
export function stateInfo(state: string): { symbol: string; word: string } {
	return STATES[state as ServiceState] ?? STATES.unknown;
}

const DEPLOY_STATUS: Record<string, { symbol: string; word: string }> = {
	queued: { symbol: '○', word: 'Queued' },
	running: { symbol: '…', word: 'Running' },
	succeeded: { symbol: '✓', word: 'Succeeded' },
	failed: { symbol: '▲', word: 'Failed' },
	cancelled: { symbol: '⊘', word: 'Cancelled' },
	superseded: { symbol: '⊘', word: 'Superseded' }
};
export function deployStatusInfo(status: string): { symbol: string; word: string } {
	return DEPLOY_STATUS[status] ?? { symbol: '◌', word: 'Unknown' };
}

/** The Deploy detail route (step 14). */
export const deployPath = (project: string, name: string, number: number) => `/services/${encodeURIComponent(project)}/${encodeURIComponent(name)}/deploys/${number}`;
export const servicePath = (s: { project: string; name: string }) => `/services/${encodeURIComponent(s.project)}/${encodeURIComponent(s.name)}`;

/** The host the panel was opened on (an IPv6 address keeps its brackets). */
export const panelHost = () => location.hostname;
/** Where an app with this public port is reached, from the address the panel was opened on. */
export const appAddress = (port: number) => `http://${panelHost()}:${port}`;

/**
 * The shares_panel_address warning: the server's text (it knows no host name) plus the
 * address the panel was opened on, which is what the user sees in the address bar.
 * The docs link is rendered next to this text by the pages.
 */
export function sharesPanelText(message: string, port: number | null): string {
	const where = port ? ` This panel was opened on ${panelHost()}, so the app is at ${appAddress(port)}.` : ` This panel was opened on ${panelHost()}.`;
	return message + where;
}

/** The same wording before a service exists (the server's text is built from the port; keep in step with SharesPanelAddressMessage). */
export function sharesPanelDraft(port: number | null): string {
	return sharesPanelText(
		`This app is reachable on the same address as the panel, on ${port ? `port ${port}` : 'the port you choose'}. Browsers share cookies by host name, not by port. ServerDock serves this app over plain HTTP, so the panel's secure cookies are never sent to it. Don't run any other HTTPS service on this address. Once domains arrive, give the panel and your apps their own names.`,
		port
	);
}

/** Common web images and the port they listen on: a suggestion only, never filled in silently. */
const KNOWN_PORTS: Record<string, number> = {
	nginx: 80,
	httpd: 80,
	caddy: 80,
	whoami: 80,
	wordpress: 80,
	grafana: 3000,
	ghost: 2368,
	'uptime-kuma': 3001
};
/** The suggested container port for an image reference, if it is a well-known image. */
export function suggestedPort(imageRef: string): { image: string; port: number } | undefined {
	const ref = imageRef.trim().split('@')[0];
	const last = ref.split('/').pop() ?? '';
	const image = last.split(':')[0];
	const port = KNOWN_PORTS[image];
	return port ? { image, port } : undefined;
}

/** What needs attention on a service, one entry per reason (the Overview list). */
export type Attention = { symbol: string; text: string };
export function attentionOf(s: Service): Attention[] {
	const out: Attention[] = [];
	const l = s.last_deploy;
	if (l && l.status === 'failed') {
		const safe = l.app_safe === 'yes' ? 'the previous version kept serving' : l.app_safe === 'no' ? 'the app is not safe' : 'whether the app is safe is unknown';
		out.push({ symbol: '▲', text: `Last deploy #${l.number} failed (${safe}).` });
	}
	if (s.state === 'missing') out.push({ symbol: '◆', text: 'The live version’s container is missing.' });
	if (s.state === 'exited') out.push({ symbol: '▲', text: 'The live version has exited.' });
	if (s.state === 'restarting') out.push({ symbol: '▲', text: 'The live version keeps restarting.' });
	if (s.state === 'unknown') out.push({ symbol: '◌', text: "ServerDock cannot confirm this service's state with Docker right now." });
	if (s.route_error) out.push({ symbol: '▲', text: `Route problem: ${s.route_error}` });
	switch (s.needs_attention) {
		case 'delete_interrupted':
			out.push({ symbol: '▲', text: 'A delete did not finish. Delete the service again to finish it.' });
			break;
		case 'versions_kept':
			out.push({ symbol: '▲', text: 'Two versions may be running after an unfinished stop-first deploy. ServerDock settles this by itself; try again once the service has settled.' });
			break;
		case 'delete_confirm_again':
			out.push({ symbol: '▲', text: 'A delete of this service needed a fresh confirmation and did not run. Nothing was deleted. Open the service and use Delete again.' });
			break;
		case 'previous_version_down':
			out.push({ symbol: '▲', text: 'A stop-first deploy failed and the previous version did not come back. Deploy again or roll back.' });
			break;
		case undefined:
		case '':
			break;
		default:
			out.push({ symbol: '▲', text: 'This service needs attention.' });
	}
	return out;
}

export const VERSIONS_KEPT_FRAGMENT = 'two versions of this service may be running';
/** A refused action's message; the versions_kept refusal gets the next step added. */
export function actionMessage(message: string): string {
	if (!message.includes(VERSIONS_KEPT_FRAGMENT)) return message;
	// The server ends with "try again afterwards": say it once, with the next step.
	return `${message.replace(/[;,.]?\s*try again afterwards\.?\s*$/i, '').replace(/[.\s]+$/, '')}. Try again once the service has settled.`;
}

/** Maps a load error to the page's message; a 401 goes to the Signed-out screen and returns undefined. */
export async function loadErrorMessage(e: unknown): Promise<string | undefined> {
	if (e instanceof DOMException && e.name === 'AbortError') return undefined;
	if (!(e instanceof ApiError)) throw e;
	if (e.status === 401) {
		await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
		return undefined;
	}
	return e.code === 'network' ? NETWORK_MESSAGE : e.message;
}

// ---- the service detail page (plan 0004 step 13) --------------------------------

export type DeployItem = Schemas['DeploysListOutput']['deploys'][number];
export type QueuedDeploy = Schemas['ServicesDeployOutput'];
export type ServiceChange = Omit<Schemas['ServicesSetPublicPortOutput'], 'service'> & { service: Service };

export const getService = (ref: string, signal?: AbortSignal) => runAction<Service>('services.get', { service: ref }, signal);
export const listDeploys = (ref: string, signal?: AbortSignal) => runAction<{ deploys: DeployItem[] }>('deploys.list', { service: ref }, signal).then((r) => r.deploys);

/** One result per deploy: word plus symbol, never colour alone. */
export function deployResult(d: DeployItem): { symbol: string; word: string } {
	switch (d.status) {
		case 'queued':
			return { symbol: '○', word: 'Queued' };
		case 'running':
			return { symbol: '…', word: 'Running' };
		case 'superseded':
			return { symbol: '⊘', word: d.superseded_by ? `Superseded by #${d.superseded_by}` : 'Superseded' };
		case 'cancelled':
			return { symbol: '⊘', word: 'Cancelled' };
		case 'failed':
			if (d.reason_code === 'interrupted') return { symbol: '◌', word: 'Interrupted' };
			if (d.app_safe === 'yes') return { symbol: '▲', word: 'Failed · app safe' };
			if (d.app_safe === 'no') return { symbol: '▲', word: 'Failed · app not safe' };
			return { symbol: '▲', word: 'Failed · app safety unknown' };
		case 'succeeded':
			return d.active ? { symbol: '●', word: 'Live' } : { symbol: '○', word: 'Replaced' };
		default:
			return { symbol: '◌', word: 'Unknown' };
	}
}

/** Only a deploy that was once live, and is not live now, can be rolled back to. */
export const canRollBackTo = (d: DeployItem) => d.was_live && !d.active;

const STAGES: Record<string, string> = {
	pull: 'pulling the image',
	check: 'checks',
	maintenance: 'maintenance page',
	stop_old_first: 'stopping the old version',
	start: 'starting the new version',
	health: 'health check',
	switch: 'switching traffic',
	commit: 'recording the switch',
	drain: 'draining the old version',
	stop_old: 'stopping the old version',
	done: 'done'
};
/** What a deploy got to, from its stage and status (the stage list itself is the Deploy page's). */
export function stagesSummary(d: DeployItem): string {
	const at = d.stage ? (STAGES[d.stage] ?? d.stage) : '';
	switch (d.status) {
		case 'succeeded':
			return 'All stages done';
		case 'queued':
			return 'Waiting to start';
		case 'running':
			return at ? `Now: ${at}` : 'Starting';
		default:
			return at ? `Stopped at: ${at}` : 'Stopped before the first stage';
	}
}

const TRIGGERS: Record<string, string> = { user: 'Signed-in user', token: 'API token', mcp: 'MCP client', webhook: 'Webhook', system: 'ServerDock itself' };
export const triggeredBy = (d: DeployItem) => TRIGGERS[d.triggered_by ?? ''] ?? 'Unknown';

export const KIND_WORDS: Record<string, string> = { deploy: 'Deploy', redeploy: 'Redeploy', rollback: 'Rollback' };

export function strategyWord(s: string): string {
	return s === 'stop_first' ? 'Stop-first' : 'Blue-green';
}
/** What a deploy does to the running app, by strategy. */
export function strategyConsequence(s: string): string {
	return s === 'stop_first'
		? 'This service is stop-first: the app is briefly down while the old version stops and the new one starts.'
		: 'This service is blue-green: the app keeps serving until the new version is healthy, then traffic switches.';
}

/** What a rollback does to the running app: it keeps the strategy the target deploy recorded, not the service's current one. */
export function rollbackConsequence(strategy: string, target: number): string {
	return strategy === 'stop_first'
		? `This rollback keeps the strategy deploy #${target} recorded, stop-first: the app is briefly down while the old version stops and the target starts.`
		: `This rollback keeps the strategy deploy #${target} recorded, blue-green: the app keeps serving until the target is healthy, then traffic switches.`;
}

/** The health check in one sentence. */
export function healthText(s: Service): string {
	const h = s.health;
	const tail = `Gives up after ${h.timeout_s} s; needs ${h.passes} passing ${h.passes === 1 ? 'check' : 'checks'} in a row.`;
	if (h.kind === 'http') return `HTTP: ${h.path || '/'} on port ${s.container_port ?? '?'} answers with success. ${tail}`;
	if (h.kind === 'tcp') return `TCP: port ${s.container_port ?? '?'} accepts connections. ${tail}`;
	return 'Stays running: no port check.';
}

/** The repository of an image reference (no tag, no digest), for the dialogs' wording. */
export function repositoryOf(ref: string): string {
	const noDigest = ref.split('@')[0];
	const slash = noDigest.lastIndexOf('/');
	const colon = noDigest.lastIndexOf(':');
	return colon > slash ? noDigest.slice(0, colon) : noDigest;
}

/** A whole number in [lo, hi] from a form field: undefined when empty, 'bad' when not valid. */
export function intField(v: string, lo: number, hi: number): number | 'bad' | undefined {
	const t = v.trim();
	if (t === '') return undefined;
	if (!/^\d{1,5}$/.test(t)) return 'bad';
	const n = Number(t);
	return n < lo || n > hi ? 'bad' : n;
}

/** The shape of services.update's input that the Settings form sends. */
export type UpdateInput = Schemas['ServicesUpdateInput'];

/** What a service-detail action failure says: a network failure is not a "nothing changed" claim. */
export function mutationMessage(e: ApiError): string {
	if (e.code === 'network') return 'Could not reach the server. The request may or may not have gone through: use Refresh to check before trying again.';
	return actionMessage(e.message);
}

// ---- the deploy detail page (plan 0004 step 14) ---------------------------------

export type DeployGet = Schemas['DeploysGetOutput'];
export type DeployLog = Schemas['DeploysLogOutput'];
export type StageItem = DeployGet['stages'][number];
export type CancelResult = Schemas['DeploysCancelOutput'];

export const getDeploy = (ref: string, number: number, signal?: AbortSignal) => runAction<DeployGet>('deploys.get', { service: ref, number }, signal);
export const getDeployLog = (ref: string, number: number, signal?: AbortSignal) => runAction<DeployLog>('deploys.log', { service: ref, number }, signal);
export const cancelDeploy = (ref: string, number: number) => runAction<CancelResult>('deploys.cancel', { service: ref, number });

/** Not finished yet: the page polls while this is true. */
export const isUnfinished = (status: string) => status === 'queued' || status === 'running';

/** How often the page asks while a deploy runs and is visible: 2 s, 5 s once a pull has run 2 minutes. */
export const POLL_FAST_MS = 2000;
export const POLL_SLOW_MS = 5000;
export const PULL_SLOW_AFTER_MS = 120_000;
/** Data older than this, or a failed poll, is shown as "Status unknown". */
export const STALE_AFTER_MS = 10_000;
export const pollDelay = (pullSince: number | undefined, now: number) => (pullSince !== undefined && now - pullSince >= PULL_SLOW_AFTER_MS ? POLL_SLOW_MS : POLL_FAST_MS);

const STAGE_LABELS: Record<string, string> = {
	pull: 'Pull the image',
	check: 'Checks',
	maintenance: 'Show the maintenance page',
	stop_old_first: 'Stop the old version first',
	start: 'Start the new version',
	health: 'Health check',
	switch: 'Switch traffic',
	commit: 'Record the switch',
	drain: 'Drain the old version',
	stop_old: 'Retire the old version'
};

/** The same stages in a sentence ("Deploy #4: health check started."): never an imperative. */
const STAGE_NOUNS: Record<string, string> = {
	pull: 'image pull',
	check: 'checks',
	maintenance: 'maintenance page',
	stop_old_first: 'first stop of the old version',
	start: 'start of the new version',
	health: 'health check',
	switch: 'traffic switch',
	commit: 'recording of the switch',
	drain: 'draining of the old version',
	stop_old: 'retirement of the old version'
};
export const stageNoun = (stage: string) => STAGE_NOUNS[stage] ?? stage;
export const stageLabel = (stage: string) => STAGE_LABELS[stage] ?? stage;

export const RETRY_PREFIX = 'waiting to retry';
/** A pull (or other stage) that failed with a transient error and waits for its next attempt. */
export const isRetrying = (s: StageItem) => s.state === 'pending' && (s.detail ?? '').startsWith(RETRY_PREFIX);

/** One stage as word plus symbol; a stage that never ran on a finished deploy is Skipped. */
export function stageView(s: StageItem, deployFinished: boolean): { symbol: string; word: string } {
	if (isRetrying(s) && !deployFinished) return { symbol: '○', word: 'Waiting to retry' };
	switch (s.state) {
		case 'ok':
			return { symbol: '✓', word: 'Done' };
		case 'failed':
			return { symbol: '✕', word: 'Failed' };
		case 'skipped':
			return { symbol: '—', word: 'Skipped' };
		case 'running':
			return { symbol: '…', word: 'Running' };
		default:
			return deployFinished ? { symbol: '—', word: 'Skipped' } : { symbol: '○', word: 'Waiting' };
	}
}

/** The stage the deploy is in now: the running one, else the one waiting to retry, else deploys.stage. */
export function currentStage(d: DeployGet): string {
	return d.stages.find((s) => s.state === 'running')?.stage ?? d.stages.find((s) => isRetrying(s))?.stage ?? d.deploy.stage ?? '';
}

export function formatBytes(n: number): string {
	if (!Number.isFinite(n) || n < 0) return '0 B';
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
	let i = 0;
	let v = n;
	while (v >= 1024 && i < units.length - 1) {
		v /= 1024;
		i++;
	}
	return `${i === 0 ? v : v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

/** The pull's progress in words (the <progress> is not a live region, so this text is not announced). */
export function pullText(p: NonNullable<DeployGet['progress']> | null | undefined): string {
	if (!p) return 'Waiting for the registry to report progress.';
	const layers = p.layers > 0 ? ` · ${p.layers_done} of ${p.layers} layers` : '';
	const size = p.bytes_total > 0 ? `${formatBytes(p.bytes)} of ${formatBytes(p.bytes_total)} (${Math.min(100, Math.floor((p.bytes / p.bytes_total) * 100))}%)` : p.bytes > 0 ? `${formatBytes(p.bytes)} downloaded, total size not known yet` : 'Size not known yet';
	return `${size}${layers}${p.status ? ` · ${p.status}` : ''}`;
}

/** The number of the deploy that was live before deploy `n` (what "kept serving" means), from the history. */
export function previousLive(deploys: DeployItem[], n: number): number | undefined {
	let best: number | undefined;
	for (const d of deploys) if (d.number < n && d.was_live && (best === undefined || d.number > best)) best = d.number;
	return best;
}

export type AppSafety = {
	symbol: string;
	/** For the "Your app" line: "safe, #2 kept serving." */
	text: string;
	/** For an announcement: "Your app is safe: #2 kept serving." */
	sentence: string;
};
const UNSURE = 'ServerDock could not confirm whether the previous version kept serving. Check the service before relying on it.';
function verdict(symbol: string, lead: 'safe' | 'not safe' | 'unknown', detail: string): AppSafety {
	return { symbol, text: `${lead}, ${detail}`.replace(/^unknown, /, 'unknown: ').replace(/^not safe, /, 'not safe: '), sentence: `Your app ${lead === 'unknown' ? 'status is' : 'is'} ${lead}: ${detail}` };
}
const plain = (symbol: string, text: string): AppSafety => ({ symbol, text, sentence: `Your app: ${text}` });

/** Did the deploy start (a stage ran) before it ended? A superseded or cancelled deploy can have been running. */
export function hasStarted(d: DeployGet): boolean {
	return !!d.deploy.started_at || d.stages.some((s) => s.state !== 'pending' && s.state !== 'skipped');
}

/** "Your app: …" in words, by what the deploy did to it. The server's app_safe decides for every ended deploy that did not succeed. */
export function appSafetyText(d: DeployGet['deploy'], previous?: number, started = true): AppSafety {
	if (d.status === 'queued') return plain('○', 'nothing has changed yet. The current version keeps serving while this deploy waits.');
	if (d.status === 'running') {
		return d.strategy === 'stop_first'
			? plain('…', 'stop-first: the app goes down briefly once the old version is stopped, until the new one is healthy.')
			: plain('…', 'the current version keeps serving until the new one is healthy, then traffic switches.');
	}
	if (d.status === 'succeeded') return d.active ? plain('●', 'the new version is live.') : plain('○', 'this version was live and has since been replaced.');
	const kept = previous ? `#${previous} kept serving.` : 'the previous version kept serving.';
	switch (d.app_safe) {
		case 'yes':
			return verdict('✓', 'safe', kept);
		case 'no':
			return verdict('▲', 'not safe', `the previous version${previous ? ` (#${previous})` : ''} is not serving.${d.strategy === 'stop_first' ? ' Stop-first stops the old version before the new one starts.' : ''} Deploy again or roll back to a version that was live.`);
		case 'unknown':
			return verdict('◌', 'unknown', UNSURE);
		default:
			// No verdict recorded: only a deploy that never started is known to have changed nothing.
			if ((d.status === 'cancelled' || d.status === 'superseded') && !started) return plain('○', 'not changed: this deploy never started, so the current version kept serving.');
			return verdict('◌', 'unknown', UNSURE);
	}
}

/** A sentence from a server text: first letter capital, a full stop unless it ends with punctuation. */
export function sentence(text: string): string {
	const t = text.trim();
	if (!t) return '';
	const c = t[0].toUpperCase() + t.slice(1);
	return /[.!?]$/.test(c) ? c : c + '.';
}

/** Why a superseded deploy ended: stopped mid-run, or replaced in the queue before it started. */
export function supersededText(d: DeployGet): string {
	const by = d.deploy.superseded_by ? `#${d.deploy.superseded_by}` : 'a newer deploy';
	return hasStarted(d) ? `Stopped and replaced by ${by}.` : `Replaced in the queue by ${by} before it started.`;
}

/** What became of the new container, from the stages (deploys.get does not return its id). */
export function newContainerText(d: DeployGet): string {
	const start = d.stages.find((s) => s.stage === 'start');
	const finished = !isUnfinished(d.deploy.status);
	if (!start || start.state === 'pending' || start.state === 'skipped') return finished ? 'None was started.' : 'Not started yet.';
	if (start.state === 'failed') return 'It could not be started.';
	if (start.state === 'running') return 'Starting.';
	if (d.deploy.status === 'succeeded') return d.deploy.active ? 'Running: this is the live version.' : 'It was the live version and has since been replaced.';
	if (finished) return 'It was started, then removed because the deploy did not finish.';
	return 'Started.';
}

/** Why Cancel cannot be used now, or null when it can. */
export function cancelReason(d: DeployGet, cancelAsked: boolean): string | null {
	if (!isUnfinished(d.deploy.status)) return null;
	if (d.cancellable) return null;
	if (cancelAsked) return 'Cancel was requested. The deploy stops at its next safe point.';
	switch (d.deploy.stage) {
		case 'switch':
		case 'commit':
			return 'Traffic is being switched to the new version. This cannot be stopped safely, and it takes a moment.';
		case 'drain':
		case 'stop_old':
		case 'done':
			return 'The new version is already serving and the old one is being retired. This cannot be stopped.';
		default:
			return 'This deploy cannot be cancelled now.';
	}
}

/** What is announced when the data changes: one status line per stage change, one alert for a failure. */
export function announcementFor(prev: DeployGet | undefined, next: DeployGet, previous?: number): { status?: string; alert?: string } {
	if (!prev) return {};
	const n = next.deploy.number;
	const wasOpen = isUnfinished(prev.deploy.status);
	if (!isUnfinished(next.deploy.status)) {
		if (!wasOpen) return {};
		const app = appSafetyText(next.deploy, previous, hasStarted(next));
		switch (next.deploy.status) {
			case 'succeeded':
				return { status: `Deploy #${n} succeeded. The new version is live.` };
			case 'failed': {
				const interrupted = next.deploy.reason_code === 'interrupted';
				const head = `Deploy #${n} ${interrupted ? 'was interrupted' : 'failed'}`;
				const why = next.deploy.reason ? `: ${sentence(next.deploy.reason)}` : '.';
				return { alert: `${head}${why} ${app.sentence}` };
			}
			case 'cancelled':
				// A deploy that left the app not safe is a failure the user must hear at once.
				return next.deploy.app_safe === 'no' ? { alert: `Deploy #${n} was cancelled. ${app.sentence}` } : { status: `Deploy #${n} was cancelled.` };
			case 'superseded': {
				const text = `Deploy #${n}: ${supersededText(next).replace(/^S/, 's').replace(/^Replaced/, 'replaced')}`;
				return next.deploy.app_safe === 'no' ? { alert: `${text} ${app.sentence}` } : { status: text };
			}
			default:
				return {};
		}
	}
	const a = stageKey(prev);
	const b = stageKey(next);
	if (a === b) return {};
	const stage = currentStage(next);
	if (!stage) return {};
	const noun = stageNoun(stage);
	const retry = next.stages.find((s) => s.stage === stage && isRetrying(s));
	if (retry) {
		const m = /\(attempt (\d+) of (\d+)\)/.exec(retry.detail ?? '');
		return { status: `Deploy #${n}: ${noun} did not succeed. Waiting to retry${m ? `, attempt ${m[1]} of ${m[2]}` : ''}.` };
	}
	return { status: `Deploy #${n}: ${noun} started.` };
}

/** Changes only when the deploy moves to another stage, or to another attempt of one: never on a progress tick. */
export function stageKey(d: DeployGet): string {
	if (!isUnfinished(d.deploy.status)) return `final:${d.deploy.status}`;
	const stage = currentStage(d);
	const retry = d.stages.find((s) => s.stage === stage && isRetrying(s));
	const m = retry ? /\(attempt (\d+) of/.exec(retry.detail ?? '') : null;
	return `${d.deploy.status}:${stage}${retry ? `:retry${m ? m[1] : ''}` : ''}`;
}

const LOG_SEPARATOR = '--- container output (';
/**
 * Splits a deploy log into what ServerDock wrote (the header and stage lines) and the
 * container's output. The separator line is the first of its kind: the header comes before
 * any container output, so a container that prints a line like it cannot move the split.
 */
export function splitLog(text: string): { header: string; output: string } {
	const at = text.startsWith(LOG_SEPARATOR) ? 0 : text.indexOf('\n' + LOG_SEPARATOR);
	if (at < 0) return { header: '', output: text };
	const start = at === 0 ? 0 : at + 1;
	const eol = text.indexOf('\n', start);
	return { header: text.slice(0, start).replace(/\n+$/, ''), output: eol < 0 ? '' : text.slice(eol + 1) };
}

/** The last `n` lines of the container output. */
export function tailLines(output: string, n: number): string {
	const lines = output.split('\n');
	if (lines.length && lines[lines.length - 1] === '') lines.pop();
	return lines.slice(-n).join('\n');
}
