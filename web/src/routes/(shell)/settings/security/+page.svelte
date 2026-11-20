<script lang="ts">
	import { goto } from '$app/navigation';
	import { onDestroy, onMount, tick } from 'svelte';
	import {
		ApiError,
		Cancelled,
		NETWORK_MESSAGE,
		runAction,
		setAuditHandler,
		setStepUpHandler,
		signedOutReason,
		withStepUp,
		type Enrollment,
		type PanelCert,
		type SessionInfo,
		type SessionRow,
		type TokenCreated,
		type TokenRow
	} from '../../../../lib/api';
	import { copyText } from '../../../../lib/copy';
	import { flash } from '../../../../lib/flow.svelte';
	import { absolute, daysUntil } from '../../../../lib/time';
	import Alerts from '../../../../lib/Alerts.svelte';
	import ConfirmDialog from '../../../../lib/ConfirmDialog.svelte';
	import Field from '../../../../lib/Field.svelte';
	import PageHeading from '../../../../lib/PageHeading.svelte';
	import Qr from '../../../../lib/Qr.svelte';
	import RecoveryCodes from '../../../../lib/RecoveryCodes.svelte';
	import ResponsiveTable from '../../../../lib/ResponsiveTable.svelte';
	import StepUpDialog from '../../../../lib/StepUpDialog.svelte';

	let info: SessionInfo | undefined = $state();
	let sessions: SessionRow[] = $state([]);
	let tokens: TokenRow[] = $state([]);
	let cert: PanelCert | undefined = $state();
	let loaded = $state(false);

	let error: string | null = $state(null);
	let links: { id: string; text: string }[] = $state([]);
	// An action that went through but whose audit record was not written is a warning, never plain success.
	let unrecorded = false;
	let status = $state('');
	let alerts: Alerts | undefined = $state();
	let confirm: ConfirmDialog | undefined = $state();
	let stepUp: StepUpDialog | undefined = $state();

	// ---- loading ---------------------------------------------------------------
	async function fail(msg: string, l: { id: string; text: string }[] = []) {
		error = msg;
		links = l;
		await tick();
		alerts?.focus();
	}
	/** Maps any action error to the page's one alert; true when it was handled. */
	async function handle(e: unknown): Promise<void> {
		if (e instanceof Cancelled) return;
		if (!(e instanceof ApiError)) throw e;
		if (e.status === 401) return void (await goto(`/signed-out?reason=${signedOutReason(e.code)}`));
		if (e.code === 'network') return fail(NETWORK_MESSAGE);
		return fail(e.message);
	}
	async function load() {
		try {
			[info, { sessions }, { tokens }, cert] = await Promise.all([
				runAction<SessionInfo>('auth.session_info'),
				runAction<{ sessions: SessionRow[] }>('auth.sessions_list'),
				runAction<{ tokens: TokenRow[] }>('tokens.list'),
				runAction<PanelCert>('system.panel_cert')
			]);
			loaded = true;
		} catch (e) {
			await handle(e);
		}
	}
	const AUDIT_WARNING = 'The change went through, but its audit record could not be written. Check the server disk and logs.';
	onMount(() => {
		setStepUpHandler(() => stepUp!.ask());
		setAuditHandler(() => {
			unrecorded = true;
			void fail(AUDIT_WARNING);
		});
		// A token shown once does not stay on screen when you look away.
		const onHidden = () => document.visibilityState === 'hidden' && (created = undefined);
		document.addEventListener('visibilitychange', onHidden);
		void load();
		return () => document.removeEventListener('visibilitychange', onHidden);
	});
	onDestroy(() => {
		setStepUpHandler(undefined);
		setAuditHandler(undefined);
	});
	/** Starting any action clears the shown-once token and the previous message. */
	function begin() {
		created = undefined;
		error = null;
		links = [];
		unrecorded = false;
	}
	/** Where an action that ends the session carries its audit warning. */
	function endSession(reason: string) {
		if (unrecorded) flash.audit = 'unrecorded';
		return goto(`/signed-out?reason=${reason}`);
	}

	const others = $derived(sessions.filter((s) => !s.current));
	const certDays = $derived(cert ? daysUntil(cert.not_after) : 0);
	const certWarn = $derived(!!cert && certDays < 60);
	const activeTokens = $derived(tokens.filter((t) => !t.revoked && new Date(t.expires_at).getTime() > Date.now()).length);

	function tokenState(t: TokenRow): { symbol: string; word: string } {
		if (t.revoked) return { symbol: '◌', word: 'Revoked' };
		if (new Date(t.expires_at).getTime() <= Date.now()) return { symbol: '▲', word: 'Expired' };
		return { symbol: '●', word: 'Active' };
	}

	function focusAfterRemoval(table: string, index: number) {
		return tick().then(() => {
			const buttons = [...document.querySelectorAll<HTMLElement>(`[data-table="${table}"] .row-action`)].filter((b) => b.offsetParent !== null);
			(buttons[index] ?? buttons[buttons.length - 1] ?? document.getElementById(`${table}-heading`))?.focus();
		});
	}

	// ---- sessions ---------------------------------------------------------------
	async function revokeSession(s: SessionRow, index: number) {
		begin();
		const device = s.user_agent || 'an unknown device';
		const ok = await confirm!.ask(
			s.current
				? { title: 'Sign out this device?', lines: [`This is the session you are using (${device}, ${s.ip_key}). You will be signed out and taken to the sign-in page.`], confirm: 'Sign out this device' }
				: { title: 'Sign out this session?', lines: [`${device}, ${s.ip_key}, last active ${absolute(s.last_seen_at)}. It stops working at once.`], confirm: 'Sign out this session' }
		);
		if (!ok) return;
		try {
			await withStepUp(() => runAction('auth.sessions_revoke', { public_id: s.public_id }));
			if (s.current) return void (await endSession('signed_out'));
			sessions = sessions.filter((x) => x.public_id !== s.public_id);
			status = 'Session signed out.';
			if (!unrecorded) await focusAfterRemoval('sessions', index); // a warning alert keeps focus
		} catch (e) {
			await handle(e);
		}
	}
	async function revokeOthers() {
		begin();
		const n = others.length;
		if (n === 0) {
			status = 'There are no other sessions to sign out.';
			return;
		}
		const ok = await confirm!.ask({
			title: 'Sign out everywhere else?',
			lines: [`${n} other ${n === 1 ? 'session' : 'sessions'} will be signed out. This device stays signed in.`],
			confirm: `Sign out ${n} ${n === 1 ? 'session' : 'sessions'}`
		});
		if (!ok) return;
		try {
			await withStepUp(() => runAction('auth.sessions_revoke', { all_others: true }));
			sessions = sessions.filter((x) => x.current);
			status = `${n} ${n === 1 ? 'session' : 'sessions'} signed out.`;
			await tick();
			if (!unrecorded) document.getElementById('sessions-heading')?.focus();
		} catch (e) {
			await handle(e);
		}
	}

	// ---- API tokens --------------------------------------------------------------
	let tokenName = $state('');
	let tokenScope = $state('read');
	let tokenDays = $state('90');
	let tokenErrors: Record<string, string> = $state({});
	let created: TokenCreated | undefined = $state();
	let busyToken = $state(false);

	async function createToken(e: SubmitEvent) {
		e.preventDefault();
		if (busyToken) return;
		begin();
		tokenErrors = {};
		const days = Number(tokenDays);
		if (!tokenName.trim()) tokenErrors.name = 'Say what the token is for.';
		if (!Number.isInteger(days) || days < 1 || days > 365) tokenErrors.days = 'Enter a whole number of days from 1 to 365.';
		if (Object.keys(tokenErrors).length) return fail('Fix the highlighted fields.', [...(tokenErrors.name ? [{ id: 'token-name', text: 'Name' }] : []), ...(tokenErrors.days ? [{ id: 'token-days', text: 'Expires in (days)' }] : [])]);
		busyToken = true;
		status = 'Creating the token…';
		try {
			created = await withStepUp(() => runAction<TokenCreated>('tokens.create', { name: tokenName.trim(), scope: tokenScope, expires_in_days: days }));
			tokenName = '';
			status = 'Token created. It is shown once.';
			tokens = (await runAction<{ tokens: TokenRow[] }>('tokens.list')).tokens;
			await tick();
			document.getElementById('new-token-heading')?.focus();
		} catch (err) {
			status = '';
			await handle(err);
		} finally {
			busyToken = false;
		}
	}
	async function revokeToken(t: TokenRow, index: number) {
		begin();
		const ok = await confirm!.ask({
			title: 'Revoke this token?',
			lines: [`"${t.name}" (sd_${t.prefix}…, ${t.scope}). Anything using it stops working at once. This cannot be undone.`],
			confirm: 'Revoke token'
		});
		if (!ok) return;
		try {
			await withStepUp(() => runAction('tokens.revoke', { id: t.id }));
			tokens = tokens.map((x) => (x.id === t.id ? { ...x, revoked: true } : x));
			status = `Token "${t.name}" revoked.`;
			if (!unrecorded) await focusAfterRemoval('tokens', index);
		} catch (e) {
			await handle(e);
		}
	}
	async function hideToken() {
		created = undefined;
		await tick();
		document.getElementById('tokens-heading')?.focus(); // the button is gone: focus goes to the section
	}
	async function copyToken() {
		status = created && (await copyText(created.token)) ? 'Token copied.' : 'Could not copy. Select the token and copy it yourself.';
	}

	// ---- password ------------------------------------------------------------------
	let currentPassword = $state('');
	let newPassword = $state('');
	let keepTokensPw = $state(false);
	let pwErrors: Record<string, string> = $state({});

	async function changePassword(e: SubmitEvent) {
		e.preventDefault();
		begin();
		pwErrors = {};
		if (!currentPassword) pwErrors.current = 'Enter your current password.';
		if ([...newPassword].length < 12) pwErrors.next = 'The new password must have at least 12 characters.';
		else if (new TextEncoder().encode(newPassword).length > 256) pwErrors.next = 'The new password is longer than 256 bytes.';
		else if (info && newPassword.toLowerCase() === info.username.toLowerCase()) pwErrors.next = 'The new password must differ from the username.';
		if (Object.keys(pwErrors).length) return fail('Fix the highlighted fields.', [...(pwErrors.current ? [{ id: 'current-password', text: 'Current password' }] : []), ...(pwErrors.next ? [{ id: 'new-password', text: 'New password' }] : [])]);
		const n = sessions.length;
		const ok = await confirm!.ask({
			title: 'Change the password?',
			lines: [
				`All ${n} ${n === 1 ? 'session' : 'sessions'}, including this one, will be signed out.`,
				keepTokensPw ? `Your ${activeTokens} active API ${activeTokens === 1 ? 'token' : 'tokens'} will be kept.` : `Your ${activeTokens} active API ${activeTokens === 1 ? 'token' : 'tokens'} will be revoked.`
			],
			confirm: 'Change password and sign out'
		});
		if (!ok) return;
		try {
			await withStepUp(() => runAction('auth.password_change', { current_password: currentPassword, new_password: newPassword, keep_tokens: keepTokensPw }));
			currentPassword = newPassword = '';
			await endSession('credentials');
		} catch (err) {
			currentPassword = '';
			if (err instanceof ApiError && err.code === 'wrong_password') {
				pwErrors = { current: 'That is not your current password.' };
				return fail('That is not your current password.', [{ id: 'current-password', text: 'Current password' }]);
			}
			await handle(err);
		}
	}

	// ---- authenticator ----------------------------------------------------------------
	let enrolling: Enrollment | undefined = $state();
	let reCode = $state('');
	let keepTokensTotp = $state(false);
	let reErrors: Record<string, string> = $state({});
	const grouped = $derived((enrolling?.secret ?? '').replace(/\s/g, '').replace(/(.{4})/g, '$1 ').trim());

	async function beginReenroll() {
		begin();
		try {
			enrolling = await withStepUp(() => runAction<Enrollment>('auth.totp_reenroll_begin'));
			reCode = '';
			reErrors = {};
			status = 'Add the new key to your authenticator app, then enter a code from it.';
			await tick();
			document.getElementById('reenroll-heading')?.focus();
		} catch (e) {
			await handle(e);
		}
	}
	async function cancelReenroll() {
		enrolling = undefined;
		status = '';
		await tick();
		document.getElementById('reenroll-button')?.focus();
	}
	async function confirmReenroll(e: SubmitEvent) {
		e.preventDefault();
		if (!enrolling) return;
		begin();
		reErrors = {};
		const c = reCode.replace(/\s/g, '');
		if (!/^\d{6}$/.test(c)) {
			reErrors.code = 'Enter the 6 digits from your app.';
			return fail('Enter the 6-digit code.', [{ id: 're-code', text: 'Code from the new authenticator' }]);
		}
		const n = sessions.length;
		const ok = await confirm!.ask({
			title: 'Replace the authenticator?',
			lines: [
				'Codes from your old authenticator stop working.',
				`All ${n} ${n === 1 ? 'session' : 'sessions'}, including this one, will be signed out.`,
				keepTokensTotp ? `Your ${activeTokens} active API ${activeTokens === 1 ? 'token' : 'tokens'} will be kept.` : `Your ${activeTokens} active API ${activeTokens === 1 ? 'token' : 'tokens'} will be revoked.`
			],
			confirm: 'Replace authenticator and sign out'
		});
		if (!ok) return;
		try {
			await withStepUp(() => runAction('auth.totp_reenroll_confirm', { enrollment_id: enrolling!.enrollment_id, code: c, keep_tokens: keepTokensTotp }));
			enrolling = undefined;
			await endSession('credentials');
		} catch (err) {
			reCode = '';
			if (err instanceof ApiError && (err.code === 'enrollment_expired' || err.code === 'enrollment_replaced')) {
				enrolling = undefined;
				return fail(`${err.message} Replace the authenticator again to get a new key.`);
			}
			if (err instanceof ApiError && (err.code === 'invalid_code' || err.code === 'code_reused')) {
				reErrors.code = err.code === 'code_reused' ? 'That code was already used. Wait for the next one.' : 'That code is wrong. Try the next code your app shows.';
				return fail(reErrors.code, [{ id: 're-code', text: 'Code from the new authenticator' }]);
			}
			await handle(err);
		}
	}

	// ---- recovery codes -------------------------------------------------------------------
	let newCodes: string[] = $state([]);
	async function regenerate() {
		begin();
		const left = info?.recovery_codes_left ?? 0;
		const ok = await confirm!.ask({
			title: 'Make new recovery codes?',
			lines: [`Your ${left} unused ${left === 1 ? 'code' : 'codes'} will stop working. The new ones are shown once.`],
			confirm: 'Make new codes'
		});
		if (!ok) return;
		try {
			const r = await withStepUp(() => runAction<{ codes: string[] }>('auth.recovery_regenerate'));
			newCodes = r.codes;
			if (info) info.recovery_codes_left = r.codes.length;
			await tick();
			document.getElementById('codes-heading')?.focus();
		} catch (e) {
			await handle(e);
		}
	}
