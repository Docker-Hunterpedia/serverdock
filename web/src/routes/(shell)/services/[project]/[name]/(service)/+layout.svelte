<script lang="ts">
	import { afterNavigate, beforeNavigate } from '$app/navigation';
	import { page } from '$app/state';
	import { onDestroy, tick, untrack } from 'svelte';
	import Alerts from '../../../../../../lib/Alerts.svelte';
	import ConfirmDialog from '../../../../../../lib/ConfirmDialog.svelte';
	import FormDialog from '../../../../../../lib/FormDialog.svelte';
	import PageHeading from '../../../../../../lib/PageHeading.svelte';
	import StateBadge from '../../../../../../lib/StateBadge.svelte';
	import { ApiError, runAction } from '../../../../../../lib/api';
	import { ServiceCtx, provideService, queuedMessage } from '../../../../../../lib/servicectx.svelte';
	import {
		SEC6_DOCS_URL,
		appAddress,
		healthText,
		mutationMessage,
		repositoryOf,
		servicePath,
		sharesPanelText,
		strategyConsequence,
		strategyWord,
		type QueuedDeploy
	} from '../../../../../../lib/services';
	import { absolute } from '../../../../../../lib/time';

	let { children } = $props();

	const ctx = new ServiceCtx();
	provideService(ctx);
	let alerts: Alerts | undefined = $state();
	let confirmDialog: ConfirmDialog | undefined = $state();
	let formDialog: FormDialog | undefined = $state();
	ctx.focusAlert = () => alerts?.focus();

	// Loaded when another service is opened and by Refresh; no polling. Moving between
	// Deploys and Settings keeps the same service, so it asks nothing.
	let loadedRef = '';
	$effect(() => {
		const p = page.params.project ?? '';
		const n = page.params.name ?? '';
		untrack(() => {
			if (`${p}/${n}` === loadedRef) return;
			loadedRef = `${p}/${n}`;
			ctx.project = p;
			ctx.name = n;
			ctx.service = undefined;
			ctx.deploys = undefined;
			ctx.busy = false;
			ctx.generation++;
			ctx.clearMessages();
			void ctx.load(false);
		});
	});
	onDestroy(() => ctx.abort());

	// A dialog that is open when the page moves on (browser Back) is closed: it must not act on another service.
	beforeNavigate(() => {
		confirmDialog?.close();
		formDialog?.close();
	});
	// Moving between the sub-pages puts focus on the new section's heading (and #port on the port section).
	afterNavigate(async (nav) => {
		if (nav.type === 'enter' || !nav.to) return;
		const inGroup = (id: string | null | undefined) => !!id?.includes('(service)');
		const same = nav.from?.params?.project === nav.to.params?.project && nav.from?.params?.name === nav.to.params?.name;
		const wantPort = nav.to.url.hash === '#port' && inGroup(nav.to.route.id);
		if (!wantPort && !(same && inGroup(nav.from?.route.id) && inGroup(nav.to.route.id))) return;
		await tick();
		await tick();
		const target = wantPort ? document.getElementById('h-port') : document.querySelector<HTMLElement>('main [data-subpage]');
		target?.focus();
	});

	const svc = $derived(ctx.service);
	const live = $derived(ctx.live);
	const onSettings = $derived(page.route.id?.endsWith('/settings') ?? false);
	const sec6 = $derived(svc?.warnings.find((w) => w.code === 'shares_panel_address'));
	const noLive = 'The service has no live version yet. Deploy it first.';

	/** Runs one action: the label is spoken while it runs; the result is said only if this service is still the page. */
	async function act<T>(ref: string, label: string, fn: () => Promise<T>, done: (out: T) => string) {
		const end = ctx.begin(label);
		if (!end) return;
		try {
			const out = await fn();
			if (ctx.ref !== ref) return;
			await ctx.load(false);
			if (ctx.ref === ref) ctx.say(done(out));
		} catch (e) {
			if (ctx.ref === ref) await ctx.fail(e);
		} finally {
			end();
		}
	}

	async function deploy() {
		if (!svc || !ctx.idle()) return;
		const ref = ctx.ref; // what the dialog was opened for, whatever the page shows later
		const cur = svc;
		const repo = repositoryOf(cur.image_ref);
		let closed = false;
		const asked = formDialog?.ask(
			{
				title: `Deploy ${ref}`,
				lines: [
					`Starts a new deploy of ${ref}. It pulls the image below again, so a moving tag such as latest may bring a newer version.`,
					`The repository stays ${repo}. Change only the tag or digest here; to use another repository, use Settings.`,
					strategyConsequence(cur.strategy)
				],
				label: 'Tag or digest',
				hint: `For example ${repo}:1.2 or ${repo}@sha256:…`,
				value: cur.image_ref,
				confirm: 'Deploy',
				invalid: 'Enter the image to deploy, with a tag or a digest.',
				working: 'Deploy request sent, waiting for the answer'
			},
			async (image) => {
				const end = ctx.begin('Deploy request sent, waiting for the answer…');
				if (!end) return 'Another action on this service is still running. Wait for it to finish, then try again.';
				try {
					const out = await runAction<QueuedDeploy>('services.deploy', { service: ref, ...(image !== cur.image_ref ? { image_ref: image } : {}) });
					if (ctx.ref === ref) {
						await ctx.load(false);
						ctx.say(queuedMessage('Deploy', out));
					}
					return undefined;
				} catch (e) {
					if (!(e instanceof ApiError) || e.status === 401) {
						if (ctx.ref === ref) await ctx.fail(e);
						return undefined;
					}
					const msg = mutationMessage(e);
					if (!closed) ctx.status = ''; // the dialog shows the refusal; the page must not keep saying "waiting"
					// With the dialog gone, the page's alert says it; with it open, it shows in the dialog and keeps the typed text.
					if (closed && ctx.ref === ref) await ctx.alert(`The deploy was not queued: ${msg}`);
					return msg;
				} finally {
					end();
				}
			}
		);
		void asked?.then(() => (closed = true));
	}

	async function redeploy() {
		if (!svc || !live || !ctx.idle()) return;
		const ref = ctx.ref;
		const cur = svc;
		const l = live;
		const ok = await confirmDialog?.ask({
			title: `Redeploy ${ref}?`,
			lines: [
				`Starts a new deploy that reuses the exact image of live deploy #${l.number} (${l.image_ref}) with its settings, in a fresh container. Nothing is pulled again.`,
				strategyConsequence(cur.strategy)
			],
			confirm: 'Redeploy'
		});
		if (ok && ctx.ref === ref) await act(ref, 'Redeploying…', () => runAction<QueuedDeploy>('services.redeploy', { service: ref }), (q) => queuedMessage('Redeploy', q));
	}

	async function restart() {
		if (!svc || !live || !ctx.idle()) return;
		const ref = ctx.ref;
		const ok = await confirmDialog?.ask({
			title: `Restart ${ref}?`,
			lines: [
				`The app is down while its container restarts: expect a short outage until the health check passes again. The address stays the same.`,
				'To switch to a fresh container without an outage, use Redeploy instead (blue-green services only).'
			],
			confirm: 'Restart now'
		});
		if (ok && ctx.ref === ref)
			await act(ref, 'Restarting…', () => runAction<{ status: string }>('services.restart', { service: ref }), () => `Restart queued. The app is briefly down while it restarts. It runs in the background: use Refresh to see how it goes.`);
	}
