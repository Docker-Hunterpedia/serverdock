<script lang="ts">
	import { page } from '$app/state';
</script>

<svelte:head>
	<title>Page not found · ServerDock</title>
</svelte:head>

<main class="plain">
	<h1 tabindex="-1">{page.status === 404 ? 'Page not found' : 'Something went wrong'}</h1>
	{#if page.status !== 404}<p>{page.error?.message}</p>{/if}
	<p><a href="/">Go to the Overview</a></p>
</main>

<style>
	.plain {
		max-width: 40rem;
		margin: 0 auto;
		padding: 2rem 1rem;
	}
</style>
