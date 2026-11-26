<script lang="ts">
	// A placeholder until step 14 (Deploy detail with live progress): it makes the
	// "Create and deploy" redirect land on a real page that says what happened so far.
	import { page } from '$app/state';
	import { created } from '../../../../../../../lib/flow.svelte';
	import PageHeading from '../../../../../../../lib/PageHeading.svelte';
	import { SEC6_DOCS_URL, servicePath, sharesPanelText } from '../../../../../../../lib/services';

	const project = $derived(page.params.project ?? '');
	const name = $derived(page.params.name ?? '');
	const number = $derived(page.params.number ?? '');
	// Shown once, for the service that was just created.
	const warnings = $derived(created.service === `${project}/${name}` ? (created.warnings ?? []) : []);
</script>

<svelte:head>
	<title>Deploy #{number} · {project}/{name} · ServerDock</title>
</svelte:head>

<PageHeading>Deploy #{number}</PageHeading>
<p><a href={servicePath({ project, name })}>{project}/{name}</a></p>
<p class="note">The deploy is queued and runs in the background. The live progress page for a deploy arrives in the next version, so this page does not show its progress yet.</p>
{#each warnings as w (w.code)}
	<div class="banner">
		<p>
			<span aria-hidden="true">▲</span>
			{w.code === 'shares_panel_address' ? sharesPanelText(w.message, created.port ?? null) : w.message}
			{#if w.code === 'shares_panel_address'}<a href={SEC6_DOCS_URL} rel="noopener">Read about this (opens GitHub)</a>{/if}
		</p>
	</div>
{/each}
<p><a class="button" href="/services">Back to Services</a></p>