</script>

<svelte:head>
	<title>{ctx.notFound ? 'Service not found' : `${onSettings ? 'Settings' : 'Deploys'} · ${ctx.name} · ${ctx.project}`} · ServerDock</title>
</svelte:head>

<ConfirmDialog bind:this={confirmDialog} />
<FormDialog bind:this={formDialog} />

{#if ctx.notFound}
	<PageHeading>Service not found</PageHeading>
	<p>There is no service called <code>{ctx.ref}</code>. It may have been deleted.</p>
	<p><a class="button" href="/services">Back to Services</a></p>
{:else}
	<PageHeading>{ctx.name} {#if svc}<span class="state"><StateBadge state={svc.state} /></span>{/if}</PageHeading>
	<p class="note"><a href="/services">Services</a> / {ctx.project} / {ctx.name}</p>

	<div class="toolbar">
		<button type="button" class="primary" onclick={deploy} aria-disabled={!svc || ctx.busy ? 'true' : undefined}>Deploy</button>
		<button type="button" onclick={() => (!live ? ctx.alert(noLive) : redeploy())} aria-disabled={!svc || ctx.busy || !live ? 'true' : undefined} aria-describedby={svc && !live ? 'no-live' : undefined}>Redeploy</button>
		<button type="button" onclick={() => (!live ? ctx.alert(noLive) : restart())} aria-disabled={!svc || ctx.busy || !live ? 'true' : undefined} aria-describedby={svc && !live ? 'no-live' : undefined}>Restart</button>
		<button type="button" onclick={() => !ctx.loading && ctx.load(true)} aria-disabled={ctx.loading ? 'true' : undefined}>Refresh</button>
	</div>
	{#if svc && !live}<p id="no-live" class="hint">Redeploy and Restart need a live version. {noLive}</p>{/if}

	<Alerts error={ctx.error} links={ctx.links} status={ctx.status} bind:this={alerts} />

	{#if !svc}
		{#if ctx.loadError}
			<p class="note"><span aria-hidden="true">◌</span> Status unknown: could not load this service. Use Refresh to try again.</p>
		{:else}
			<p class="note">Loading the service…</p>
		{/if}
	{:else}
		{#if ctx.loadError}
			<!-- The last good service stays (the forms below keep what was typed) but its state is not shown as current. -->
			<p class="note" data-testid="status-unknown"><span aria-hidden="true">◌</span> Status unknown: could not load this service again. What is shown below may be out of date. Use Refresh to try again.</p>
		{/if}
		{#if !ctx.loadError}
		{#if svc.needs_attention === 'versions_kept'}
			<div class="banner" data-testid="needs-attention"><p><span aria-hidden="true">▲</span> Two versions of this service may be running after an unfinished stop-first deploy. ServerDock settles this by itself; try again once the service has settled.</p></div>
		{:else if svc.needs_attention === 'previous_version_down'}
			<div class="banner" data-testid="needs-attention"><p><span aria-hidden="true">▲</span> A stop-first deploy failed and the previous version did not come back. Deploy again or roll back to a version that was live.</p></div>
		{:else if svc.needs_attention === 'delete_confirm_again'}
			<div class="banner" data-testid="needs-attention"><p><span aria-hidden="true">▲</span> A delete of this service needed a fresh confirmation and did not run. Nothing was deleted and the service was left as it was. Deploys and restarts that were queued were cancelled, and a deploy that was running was stopped or, if it had already switched, finished early. Open Settings and use Delete again.</p></div>
		{:else if svc.needs_attention === 'delete_interrupted'}
			<div class="banner" data-testid="needs-attention"><p><span aria-hidden="true">▲</span> A delete of this service did not finish. Open Settings and delete it again to finish it.</p></div>
		{/if}

		<div class="cards">
			<section class="card-s" aria-labelledby="c-running" data-testid="card-running">
				<h2 id="c-running">Running now</h2>
				{#if live}
					<p><StateBadge state={svc.state} /> · live deploy <strong>#{live.number}</strong></p>
					<p>Image <code>{live.image_ref}</code></p>
					{#if live.repo_digest}<p class="hint">Digest <code>{live.repo_digest}</code></p>{/if}
					<p class="hint">Deployed {absolute(live.finished_at || live.created_at)}</p>
					{#if svc.state === 'missing'}
						<p class="warn" data-testid="state-missing"><span aria-hidden="true">◆</span> ServerDock cannot find the container of the live version. Redeploy to create it again.</p>
					{:else if svc.state === 'exited'}
						<p class="warn"><span aria-hidden="true">▲</span> The live version has exited. Restart it, or Redeploy.</p>
					{:else if svc.state === 'restarting'}
						<p class="warn"><span aria-hidden="true">▲</span> The live version keeps restarting. Check its health settings, or roll back.</p>
					{:else if svc.state === 'unknown'}
						<p class="warn"><span aria-hidden="true">◌</span> ServerDock cannot confirm the state with Docker right now. Use Refresh to ask again.</p>
					{/if}
				{:else}
					<p>Nothing is running: this service has no live version yet.</p>
					<p class="hint">Next deploy uses <code>{svc.image_ref}</code>.</p>
				{/if}
			</section>

			<section class="card-s" aria-labelledby="c-traffic" data-testid="card-traffic">
				<h2 id="c-traffic">Traffic</h2>
				{#if svc.public_port}
					<p>Address <code>{appAddress(svc.public_port)}</code> (plain HTTP)</p>
				{:else}
					<p>Not exposed: the app has no public port{svc.container_port ? '' : ' and listens on no port'}.</p>
				{/if}
				{#if svc.route_error}
					<p class="warn" data-testid="route-error"><span aria-hidden="true">▲</span> Route problem: {svc.route_error} <a href="{servicePath(svc)}/settings#port">Open Settings to try again</a>.</p>
				{:else if svc.public_port && svc.routes_stale}
					<p class="warn"><span aria-hidden="true">▲</span> The proxy may not match the saved route. <a href="{servicePath(svc)}/settings#port">Open Settings to try again</a>.</p>
				{:else if svc.public_port && !svc.routes_ready}
					<p class="hint"><span aria-hidden="true">◌</span> Routes are not served yet: ServerDock is still reading the container list.</p>
				{/if}
				{#if sec6}
					<p class="sec6" data-testid="sec6"><span aria-hidden="true">▲</span> {sharesPanelText(sec6.message, svc.public_port ?? null)} <a href={SEC6_DOCS_URL} target="_blank" rel="noopener noreferrer">Read about this (opens GitHub)</a></p>
				{/if}
				<p>Health check: {healthText(svc)}</p>
			</section>

			<section class="card-s" aria-labelledby="c-strategy" data-testid="card-strategy">
				<h2 id="c-strategy">Strategy</h2>
				<p><strong>{strategyWord(svc.strategy)}</strong></p>
				<p class="hint">{strategyConsequence(svc.strategy)}</p>
				<p class="hint">Drain {svc.drain_s} s · stop grace {svc.stop_grace_s} s</p>
			</section>
		</div>

		{/if}

		<nav aria-label="Service sections" class="subnav">
			<ul>
				<li><a href={servicePath(svc)} aria-current={onSettings ? undefined : 'page'}>Deploys</a></li>
				<li><a href="{servicePath(svc)}/settings" aria-current={onSettings ? 'page' : undefined}>Settings</a></li>
			</ul>
		</nav>

		{@render children()}
	{/if}
{/if}

<style>
	.state {
		font-size: 1rem;
		margin-left: 0.5rem;
	}
	.cards {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(min(100%, 16rem), 1fr));
		gap: 1rem;
		margin: 0 0 1.5rem;
	}
	.card-s {
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
		background: var(--surface);
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.card-s p {
		margin: 0 0 0.5rem;
	}
	.warn {
		font-weight: 600;
	}
	.sec6 {
		border-left: 4px solid var(--border);
		padding-left: 0.75rem;
	}
	.subnav ul {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem;
		list-style: none;
		margin: 0 0 1rem;
		padding: 0;
		border-bottom: 2px solid var(--border);
	}
	.subnav a {
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
		min-width: var(--target);
		padding: 0.5rem 1rem;
		color: var(--text);
		text-decoration: none;
		border-bottom: 4px solid transparent;
		margin-bottom: -2px;
	}
	.subnav a[aria-current='page'] {
		font-weight: 700;
		border-bottom-color: var(--text);
	}
</style>
