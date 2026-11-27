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
