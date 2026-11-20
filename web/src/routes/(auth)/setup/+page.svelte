<script lang="ts">
	import { goto } from '$app/navigation';
	import { onDestroy, tick } from 'svelte';
	import { ApiError, NETWORK_MESSAGE, setupComplete, setupEnroll, type Enrollment, type SignedIn } from '../../../lib/api';
	import { Countdown } from '../../../lib/countdown.svelte';
	import { copyText } from '../../../lib/copy';
	import { flash } from '../../../lib/flow.svelte';
	import Alerts from '../../../lib/Alerts.svelte';
	import Field from '../../../lib/Field.svelte';
	import PageHeading from '../../../lib/PageHeading.svelte';
	import Qr from '../../../lib/Qr.svelte';

	let { data } = $props();

	type Stage = 'token' | 'account' | 'enroll' | 'recovery';
	let stage: Stage = $state('token');
	let token = $state('');
	let username = $state('admin');
	let password = $state('');
	let showPassword = $state(false);
	let code = $state('');
	let enrollment: Enrollment | undefined = $state();
	let recovery: string[] = $state([]);
	let saved = $state(false);
	let signedIn: SignedIn | undefined;

	let busy = $state(false);
	let error: string | null = $state(null);
	let links: { id: string; text: string }[] = $state([]);
	let status = $state('');
	let fieldError: Record<string, string> = $state({});
	let alerts: Alerts | undefined = $state();
	let tokenEl: HTMLInputElement | undefined = $state();
	const countdown = new Countdown((m) => (status = m));
	onDestroy(() => countdown.stop());

	const st = $derived(data.status);
	const step = $derived(stage === 'token' ? 1 : 2);
	const title = $derived(
		{ token: 'Prove you own this server', account: 'Admin account and two-factor', enroll: 'Add your authenticator', recovery: 'Save your recovery codes' }[stage]
	);
	const expires = $derived(st?.token_expires_at ? new Date(st.token_expires_at) : null);
	const utc = (d: Date) => d.toISOString().slice(11, 16) + ' UTC';
	const utcSeconds = (iso: string) => iso.slice(11, 19) + ' UTC';
	const grouped = $derived((enrollment?.secret ?? '').replace(/\s/g, '').replace(/(.{4})/g, '$1 ').trim());
	const locked = $derived(busy || countdown.active);

	async function go(next: Stage) {
		stage = next;
		clear();
		await tick();
		document.querySelector<HTMLElement>('h1[tabindex="-1"]')?.focus();
	}
	function clear() {
		error = null;
		links = [];
		fieldError = {};
		status = '';
	}
	async function fail(msg: string, l: { id: string; text: string }[] = []) {
		error = msg;
		links = l;
		await tick();
		alerts?.focus();
	}

	/** One plain message per error, A6. Returns true when it was handled here. */
	async function describe(e: unknown, where: Stage): Promise<void> {
		if (!(e instanceof ApiError)) throw e;
		switch (e.code) {
			case 'network':
				return fail(NETWORK_MESSAGE);
			case 'invalid_token':
			case 'setup_token_expired':
			case 'token_consumed':
				await go('token');
				fieldError = { token: e.message };
				return fail(e.message, [{ id: 'token', text: 'Setup token' }]);
			case 'enrollment_replaced':
				await go('account');
				return fail(e.message);
			case 'invalid_code':
			case 'code_reused': {
				const server = st ? ` The server's time is ${utcSeconds(st.server_time)} when this page loaded; if your device's clock is far from that, fix it and try again.` : '';
				const msg = e.code === 'code_reused' ? 'That code was already used. Wait for the next one.' : `That code is wrong. Try the next code your app shows.${server}`;
				fieldError = { code: msg };
				return fail(msg, [{ id: 'code', text: 'Code from your authenticator' }]);
			}
			case 'invalid_input':
				if (where === 'enroll') {
					await go('account');
					fieldError = /password/i.test(e.message) ? { password: e.message } : { username: e.message };
					return fail(e.message, [{ id: /password/i.test(e.message) ? 'password' : 'username', text: /password/i.test(e.message) ? 'Password' : 'Username' }]);
				}
				return fail(e.message);
			case 'rate_limited':
				countdown.start(e.retryAfter ?? 60);
				return;
			case 'busy':
			case 'audit_unavailable':
				return fail('The server is busy. Try again in a moment. Nothing was changed.');
			default:
				return fail(e.message);
		}
	}

	async function submitToken(e: SubmitEvent) {
		e.preventDefault();
		if (locked) return;
		clear();
		token = token.trim();
		if (!token) {
			fieldError = { token: 'Enter the setup token.' };
			return fail('Enter the setup token to continue.', [{ id: 'token', text: 'Setup token' }]);
		}
		await go('account');
	}

	async function submitAccount(e: SubmitEvent) {
		e.preventDefault();
		if (locked) return;
		clear();
		const u = username.trim();
		const problems: { id: string; text: string }[] = [];
		if (!u) {
			fieldError.username = 'Enter a username.';
			problems.push({ id: 'username', text: 'Username' });
		} else if (!/^[A-Za-z0-9._@-]{1,64}$/.test(u)) {
			fieldError.username = 'Use 1 to 64 letters, digits and . _ - @ only.';
			problems.push({ id: 'username', text: 'Username' });
		}
		if ([...password].length < 12) {
			fieldError.password = 'The password must have at least 12 characters.';
			problems.push({ id: 'password', text: 'Password' });
		}
		if (problems.length) return fail('Fix the highlighted fields.', problems);
		username = u;
		busy = true;
		status = 'Checking…';
		try {
			enrollment = await setupEnroll(token, u);
			status = '';
			await go('enroll');
		} catch (err) {
			status = '';
			await describe(err, 'account');
		} finally {
			busy = false;
		}
	}

	async function submitCode(e: SubmitEvent) {
		e.preventDefault();
		if (locked || !enrollment) return;
		clear();
		const c = code.replace(/\s/g, '');
		if (!/^\d{6}$/.test(c)) {
			fieldError = { code: 'Enter the 6 digits from your app.' };
			return fail('Enter the 6-digit code.', [{ id: 'code', text: 'Code from your authenticator' }]);
		}
		busy = true;
		status = 'Creating the account…';
		try {
			signedIn = await setupComplete({ token, enrollment_id: enrollment.enrollment_id, username, password, code: c });
			recovery = signedIn.recovery_codes ?? [];
			password = '';
			token = '';
			code = '';
			status = '';
			await go('recovery');
		} catch (err) {
			status = '';
			code = '';
			await describe(err, 'enroll');
		} finally {
			busy = false;
		}
	}

	async function copy(text: string, what: string) {
		status = (await copyText(text)) ? `${what} copied.` : `Could not copy. Select the text and copy it yourself.`;
	}
	function download() {
		const url = URL.createObjectURL(new Blob([recovery.join('\n') + '\n'], { type: 'text/plain' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = 'serverdock-recovery-codes.txt';
		a.click();
		URL.revokeObjectURL(url);
		status = 'Download started.';
	}

	// Until the codes are confirmed saved, leaving the page asks first.
	function guard(e: BeforeUnloadEvent) {
		if (stage === 'recovery' && !saved) e.preventDefault();
	}
	async function finish(e: Event) {
		e.preventDefault();
		if (!saved) {
			status = 'Tick the box to confirm you saved the codes first.';
			return;
		}
		flash.audit = signedIn?.audit;
		recovery = [];
		await goto('/');
	}
</script>

<svelte:window onbeforeunload={guard} />

<svelte:head>
	<title>{title} · ServerDock</title>
</svelte:head>

<ol class="stepper" aria-label="Setup progress">
	<li aria-current={step === 1 ? 'step' : undefined}>Step 1 of 2: Prove you own this server</li>
	<li aria-current={step === 2 ? 'step' : undefined}>Step 2 of 2: Admin account and two-factor</li>
</ol>

<PageHeading>{title}</PageHeading>

{#if data.loadError}
	<p role="alert">{data.loadError}</p>
{/if}

<Alerts bind:this={alerts} {error} {links} {status} />
{#if countdown.active}
	<p class="banner" aria-hidden="true">Try again in {countdown.text}</p>
{/if}

{#if stage === 'token'}
	<form onsubmit={submitToken} aria-busy={busy} novalidate>
		<p>ServerDock wrote a one-time setup token to a file on this server, so only someone with access to the server can read it.</p>
		{#if st?.token_file}
			<p id="where">Read it on the server with:</p>
			<div class="codeblock">
				<code aria-labelledby="where">sudo cat {st.token_file}</code>
				<button type="button" onclick={() => copy(`sudo cat ${st.token_file}`, 'Command')}>Copy command</button>
			</div>
		{/if}
		<p id="expiry">
			{#if expires}
				This token works until {utc(expires)}. After that, restart ServerDock to get a new one.
			{:else}
				There is no setup token right now. Restart ServerDock to get a new one.
			{/if}
		</p>
		<p>
			Before you type it, check this page's certificate: compare its details with the fingerprint ServerDock printed in its log when it started, or open this page through an SSH tunnel.
			<a href="https://github.com/Docker-Hunterpedia/serverdock/blob/main/docs/security.md" rel="noopener">How to check (opens GitHub)</a>
		</p>
		<Field id="token" label="Setup token" bind:value={token} bind:el={tokenEl} autocomplete="off" hint="Paste the whole line from the file. It starts with sd_setup_." error={fieldError.token} />
		<div class="actions">
			<button type="submit" class="primary" aria-disabled={locked}>Continue</button>
		</div>
	</form>
{:else if stage === 'account'}
	<form onsubmit={submitAccount} aria-busy={busy} novalidate>
		<p>Choose the administrator's sign-in. You will add an authenticator app next.</p>
		<Field id="username" label="Username" bind:value={username} autocomplete="username" hint="Letters, digits and . _ - @ only." error={fieldError.username} />
		<Field
			id="password"
			label="Password"
			bind:value={password}
			type={showPassword ? 'text' : 'password'}
			autocomplete="new-password"
			hint="At least 12 characters. A few ordinary words work well. No other rules."
			error={fieldError.password}
		/>
		<p><button type="button" class="link" aria-pressed={showPassword} onclick={() => (showPassword = !showPassword)}>{showPassword ? 'Hide password' : 'Show password'}</button></p>
		<div class="actions">
			<button type="button" onclick={() => go('token')}>Back</button>
			<button type="submit" class="primary" aria-disabled={locked}>Continue</button>
		</div>
	</form>
{:else if stage === 'enroll' && enrollment}
	<form onsubmit={submitCode} aria-busy={busy} novalidate>
		<p>In your authenticator app, add an account with this key. The QR code below holds the same key.</p>
		<p id="key-label" class="hint">Setup key for {username}</p>
		<div class="codeblock">
			<code aria-labelledby="key-label">{grouped}</code>
			<button type="button" onclick={() => copy(enrollment!.secret, 'Key')}>Copy key</button>
		</div>
		<Qr text={enrollment.otpauth_uri} label="QR code for your authenticator app" />
		<p>
			On this device? <a href={enrollment.otpauth_uri}>Open in authenticator app on this device</a>
		</p>
		<Field id="code" label="Code from your authenticator" bind:value={code} inputmode="numeric" autocomplete="one-time-code" maxlength={7} hint="The 6 digits your app shows now. Nothing is sent until you press Create account." error={fieldError.code} />
		<div class="actions">
			<button type="button" onclick={() => go('account')}>Back</button>
			<button type="submit" class="primary" aria-disabled={locked}>Create account</button>
		</div>
	</form>
{:else if stage === 'recovery'}
	<p>If you lose your authenticator, each of these codes signs you in once. Keep them somewhere safe and private. They are shown only now.</p>
	<ol class="codes" aria-label="Recovery codes">
		{#each recovery as c (c)}
			<li><code>{c}</code></li>
		{/each}
	</ol>
	<div class="actions">
		<button type="button" onclick={() => copy(recovery.join('\n'), 'Codes')}>Copy all</button>
		<button type="button" onclick={download}>Download as text</button>
	</div>
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
{/if}

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
