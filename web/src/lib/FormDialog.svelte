<script lang="ts" module>
	export type FormSpec = {
		title: string;
		lines: string[];
		label: string;
		value: string;
		hint?: string;
		/** The verb-specific button. */
		confirm: string;
		/** The text that must be typed exactly (a delete); the button stays disabled until it is. */
		match?: string;
		/** Said when the typed text is empty or does not match. */
		invalid?: string;
	};
</script>

<script lang="ts">
	// A confirmation dialog with one text field (A8): it names the target and the
	// consequence, has a verb-specific button, puts default focus on Cancel, closes on
	// Escape and gives focus back to the control that opened it. `run` is called with the
	// typed text when the user confirms; it may answer with an error to show here, which
	// keeps the dialog open with the typed text.
	import Field from './Field.svelte';
	import { currentOpener, trackOpener } from './opener';
	import { onMount, tick } from 'svelte';

	let dialog: HTMLDialogElement | undefined = $state();
	let cancelButton: HTMLButtonElement | undefined = $state();
	let field: HTMLInputElement | undefined = $state();
	let spec: FormSpec = $state({ title: '', lines: [], label: '', value: '', confirm: '' });
	let value = $state('');
	let error = $state('');
	let status = $state('');
	let busy = $state(false);
	const uid = Math.random().toString(36).slice(2, 8);
	let opener: HTMLElement | null = null;
	onMount(trackOpener);
	let resolve: ((r: { ok: boolean; value: string }) => void) | undefined;
	let run: ((value: string) => Promise<string | undefined>) | undefined;

	const matches = $derived(spec.match === undefined || value.trim() === spec.match);

	export async function ask(s: FormSpec, onConfirm?: (value: string) => Promise<string | undefined>): Promise<{ ok: boolean; value: string }> {
		opener = currentOpener();
		spec = s;
		value = s.value;
		error = '';
		status = '';
		busy = false;
		run = onConfirm;
		await tick();
		const answer = new Promise<{ ok: boolean; value: string }>((r) => (resolve = r));
		dialog?.showModal();
		cancelButton?.focus();
		return answer;
	}
	function finish(ok: boolean) {
		const r = resolve;
		resolve = undefined;
		if (dialog?.open) dialog.close();
		if (!r) return;
		r({ ok, value: value.trim() });
		const back = opener;
		if (back?.isConnected) back.focus(); // callers may move focus again afterwards
	}
	async function complain(msg: string) {
		error = msg;
		status = '';
		await tick();
		status = msg; // also said in the status region: focus may already be on the field
		field?.focus();
	}
	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (busy) return;
		const v = value.trim();
		if (!v || !matches) {
			await complain(spec.invalid ?? 'Enter a value.');
			return;
		}
		if (run) {
			busy = true;
			error = '';
			try {
				const msg = await run(v);
				if (msg) {
					await complain(msg);
					return;
				}
			} finally {
				busy = false;
			}
		}
		finish(true);
	}
</script>

<dialog bind:this={dialog} aria-labelledby="form-title-{uid}" aria-describedby="form-body-{uid}" oncancel={(e) => { e.preventDefault(); finish(false); }} onclose={() => finish(false)}>
	<form onsubmit={submit} novalidate aria-busy={busy}>
		<h2 id="form-title-{uid}">{spec.title}</h2>
		<div id="form-body-{uid}">
			{#each spec.lines as line (line)}<p>{line}</p>{/each}
		</div>
		<div role="status" class="sr">{status}</div>
		<Field id="form-field-{uid}" label={spec.label} bind:value bind:el={field} hint={spec.hint} {error} />
		<div class="actions">
			<button type="button" bind:this={cancelButton} onclick={() => finish(false)}>Cancel</button>
			<button type="submit" class="primary" aria-disabled={busy || !matches ? 'true' : undefined}>{busy ? 'Working…' : spec.confirm}</button>
		</div>
	</form>
</dialog>

<style>
	dialog {
		max-width: min(32rem, calc(100vw - 2rem));
		padding: 1.5rem;
		color: var(--text);
		background: var(--bg);
		border: 2px solid var(--border);
		border-radius: 0.75rem;
		overflow-wrap: anywhere; /* an image reference or target has no spaces to wrap at (320 px reflow) */
	}
	dialog::backdrop {
		background: var(--qr-fg);
		opacity: 0.5;
	}
	h2 {
		margin-top: 0;
	}
</style>
