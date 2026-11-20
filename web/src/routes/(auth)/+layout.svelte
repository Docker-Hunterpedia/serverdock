<script lang="ts">
	let { children } = $props();
</script>

<main class="card">
	<p class="brand-line">ServerDock</p>
	{@render children()}
</main>
