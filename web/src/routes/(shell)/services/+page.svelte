<script lang="ts">
	import { onDestroy, onMount, tick } from 'svelte';
	import Alerts from '../../../lib/Alerts.svelte';
	import PageHeading from '../../../lib/PageHeading.svelte';
	import ResponsiveTable from '../../../lib/ResponsiveTable.svelte';
	import StateBadge from '../../../lib/StateBadge.svelte';
	import { absolute } from '../../../lib/time';
	import { DOCS_URL, appAddress, deployStatusInfo, listServices, loadErrorMessage, servicePath, type Service } from '../../../lib/services';

	// Loaded on navigation and by Refresh; nothing else: no polling and no timers.
	let services: Service[] | undefined = $state();
	let error: string | null = $state(null);
	let status = $state('');
	let loading = $state(true);
	let alerts: Alerts | undefined = $state();
	let ctrl: AbortController | undefined;

	async function load(announce: boolean) {
		ctrl?.abort();
		const c = (ctrl = new AbortController());
		loading = true;
		error = null;
		clearTimeout(clearTimer);
		status = announce ? 'Refreshing the list…' : '';
		try {
			const list = await listServices(c.signal);
			if (c !== ctrl) return;
			services = list;
			status = announce ? `Updated. ${list.length} ${list.length === 1 ? 'service' : 'services'}.` : '';
			if (announce) clearLater();
		} catch (e) {
			if (c !== ctrl) return;
			const msg = await loadErrorMessage(e);
			if (msg === undefined) return;
			// A12: the stale list is replaced by an honest "unknown" line.
			services = undefined;
			status = '';
			error = msg;
			await tick();
			alerts?.focus();
		} finally {
			if (c === ctrl) loading = false;
		}
	}
	// One timeout, started by the Refresh click, clears "Updated."; nothing runs while idle.
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

	const groups = $derived.by(() => {
		const by = new Map<string, Service[]>();
		for (const s of services ?? []) by.set(s.project, [...(by.get(s.project) ?? []), s]);
		return [...by.entries()];
	});

	const columns = [
		{ key: 'name', label: 'Name' },
		{ key: 'state', label: 'State' },
		{ key: 'image', label: 'Image' },
		{ key: 'live', label: 'Live deploy' },
		{ key: 'address', label: 'Address' },
		{ key: 'last', label: 'Last deploy' }
	];
</script>

<svelte:head>
	<title>Services · ServerDock</title>
</svelte:head>

<PageHeading>Services</PageHeading>

<div class="toolbar">
	<a class="primary" href="/services/new">New service</a>
	<button type="button" onclick={() => !loading && load(true)} aria-disabled={loading ? 'true' : undefined}>Refresh</button>
</div>

<Alerts {error} {status} bind:this={alerts} />

{#if error}
	<p class="note"><span aria-hidden="true">◌</span> Status unknown: could not load the services. Use Refresh to try again.</p>
{:else if services === undefined}
	<p class="note">Loading the services…</p>
{:else if services.length === 0}
	<section class="empty" aria-labelledby="empty-title">
		<h2 id="empty-title">No services yet</h2>
		<p>A service is one app that ServerDock runs from a Docker image, keeps healthy and updates without taking it offline.</p>
		<p><a class="primary" href="/services/new">Deploy an image</a></p>
		<p><a href={DOCS_URL} target="_blank" rel="noopener noreferrer">Read the documentation (opens GitHub)</a></p>
	</section>
{:else}
	{#each groups as [project, rows] (project)}
		<section class="group">
			<h2><span class="sr">Project</span> {project}</h2>
			<ResponsiveTable
				caption="Services in {project}"
				{columns}
				{rows}
				rowKey={(r: Service) => r.service}
				rowLabel={(r: Service) => r.service}
				empty="No services."
			>
				{#snippet cell(r: Service, key: string)}
					{#if key === 'name'}
						<a href={servicePath(r)}>{r.name}</a>
					{:else if key === 'state'}
						<StateBadge state={r.state} />
					{:else if key === 'image'}
						<code>{r.image_ref}</code>
					{:else if key === 'live'}
						{r.active_deploy ? `#${r.active_deploy}` : 'None yet'}
					{:else if key === 'address'}
						{#if r.public_port}<code>{appAddress(r.public_port)}</code>{:else}Not exposed{/if}
					{:else if key === 'last'}
						{#if r.last_deploy}
							{@const st = deployStatusInfo(r.last_deploy.status)}
							#{r.last_deploy.number} <span class="nowrap"><span aria-hidden="true">{st.symbol}</span> {st.word}</span>,
							{r.last_deploy.finished_at ? `finished ${absolute(r.last_deploy.finished_at)}` : `${r.last_deploy.status === 'queued' ? 'queued' : 'started'} ${absolute(r.last_deploy.created_at)}`}
						{:else}
							Never deployed
						{/if}
					{/if}
				{/snippet}
			</ResponsiveTable>
		</section>
	{/each}
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
	.group {
		margin: 0 0 1.5rem;
	}
	.nowrap {
		white-space: nowrap;
	}
</style>
