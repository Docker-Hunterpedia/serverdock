<script lang="ts">
	import { onDestroy, onMount, tick } from 'svelte';
	import Alerts from '../../lib/Alerts.svelte';
	import { flash } from '../../lib/flow.svelte';
	import PageHeading from '../../lib/PageHeading.svelte';
	import ResponsiveTable from '../../lib/ResponsiveTable.svelte';
	import StateBadge from '../../lib/StateBadge.svelte';
	import { absolute } from '../../lib/time';
	import {
		DOCS_URL,
		SEC6_DOCS_URL,
		STATE_ORDER,
		attentionOf,
		deployPath,
		deployStatusInfo,
		getExposure,
		listServices,
		loadErrorMessage,
		servicePath,
		type Exposure,
		type Service
	} from '../../lib/services';

	// Shown once, right after signing in; never from storage.
	let notice = $state(flash.notice);
	let unrecorded = $state(flash.audit === 'unrecorded');
	flash.notice = undefined;
	flash.audit = undefined;
	// A dismissed banner takes its button with it, so focus moves to the heading.
	async function dismiss(hide: () => void) {
		hide();
		await tick();
		document.querySelector<HTMLElement>('h1[tabindex="-1"]')?.focus();
	}
	const failures = $derived(notice?.totp_failures ?? 0);
	const when = $derived(notice?.last_failure_at ? new Date(notice.last_failure_at * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : '');

	// The summary loads on navigation and by Refresh only: no polling, no timers.
	let services: Service[] | undefined = $state();
	let exposure: Exposure | undefined = $state();
	let exposureFailed = $state(false);
	let error: string | null = $state(null);
	let status = $state('');
	let loading = $state(true);
	let loadedAt = $state('');
	let alerts: Alerts | undefined = $state();
	let ctrl: AbortController | undefined;

	async function load(announce: boolean) {
		ctrl?.abort();
		const c = (ctrl = new AbortController());
		loading = true;
		error = null;
		status = announce ? 'Refreshing the overview…' : 'Loading the overview…';
		try {
			// The exposure check is a courtesy: when Docker cannot answer, the rest still shows.
			const [list, exp] = await Promise.all([
				listServices(c.signal),
				getExposure(c.signal).then(
					(x) => ({ ok: true as const, x }),
					(e) => {
						if (e instanceof DOMException && e.name === 'AbortError') throw e;
						return { ok: false as const, x: undefined };
					}
				)
			]);
			if (c !== ctrl) return;
			services = list;
			exposure = exp.x;
			exposureFailed = !exp.ok;
			loadedAt = new Date().toISOString();
			if (announce) {
				const needing = list.filter((s) => attentionOf(s).length > 0).length;
				status =
					`Updated. ${list.length} ${list.length === 1 ? 'service' : 'services'}, ${needing} ${needing === 1 ? 'needs' : 'need'} attention.` +
					(exp.ok ? '' : ' Could not check which other containers publish ports.');
				clearLater();
			} else status = '';
		} catch (e) {
			if (c !== ctrl) return;
			const msg = await loadErrorMessage(e);
			if (msg === undefined) return;
			services = undefined;
			exposure = undefined;
			status = '';
			error = msg;
			await tick();
			alerts?.focus();
		} finally {
			if (c === ctrl) loading = false;
		}
	}
	// "Updated." is cleared a few seconds after a Refresh (one timeout started by the
	// click; nothing runs while the page is idle).
	let clearTimer: ReturnType<typeof setTimeout> | undefined;
	function clearLater() {
		clearTimeout(clearTimer);
		clearTimer = setTimeout(() => (status = ''), 8000);
	}
	onMount(() => void load(false));
	onDestroy(() => {
		ctrl?.abort();
		clearTimeout(clearTimer);
	});

	const counts = $derived.by(() => {
		const n: Record<string, number> = {};
		for (const s of services ?? []) n[s.state] = (n[s.state] ?? 0) + 1;
		return n;
	});
	const attention = $derived((services ?? []).map((s) => ({ s, items: attentionOf(s) })).filter((x) => x.items.length > 0));
	const recent = $derived(
		(services ?? [])
			.filter((s) => s.last_deploy)
			.sort((a, b) => (b.last_deploy!.created_at < a.last_deploy!.created_at ? -1 : b.last_deploy!.created_at > a.last_deploy!.created_at ? 1 : 0))
			.slice(0, 5)
	);
	const shownContainers = $derived((exposure?.containers ?? []).slice(0, 10));
	const exposed = $derived(exposure?.containers.length ?? 0);
	// The port a container publishes on this server is its host port. The same port on
	// 0.0.0.0 and on :: is one entry.
	const portText = (c: { ports: { host_port: number; proto: string }[]; ports_truncated?: boolean }) =>
		[...new Set(c.ports.map((p) => `${p.host_port}/${p.proto}`))].join(', ') + (c.ports_truncated ? ', …' : '');
	const whenText = (l: { status: string; created_at: string; finished_at?: string }) =>
		l.finished_at ? `finished ${absolute(l.finished_at)}` : `${l.status === 'queued' ? 'queued' : 'started'} ${absolute(l.created_at)}`;
	const recentColumns = [
		{ key: 'deploy', label: 'Deploy' },
		{ key: 'result', label: 'Result' },
		{ key: 'image', label: 'Image' },
		{ key: 'when', label: 'When' }
	];
</script>

<svelte:head>
	<title>Overview · ServerDock</title>
</svelte:head>

<PageHeading>Overview</PageHeading>

{#if unrecorded}
	<div role="alert" class="banner">
		<p><span aria-hidden="true">▲</span> The audit record for this action could not be written. It went through, but check the server's disk and logs.</p>
		<button type="button" onclick={() => dismiss(() => (unrecorded = false))}>Dismiss audit warning</button>
	</div>
{/if}
{#if failures > 0}
	<div role="status" class="banner">
		<p>
			{failures} wrong two-factor {failures === 1 ? 'code was' : 'codes were'} entered since your last sign-in{when ? `, the latest at ${when}` : ''}{notice?.last_failure_ip ? ` from ${notice.last_failure_ip}` : ''}.
			If that was not you, change your password.
		</p>
		<button type="button" onclick={() => dismiss(() => (notice = undefined))}>Dismiss sign-in notice</button>
	</div>
{/if}

<div class="toolbar">
	<a class="primary" href="/services/new">New service</a>
	<button type="button" onclick={() => !loading && load(true)} aria-disabled={loading ? 'true' : undefined}>Refresh</button>
	{#if loadedAt}<span class="note tight">{error ? 'Last loaded' : 'Loaded'} {absolute(loadedAt)}</span>{/if}
</div>

<Alerts {error} {status} bind:this={alerts} />

{#if error}
	<p class="note"><span aria-hidden="true">◌</span> Status unknown: could not load the overview. Use Refresh to try again.</p>
{:else if services === undefined}
	<p class="note">Loading the overview…</p>
{:else}
	{#if exposed > 0}
		<section class="banner exposure" aria-labelledby="exposure-title" data-testid="exposure-banner">
			<h2 id="exposure-title"><span aria-hidden="true">▲</span> Other containers use this server's address</h2>
			<p>
				{exposed}
				{exposed === 1 ? 'container publishes a port' : 'containers publish ports'} on this server's address:
				{#each shownContainers as c, i (c.name)}{i > 0 ? ', ' : ''}{c.name} ({portText(c)}){/each}{#if exposed > shownContainers.length || exposure?.truncated}, and more{/if}.
				A service there that uses HTTPS could receive your panel sign-in cookie.
				<a href={SEC6_DOCS_URL} target="_blank" rel="noopener noreferrer">Read about this (opens GitHub)</a>
			</p>
		</section>
	{:else if exposureFailed}
		<p class="note"><span aria-hidden="true">◌</span> Could not check which other containers publish ports on this server.</p>
	{/if}

	{#if services.length === 0}
		<section class="empty" aria-labelledby="empty-title">
			<h2 id="empty-title">No services yet</h2>
			<p>Deploy an image and ServerDock runs it, checks it is healthy and updates it without taking it offline.</p>
			<p><a class="primary" href="/services/new">Deploy an image</a></p>
			<p><a href={DOCS_URL} target="_blank" rel="noopener noreferrer">Read the documentation (opens GitHub)</a></p>
		</section>
	{:else}
		<section aria-labelledby="counts-title" class="block">
			<h2 id="counts-title">Services ({services.length})</h2>
			<ul class="counts" data-testid="state-counts">
				{#each STATE_ORDER as st (st)}
					<li data-state={st}><StateBadge state={st} />: <strong>{counts[st] ?? 0}</strong></li>
				{/each}
			</ul>
			<p><a class="button" href="/services">All services</a></p>
		</section>

		<section aria-labelledby="attention-title" class="block">
			<h2 id="attention-title">Needs attention</h2>
			{#if attention.length === 0}
				<p class="note">Nothing needs attention.</p>
			{:else}
				<ul class="attention" data-testid="needs-attention">
					{#each attention as { s, items } (s.service)}
						<li>
							<a href={servicePath(s)}>{s.service}</a>
							<ul>
								{#each items as it (it.text)}<li><span aria-hidden="true">{it.symbol}</span> {it.text}</li>{/each}
							</ul>
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section aria-labelledby="recent-title" class="block">
			<h2 id="recent-title">Recent deploys</h2>
			<ResponsiveTable
				caption="The latest deploy of the five most recently deployed services"
				columns={recentColumns}
				rows={recent}
				rowKey={(r: Service) => r.service}
				rowLabel={(r: Service) => r.service}
				empty="Nothing has been deployed yet."
			>
				{#snippet cell(r: Service, key: string)}
					{@const l = r.last_deploy!}
					{#if key === 'deploy'}
						<a href={deployPath(r.project, r.name, l.number)}>{r.service} #{l.number}</a>
					{:else if key === 'result'}
						{@const st = deployStatusInfo(l.status)}
						<span class="nowrap"><span aria-hidden="true">{st.symbol}</span> {st.word}</span>
					{:else if key === 'image'}
						<code>{l.image_ref ?? ''}</code>
					{:else if key === 'when'}
						{whenText(l)}
					{/if}
				{/snippet}
			</ResponsiveTable>
		</section>
	{/if}
{/if}

<style>
	.empty {
		border: 1px dashed var(--border);
		border-radius: 0.75rem;
		padding: 1.5rem;
		max-width: 40rem;
	}
	.empty p {
		margin: 0 0 0.75rem;
	}
	.block {
		margin: 0 0 1.5rem;
		max-width: 56rem;
	}
	.counts {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem 1.5rem;
		list-style: none;
		margin: 0 0 0.75rem;
		padding: 0;
	}
	.attention,
	.attention ul {
		margin: 0;
		padding-left: 1.25rem;
	}
	.attention > li {
		margin: 0 0 0.75rem;
	}
	.exposure h2 {
		margin: 0 0 0.5rem;
	}
	.exposure p {
		margin: 0;
	}
	.attention a,
	.exposure a {
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
	}
	.exposure p a {
		display: inline;
		padding: 0.75rem 0;
	}
	.tight {
		margin: 0;
	}
	.nowrap {
		white-space: nowrap;
	}
</style>
