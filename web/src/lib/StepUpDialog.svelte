<script lang="ts">
	// Step-up: a sensitive action needs a fresh two-factor code (403 mfa_required).
	// The dialog asks for one, sends it to /auth/reauth, and resolves true when it was
	// accepted. Five wrong codes end the session (Signed-out, reason step_up).
	import { goto } from '$app/navigation';
	import { ApiError, NETWORK_MESSAGE, reauth, signedOutReason } from './api';
	import { Countdown, humanDuration } from './countdown.svelte';
	import { currentOpener, trackOpener } from './opener';
	import { onMount } from 'svelte';
	import Field from './Field.svelte';

	let dialog: HTMLDialogElement | undefined = $state();
	let code = $state('');
	let error = $state('');
	let busy = $state(false);
	const countdown = new Countdown((m) => (error = m));
	let field: HTMLInputElement | undefined = $state();
	let opener: HTMLElement | null = null;
	onMount(trackOpener);
	let resolve: ((ok: boolean) => void) | undefined;

	export function ask(): Promise<boolean> {
		code = '';
		error = '';
		opener = currentOpener();
		dialog?.showModal();
		field?.focus();
		return new Promise((r) => (resolve = r));
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
	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (countdown.active) {
			error = `Try again in ${humanDuration(countdown.remaining)}.`;
			return;
		}
		if (busy) return;
		const c = code.replace(/\s/g, '');
		if (!/^\d{6}$/.test(c)) {
			error = 'Enter the 6 digits from your app.';
			field?.focus();
			return;
		}
		busy = true;
		error = '';
		try {
			await reauth(c);
			finish(true);
		} catch (err) {
			code = '';
			if (!(err instanceof ApiError)) throw err;
			if (err.status === 401) {
				finish(false);
				// Too many wrong step-up codes end the session; any other 401 is the usual reason.
				await goto(`/signed-out?reason=${err.code === 'revoked' ? 'step_up' : signedOutReason(err.code)}`);
				return;
			}
			if (err.code === 'rate_limited') {
				countdown.start(err.retryAfter ?? 60);
				field?.focus();
				return;
			}
			if (err.code === 'invalid_code') error = `That code is wrong.${err.failuresLeft !== undefined ? ` ${err.failuresLeft} ${err.failuresLeft === 1 ? 'try' : 'tries'} left before you are signed out.` : ''}`;
			else if (err.code === 'code_reused') error = 'That code was already used. Wait for the next one.';
			else if (err.code === 'network') error = NETWORK_MESSAGE;
			else error = err.message;
			field?.focus();
		} finally {
			busy = false;
		}
	}
</script>

<dialog bind:this={dialog} aria-labelledby="stepup-title" aria-describedby="stepup-body" oncancel={(e) => { e.preventDefault(); finish(false); }} onclose={() => finish(false)}>
	<form onsubmit={submit} aria-busy={busy} novalidate>
		<h2 id="stepup-title">Confirm it is you</h2>
		<p id="stepup-body">This change needs a fresh code from your authenticator app.</p>
		{#if countdown.active}<p class="banner">Try again in {countdown.text}</p>{/if}
		<!-- One announcement path: the error is the field's own (aria-describedby, aria-invalid) and focus returns to the field, so it is read once. -->
		<Field id="stepup-code" label="Code from your authenticator" bind:value={code} bind:el={field} inputmode="numeric" autocomplete="one-time-code" maxlength={7} {error} />
		<div class="actions">
			<button type="button" onclick={() => finish(false)}>Cancel</button>
			<button type="submit" class="primary" aria-disabled={busy || countdown.active}>Confirm</button>
		</div>
	</form>
</dialog>

<style>
	dialog {
		max-width: min(28rem, calc(100vw - 2rem));
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
