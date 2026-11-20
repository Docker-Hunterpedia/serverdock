<script lang="ts">
	import PageHeading from '../../lib/PageHeading.svelte';
</script>

<svelte:head>
	<title>Overview · ServerDock</title>
</svelte:head>

<PageHeading>Overview</PageHeading>

<section class="empty" aria-labelledby="empty-title">
	<h2 id="empty-title">No services yet</h2>
	<p>Deploying from an image arrives in the next version.</p>
	<p><a href="https://github.com/Docker-Hunterpedia/serverdock/tree/main/docs" rel="noopener">Read the documentation (opens GitHub)</a></p>
</section>

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
</style>
