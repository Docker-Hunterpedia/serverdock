<script lang="ts">
	// A placeholder until step 14 (Deploy detail with live progress): it makes the
	// "Create and deploy" redirect land on a real page that says what happened so far.
	import { page } from '$app/state';
	import { takeCreated } from '../../../../../../../lib/flow.svelte';
	import PageHeading from '../../../../../../../lib/PageHeading.svelte';
	import { SEC6_DOCS_URL, servicePath, sharesPanelText } from '../../../../../../../lib/services';

	const project = $derived(page.params.project ?? '');
	const name = $derived(page.params.name ?? '');
	const number = $derived(page.params.number ?? '');
	// Taken once, and only when this is the deploy that "Create and deploy" just queued.
	const just = takeCreated(`${page.params.project}/${page.params.name}`, Number(page.params.number));
	const warnings = just?.warnings ?? [];
</script>

<svelte:head>
	<title>Deploy #{number} · {project}/{name} · ServerDock</title>
</svelte:head>

<PageHeading>Deploy #{number}</PageHeading>
<p><a href={servicePath({ project, name })}>{project}/{name}</a></p>
{#if just}
	<p class="note">This deploy was just queued and runs in the background.</p>
	{#if just.applied === false}
		<div class="banner" data-testid="not-applied">
			<p><span aria-hidden="true">▲</span> {just.notice || 'Saved, but not applied yet: the public port is not being served yet.'}</p>
		</div>
	{/if}
{/if}
<p class="note">This page does not show a deploy's status yet. See <a href="/services">Services</a> for the result of each service's latest deploy.</p>
{#each warnings as w (w.code)}
	<div class="banner">
		<p>
			<span aria-hidden="true">▲</span>
			{w.code === 'shares_panel_address' ? sharesPanelText(w.message, just?.port ?? null) : w.message}
			{#if w.code === 'shares_panel_address'}<a href={SEC6_DOCS_URL} target="_blank" rel="noopener noreferrer">Read about this (opens GitHub)</a>{/if}
		</p>
	</div>
{/each}
<p><a class="button" href="/services">Back to Services</a></p>
