<script lang="ts">
	// A service state as word plus symbol (the symbol is decoration; the word carries it).
	import { stateInfo } from './services';
	let { state }: { state: string } = $props();
	const info = $derived(stateInfo(state));
</script>

<span class="state state-{state}"><span aria-hidden="true">{info.symbol}</span> {info.word}</span>

<style>
	.state {
		white-space: nowrap;
		font-weight: 600;
	}
</style>
