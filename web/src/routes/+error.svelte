<script lang="ts">
	import { page } from '$app/state';
	import PageHeading from '../lib/PageHeading.svelte';

	const notFound = $derived(page.status === 404);
	const heading = $derived(notFound ? 'Page not found' : 'Something went wrong');
</script>

<svelte:head>
	<title>{heading} · ServerDock</title>
</svelte:head>

<main class="plain">
	<PageHeading>{heading}</PageHeading>
	{#if !notFound}
		<p>{page.error?.message}</p>
		{#if page.status >= 500}
			<p><button type="button" onclick={() => location.reload()}>Try again</button></p>
		{/if}
	{/if}
	<p><a class="back" href="/">Go to the Overview</a></p>
</main>

<style>
	.plain {
		max-width: 40rem;
		margin: 0 auto;
		padding: 2rem 1rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
	}
</style>
