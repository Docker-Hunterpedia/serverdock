<script lang="ts">
	// The page's one assertive region (errors, with links to the fields at fault) and
	// its one status region (progress, copy confirmations, countdown steps). A failed
	// submit calls focus() so the summary is read and reachable.
	type Props = { error?: string | null; links?: { id: string; text: string }[]; status?: string };
	let { error = null, links = [], status = '' }: Props = $props();
	let box: HTMLElement | undefined = $state();

	export function focus() {
		box?.focus();
	}
	function toField(e: MouseEvent, id: string) {
		e.preventDefault();
		document.getElementById(id)?.focus();
	}
</script>

<div class="alerts">
	<div role="alert" tabindex="-1" bind:this={box} class="alert" class:shown={!!error}>
		{#if error}
			<p><span aria-hidden="true">▲</span> {error}</p>
			{#if links.length}
				<ul>
					{#each links as l (l.id)}
						<li><a href="#{l.id}" onclick={(e) => toField(e, l.id)}>{l.text}</a></li>
					{/each}
				</ul>
			{/if}
		{/if}
	</div>
	<div role="status" class="status">{status}</div>
</div>
