<script lang="ts">
	// A confirmation dialog (A8): it names the target and the consequence, has
	// verb-specific buttons, puts default focus on Cancel, closes on Escape, and is a
	// native modal <dialog>, so focus is trapped only inside it and returns to the
	// button that opened it.
	import { currentOpener, trackOpener } from './opener';
	import { onMount, tick } from 'svelte';
	type Spec = { title: string; lines: string[]; confirm: string };
	let dialog: HTMLDialogElement | undefined = $state();
	let cancelButton: HTMLButtonElement | undefined = $state();
	let spec: Spec = $state({ title: '', lines: [], confirm: '' });
	let opener: HTMLElement | null = null;
	onMount(trackOpener);
	let resolve: ((ok: boolean) => void) | undefined;

	export async function ask(s: Spec): Promise<boolean> {
		opener = currentOpener();
		spec = s;
		await tick(); // the new title and body are in the DOM before the dialog is announced
		const answer = new Promise<boolean>((r) => (resolve = r));
		dialog?.showModal();
		cancelButton?.focus();
		return answer;
	}
	function finish(ok: boolean) {
		const r = resolve;
		resolve = undefined;
		if (dialog?.open) dialog.close();
		if (!r) return; // already finished (close() also fires the close event)
		r(ok);
		const back = opener;
		if (back?.isConnected) {
			back.focus(); // callers may move focus again afterwards
		}
	}
</script>

<dialog bind:this={dialog} aria-labelledby="confirm-title" aria-describedby="confirm-body" oncancel={(e) => { e.preventDefault(); finish(false); }} onclose={() => finish(false)}>
	<h2 id="confirm-title">{spec.title}</h2>
	<div id="confirm-body">
		{#each spec.lines as line (line)}<p>{line}</p>{/each}
	</div>
	<div class="actions">
		<button type="button" bind:this={cancelButton} onclick={() => finish(false)}>Cancel</button>
		<button type="button" class="primary" onclick={() => finish(true)}>{spec.confirm}</button>
	</div>
</dialog>

<style>
	dialog {
		max-width: min(32rem, calc(100vw - 2rem));
		padding: 1.5rem;
		color: var(--text);
		background: var(--bg);
		border: 2px solid var(--border);
		border-radius: 0.75rem;
	}
	dialog::backdrop {
		background: var(--qr-fg);
		opacity: 0.5;
	}
	h2 {
		margin-top: 0;
	}
</style>
