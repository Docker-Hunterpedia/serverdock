<script lang="ts">
	import '../app.css';
	import { afterNavigate } from '$app/navigation';
	import { tick } from 'svelte';

	let { children } = $props();

	// On a route change focus moves to the new page's <h1> (never on the first load,
	// so the skip link stays the first tab stop).
	afterNavigate(async (nav) => {
		if (nav.type === 'enter') return;
		await tick();
		document.querySelector<HTMLElement>('h1[tabindex="-1"]')?.focus();
	});
</script>

{@render children()}
