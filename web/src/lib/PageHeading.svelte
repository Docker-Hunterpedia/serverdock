<script lang="ts">
	import type { Snippet } from 'svelte';

	// The page's one <h1>. tabindex="-1" lets the root layout move focus here on a
	// route change (it is never a tab stop).
	let { children }: { children: Snippet } = $props();
</script>

<h1 tabindex="-1">{@render children()}</h1>