</script>

<svelte:head>
	<title>Security · ServerDock</title>
</svelte:head>

<nav class="subnav" aria-label="Settings">
	<ul>
		<li><a href="/settings/security" aria-current="page">Security</a></li>
	</ul>
</nav>

<PageHeading>Security</PageHeading>

<Alerts bind:this={alerts} {error} {links} {status} />

<!-- Always in the page, filled once the certificate is known, so the warning is announced. -->
<div role="status" class="cert-status">
	{#if loaded && cert && certWarn}
		<p class="warn">
			<span aria-hidden="true">▲</span>
			The panel certificate {certDays < 0 ? 'has expired' : `expires in ${certDays} ${certDays === 1 ? 'day' : 'days'}`} ({absolute(cert.not_after)}). ServerDock renews it when it starts: restart ServerDock soon.
		</p>
	{/if}
</div>

{#if !loaded && !error}
	<p>Loading…</p>
{/if}

{#if loaded && info && cert}
	<section aria-labelledby="panel-heading">
		<h2 id="panel-heading" tabindex="-1">This server</h2>
		<dl class="facts">
			<div><dt>Panel certificate fingerprint (SHA-256)</dt><dd><code id="fingerprint">{cert.fingerprint}</code></dd></div>
			<div><dt>Certificate created</dt><dd>{absolute(cert.created)}</dd></div>
			<div><dt>Certificate expires</dt><dd>{absolute(cert.not_after)}</dd></div>
			<div>
				<dt>Admin created</dt>
				<dd id="admin-created">{info.admin_created_from || info.admin_created_at ? `Admin created from ${info.admin_created_from || 'an unknown address'} at ${absolute(info.admin_created_at)}` : 'Not recorded'}</dd>
			</div>
		</dl>
	</section>

	<section aria-labelledby="sessions-heading" data-table="sessions">
		<h2 id="sessions-heading" tabindex="-1">Sessions</h2>
		<ResponsiveTable
			caption="Signed-in sessions"
			columns={[
				{ key: 'device', label: 'Device' },
				{ key: 'ip', label: 'Address' },
				{ key: 'since', label: 'Signed in' },
				{ key: 'seen', label: 'Last active' },
				{ key: 'state', label: 'State' },
				{ key: 'action', label: 'Action' }
			]}
			rows={sessions}
			rowKey={(s) => s.public_id}
			rowLabel={(s) => s.user_agent}
			empty="No sessions."
		>
			{#snippet cell(s, key)}
				{#if key === 'device'}{s.user_agent || 'Unknown device'}
				{:else if key === 'ip'}{s.ip_key}
				{:else if key === 'since'}{absolute(s.created_at)}
				{:else if key === 'seen'}{absolute(s.last_seen_at)}
				{:else if key === 'state'}<span aria-hidden="true">●</span> {s.current ? 'This device' : 'Active'}
				{:else}
					<button type="button" class="row-action" onclick={() => revokeSession(s, sessions.indexOf(s))}>
						{#if s.current}Sign out this device{:else}Sign out<span class="sr"> session from {s.ip_key}, {s.user_agent || 'unknown device'}</span>{/if}
					</button>
				{/if}
			{/snippet}
		</ResponsiveTable>
		<p>
			<button type="button" onclick={revokeOthers} aria-disabled={others.length === 0} class="others">
				Sign out everywhere else ({others.length} other {others.length === 1 ? 'session' : 'sessions'})
			</button>
		</p>
	</section>

	<section aria-labelledby="tokens-heading" data-table="tokens">
		<h2 id="tokens-heading" tabindex="-1">API tokens</h2>
		<p class="hint">A token lets a script or an AI client call ServerDock without your password. It can never change credentials or run destructive actions.</p>
		<ResponsiveTable
			caption="API tokens"
			columns={[
				{ key: 'name', label: 'Name' },
				{ key: 'prefix', label: 'Prefix' },
				{ key: 'scope', label: 'Scope' },
				{ key: 'expires', label: 'Expires' },
				{ key: 'used', label: 'Last used' },
				{ key: 'state', label: 'State' },
				{ key: 'action', label: 'Action' }
			]}
			rows={tokens}
			rowKey={(t) => t.id}
			rowLabel={(t) => t.name}
			empty="No API tokens yet."
		>
			{#snippet cell(t, key)}
				{#if key === 'name'}{t.name}
				{:else if key === 'prefix'}<code>sd_{t.prefix}…</code>
				{:else if key === 'scope'}{t.scope}
				{:else if key === 'expires'}{absolute(t.expires_at)}
				{:else if key === 'used'}{absolute(t.last_used_at)}
				{:else if key === 'state'}<span aria-hidden="true">{tokenState(t).symbol}</span> {tokenState(t).word}
				{:else if !t.revoked}
					<button type="button" class="row-action" onclick={() => revokeToken(t, tokens.indexOf(t))}>Revoke<span class="sr"> token {t.name} (sd_{t.prefix}…)</span></button>
				{:else}<span class="muted">None</span>
				{/if}
			{/snippet}
		</ResponsiveTable>

		{#if created}
			<div class="banner" role="group" aria-labelledby="new-token-heading">
				<h3 id="new-token-heading" tabindex="-1">Your new token</h3>
				<p>Copy it now. It is shown only once and cannot be looked up later.</p>
				<div class="codeblock"><code id="new-token">{created.token}</code><button type="button" onclick={copyToken}>Copy token</button></div>
				<p class="hint">{created.scope} scope, expires {absolute(created.expires_at)}.</p>
				<button type="button" onclick={hideToken}>I have copied it, hide it</button>
			</div>
		{/if}

		<form onsubmit={createToken} aria-busy={busyToken} novalidate>
			<h3>Create a token</h3>
			<Field id="token-name" label="Name" bind:value={tokenName} autocomplete="off" hint="What it is for, for example “backup script”." error={tokenErrors.name} />
			<div class="field">
				<label for="token-scope">Scope</label>
				<p id="token-scope-hint" class="hint">Read can only look; write can change non-sensitive things; admin can do everything an API token may.</p>
				<select id="token-scope" bind:value={tokenScope} aria-describedby="token-scope-hint">
					<option value="read">Read</option>
					<option value="write">Write</option>
					<option value="admin">Admin</option>
				</select>
			</div>
			<Field id="token-days" label="Expires in (days)" bind:value={tokenDays} inputmode="numeric" hint="1 to 365. 90 is a good default." error={tokenErrors.days} />
			<div class="actions"><button type="submit" class="primary" aria-disabled={busyToken}>Create token</button></div>
		</form>
	</section>

	<section aria-labelledby="password-heading">
		<h2 id="password-heading" tabindex="-1">Password</h2>
		<form onsubmit={changePassword} novalidate>
			<!-- For password managers: which account this password belongs to. -->
			<input class="sr" type="text" name="username" autocomplete="username" value={info.username} readonly tabindex="-1" aria-hidden="true" />
			<Field id="current-password" label="Current password" bind:value={currentPassword} type="password" autocomplete="current-password" error={pwErrors.current} />
			<Field id="new-password" label="New password" bind:value={newPassword} type="password" autocomplete="new-password" hint="At least 12 characters. A few ordinary words work well." error={pwErrors.next} />
			<p class="check-row">
				<label class="check"><input type="checkbox" bind:checked={keepTokensPw} aria-describedby="keep-pw-hint" /> Keep API tokens</label>
			</p>
			<p id="keep-pw-hint" class="hint">Tick this only if you are rotating routinely, not after a suspected compromise.</p>
			<div class="actions"><button type="submit" class="primary">Change password…</button></div>
		</form>
	</section>

	<section aria-labelledby="totp-heading">
		<h2 id="totp-heading" tabindex="-1">Authenticator</h2>
		{#if !enrolling}
			<p>Replace the app that makes your sign-in codes, for example after changing phones.</p>
			<div class="actions"><button type="button" id="reenroll-button" onclick={beginReenroll}>Replace authenticator…</button></div>
		{:else}
			<form onsubmit={confirmReenroll} novalidate>
				<h3 id="reenroll-heading" tabindex="-1">Add the new key</h3>
				<p id="re-key-label" class="hint">New setup key</p>
				<div class="codeblock" role="group" aria-labelledby="re-key-label">
					<code id="re-key">{grouped}</code>
					<button type="button" onclick={async () => (status = (await copyText(enrolling!.secret)) ? 'Key copied.' : 'Could not copy.')}>Copy key</button>
				</div>
				<p class="hint">Clipboard history apps may keep a copy of what you copy here. Typing the key into your app is safer.</p>
				<Qr text={enrolling.otpauth_uri} label="QR code for your authenticator app" />
				<p><a href={enrolling.otpauth_uri}>Open in authenticator app on this device</a></p>
				<Field id="re-code" label="Code from the new authenticator" bind:value={reCode} inputmode="numeric" autocomplete="one-time-code" maxlength={7} error={reErrors.code} />
				<p class="check-row">
					<label class="check"><input type="checkbox" bind:checked={keepTokensTotp} aria-describedby="keep-totp-hint" /> Keep API tokens</label>
				</p>
				<p id="keep-totp-hint" class="hint">Tick this only if you are rotating routinely, not after a suspected compromise.</p>
				<div class="actions">
					<button type="button" onclick={cancelReenroll}>Cancel</button>
					<button type="submit" class="primary">Replace authenticator…</button>
				</div>
			</form>
		{/if}
	</section>

	<section aria-labelledby="codes-heading">
		<h2 id="codes-heading" tabindex="-1">Recovery codes</h2>
		{#if newCodes.length}
			<RecoveryCodes
				codes={newCodes}
				lead="Your old codes no longer work. Each of these signs you in once. Keep them somewhere safe and private. They are shown only now."
				onstatus={(m) => (status = m)}
				onfinish={() => {
					newCodes = [];
					status = 'New recovery codes saved.';
					void tick().then(() => document.getElementById('codes-heading')?.focus());
				}}
			/>
		{:else}
			<p id="codes-left">You have {info.recovery_codes_left} unused recovery {info.recovery_codes_left === 1 ? 'code' : 'codes'}.</p>
			<div class="actions"><button type="button" onclick={regenerate}>Make new recovery codes…</button></div>
		{/if}
	</section>
{/if}

<ConfirmDialog bind:this={confirm} />
<StepUpDialog bind:this={stepUp} />

<style>
	section {
		max-width: 56rem;
		margin: 0 0 2rem;
	}
	h2,
	h3 {
		margin: 1rem 0 0.5rem;
	}
	h2:focus,
	h3:focus {
		outline: none;
	}
	.subnav ul {
		display: flex;
		list-style: none;
		gap: 0.5rem;
		margin: 0 0 1rem;
		padding: 0;
	}
	.subnav a {
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
		padding: 0 0.75rem;
		color: var(--text);
		text-decoration: none;
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		font-weight: 700;
	}
	.facts div {
		display: grid;
		grid-template-columns: minmax(10rem, 16rem) 1fr;
		gap: 0.5rem;
		padding: 0.375rem 0;
		border-bottom: 1px solid var(--border);
	}
	.facts dt {
		color: var(--muted);
	}
	.facts dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
	.warn {
		border: 2px solid var(--danger);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
		font-weight: 600;
	}
	.muted {
		color: var(--muted);
	}
	select {
		display: block;
		min-height: var(--target);
		padding: 0.5rem 0.75rem;
		font: inherit;
		color: var(--text);
		background: var(--bg);
		border: 2px solid var(--border);
		border-radius: 0.5rem;
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
	.check-row {
		margin: 0.5rem 0 0;
	}
	@media (max-width: 47.99rem) {
		.facts div {
			grid-template-columns: 1fr;
		}
	}
</style>
