<script lang="ts">
	// Recovery codes, shown once (A7): a labelled list, Copy all, Download, a guard
	// against leaving before they are saved, and a Continue that stays aria-disabled
	// (with its reason) until the box is ticked. Used by first-run setup and by
	// "Make new recovery codes" in Settings > Security.
	import { beforeNavigate } from '$app/navigation';
	import { copyText } from './copy';

	type Props = { codes: string[]; onstatus: (message: string) => void; onfinish: () => void; lead: string };
	let { codes, onstatus, onfinish, lead }: Props = $props();
	let saved = $state(false);

	async function copy() {
		onstatus((await copyText(codes.join('\n'))) ? 'Codes copied.' : 'Could not copy. Select the text and copy it yourself.');
	}
	function download() {
		const url = URL.createObjectURL(new Blob([codes.join('\n') + '\n'], { type: 'text/plain' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = 'serverdock-recovery-codes.txt';
		a.click();
		setTimeout(() => URL.revokeObjectURL(url), 1000); // not at once: the browser may still be reading it
		onstatus('Download started.');
	}

	// In-app navigation is cancelled with a message until the codes are saved;
	// reloading or closing the tab is the beforeunload guard.
	beforeNavigate((nav) => {
		if (!saved && nav.type !== 'leave') {
			nav.cancel();
			onstatus('Tick "I have saved these codes" before you leave this page. They are not shown again.');
		}
	});
	function guard(e: BeforeUnloadEvent) {
		if (!saved) e.preventDefault();
	}
	function finish(e: Event) {
		e.preventDefault();
		if (!saved) {
			onstatus('Tick the box to confirm you saved the codes first.');
			return;
		}
		onfinish();
	}
</script>

<svelte:window onbeforeunload={guard} />

<p>{lead}</p>
<ol class="codes" aria-label="Recovery codes">
	{#each codes as c (c)}
		<li><code>{c}</code></li>
	{/each}
</ol>
<div class="actions">
	<button type="button" onclick={copy}>Copy all</button>
	<button type="button" onclick={download}>Download as text</button>
</div>
<p class="hint">Clipboard history apps may keep a copy; Download is safer for recovery codes.</p>
<p class="hint">You can make new codes later in Settings › Security.</p>
<form onsubmit={finish}>
	<p>
		<label class="check"><input type="checkbox" bind:checked={saved} /> I have saved these codes</label>
	</p>
	<p id="why-disabled" class="hint">{saved ? '' : 'Tick the box to confirm you saved the codes.'}</p>
	<div class="actions">
		<button type="submit" class="primary" aria-disabled={!saved} aria-describedby="why-disabled">Continue</button>
	</div>
</form>

<style>
	.codes {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
		gap: 0.5rem;
		padding-left: 2rem;
		margin: 0 0 1rem;
	}
	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.75rem;
		min-height: var(--target);
		font-weight: 600;
	}
	.check input {
		width: 1.5rem;
		height: 1.5rem;
	}
</style>
