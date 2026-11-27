<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { ApiError, Cancelled, runAction, setStepUpHandler, withStepUp } from '../../../../../../../lib/api';
	import Alerts from '../../../../../../../lib/Alerts.svelte';
	import ConfirmDialog from '../../../../../../../lib/ConfirmDialog.svelte';
	import Field from '../../../../../../../lib/Field.svelte';
	import FormDialog from '../../../../../../../lib/FormDialog.svelte';
	import { listNotice } from '../../../../../../../lib/flow.svelte';
	import { useService } from '../../../../../../../lib/servicectx.svelte';
	import StepUpDialog from '../../../../../../../lib/StepUpDialog.svelte';
	import {
		CONFIRM_AGAIN_FRAGMENT,
		getService,
		SEC6_DOCS_URL,
		appAddress,
		intField,
		mutationMessage,
		panelHost,
		sharesPanelDraft,
		type ServiceChange,
		type UpdateInput
	} from '../../../../../../../lib/services';

	const ctx = useService();
	// The sub-page is only rendered once the service is loaded, so these start from it.
	const first = ctx.service!;

	let stepUp: StepUpDialog | undefined = $state();
	let confirmDialog: ConfirmDialog | undefined = $state();
	let formDialog: FormDialog | undefined = $state();
	onMount(() => {
		setStepUpHandler(() => stepUp!.ask());
		return () => setStepUpHandler(undefined);
	});

	// ---- the service's settings (services.update) -------------------------------
	let containerPort = $state(first.container_port ? String(first.container_port) : '');
	let healthKind = $state(first.health.kind);
	let healthPath = $state(first.health.path || '/');
	let timeoutS = $state(String(first.health.timeout_s));
	let passes = $state(String(first.health.passes));
	let strategy = $state(first.strategy);
	let drainS = $state(String(first.drain_s));
	let graceS = $state(String(first.stop_grace_s));
	let fErrors: Record<string, string> = $state({});
	let fSummary: string | null = $state(null);
	let fLinks: { id: string; text: string }[] = $state([]);
	let fStatus = $state('');
	let fBusy = $state(false);
	let fAlerts: Alerts | undefined = $state();

	const FIELDS: Record<string, string> = {
		containerPort: 'Port your app listens on',
		healthPath: 'Health check path',
		timeoutS: 'Health timeout',
		passes: 'Health passes',
		drainS: 'Drain time',
		graceS: 'Stop grace'
	};
	const idOf = (k: string) => `s-${k}`;

	async function failForm(summary: string, errs: Record<string, string>) {
		fErrors = errs;
		fSummary = summary;
		fLinks = Object.keys(errs).map((k) => ({ id: idOf(k), text: `${FIELDS[k]}: ${errs[k]}` }));
		fStatus = '';
		await Promise.resolve();
		fAlerts?.focus();
	}
	function clearError(e: Event) {
		const id = (e.target as HTMLElement | null)?.id ?? '';
		const k = id.replace(/^s-/, '');
		if (k in fErrors) {
			const { [k]: _gone, ...rest } = fErrors;
			fErrors = rest;
		}
	}

	async function saveFields(e: SubmitEvent) {
		e.preventDefault();
		if (fBusy) return;
		fStatus = '';
		const errs: Record<string, string> = {};
		const cp = intField(containerPort, 1, 65535);
		if (cp === 'bad') errs.containerPort = 'Enter a port from 1 to 65535.';
		else if (cp === undefined && first.container_port) errs.containerPort = 'The port cannot be removed here. Enter a port, or create a new service for a worker.';
		const t = intField(timeoutS, 5, 600);
		if (t === 'bad' || t === undefined) errs.timeoutS = 'Enter a number of seconds from 5 to 600.';
		const pa = intField(passes, 1, 10);
		if (pa === 'bad' || pa === undefined) errs.passes = 'Enter a number from 1 to 10.';
		const dr = intField(drainS, 10, 300);
		if (dr === 'bad' || dr === undefined) errs.drainS = 'Enter a number of seconds from 10 to 300.';
		const gr = intField(graceS, 0, 300);
		if (gr === 'bad' || gr === undefined) errs.graceS = 'Enter a number of seconds from 0 to 300.';
		if (healthKind !== 'running' && cp === undefined) errs.containerPort = 'This health check needs the port your app listens on. Fill it in, or choose "Stays running".';
		if (healthKind === 'http' && !/^\/\S{0,255}$/.test(healthPath.trim())) errs.healthPath = 'Enter a path that starts with /, for example /health.';
		if (Object.keys(errs).length) {
			await failForm(Object.keys(errs).length === 1 ? 'One setting needs fixing before it can be saved.' : `${Object.keys(errs).length} settings need fixing before they can be saved.`, errs);
			return;
		}
		const input: UpdateInput = {
			service: ctx.ref,
			strategy,
			drain_s: dr as number,
			stop_grace_s: gr as number,
			health: { kind: healthKind, ...(healthKind === 'http' ? { path: healthPath.trim() } : {}), timeout_s: t as number, passes: pa as number }
		};
		if (typeof cp === 'number' && cp !== first.container_port) input.container_port = cp;
		fBusy = true;
		fErrors = {};
		fSummary = null;
		fLinks = [];
		try {
			const out = await runAction<ServiceChange>('services.update', input);
			ctx.service = out.service;
			fStatus = out.notice || 'Saved. Deploy to apply.';
		} catch (err) {
			if (!(err instanceof ApiError)) throw err;
			if (err.status === 401) {
				await ctx.fail(err);
				return;
			}
			await failForm(mutationMessage(err), {});
		} finally {
			fBusy = false;
		}
	}

	// ---- the image repository (services.set_source) ----------------------------
	let image = $state(first.image_ref);
	let iError = $state('');
	let iSummary: string | null = $state(null);
	let iStatus = $state('');
	let iBusy = $state(false);
	let iAlerts: Alerts | undefined = $state();

	async function saveSource(e: SubmitEvent) {
		e.preventDefault();
		if (iBusy) return;
		iStatus = '';
		iSummary = null;
		iError = '';
		if (!image.trim()) {
			iError = 'Enter the image to run, for example registry.example.com/shop/api:2.4.';
			iSummary = 'The image repository was not changed. Fix the field below and try again.';
			await Promise.resolve();
			iAlerts?.focus();
			return;
		}
		iBusy = true;
		try {
			const out = await runAction<ServiceChange>('services.set_source', { service: ctx.ref, image_ref: image.trim() });
			ctx.service = out.service;
			image = out.service.image_ref;
			iStatus = out.notice || 'Saved. Deploy to apply.';
		} catch (err) {
			if (!(err instanceof ApiError)) throw err;
			if (err.status === 401) {
				await ctx.fail(err);
				return;
			}
			if (err.status === 400 || err.status === 409) {
				iError = mutationMessage(err).replace(/^[a-z_.]+:\s*/i, '');
				iSummary = 'The image repository was not changed. Fix the field below and try again.';
			} else iSummary = mutationMessage(err);
			await Promise.resolve();
			iAlerts?.focus();
		} finally {
			iBusy = false;
		}
	}

	// ---- the public port (services.set_public_port, services.retry_port) --------
	let publicPort = $state(first.public_port ? String(first.public_port) : '');
	let pError = $state('');
	let pSummary: string | null = $state(null);
	let pStatus = $state('');
	let pNotice = $state(''); // the server's "not applied" reason, kept visible with Try again
	let pBusy = $state(false);
	let pAlerts: Alerts | undefined = $state();
	const svc = $derived(ctx.service ?? first);
	const typedPort = $derived(/^\d+$/.test(publicPort.trim()) ? Number(publicPort.trim()) : null);
	// Try again is for what the server says is waiting: a route error, a stale proxy, routes not ready, or a notice.
	const canRetry = $derived(!!svc.route_error || svc.routes_stale || (!!svc.public_port && !svc.routes_ready) || !!pNotice);

	async function report(out: ServiceChange, applied: string) {
		ctx.service = out.service;
		publicPort = out.service.public_port ? String(out.service.public_port) : '';
		pNotice = out.applied ? '' : (out.notice ?? '');
		pStatus = out.applied ? applied : '';
		if (!out.applied) {
			pSummary = out.notice || 'Saved, but not applied yet. Use Try again.';
			await Promise.resolve();
			pAlerts?.focus();
		}
	}
	/**
	 * A port change reloads the proxy that also serves this panel, so the answer can be
	 * lost on the way. Then the saved state is read back instead of guessing: `want` is
	 * the public port the change should have left (null: cleared).
	 */
	async function portFailure(err: unknown, want?: number | null) {
		if (!(err instanceof ApiError)) throw err;
		if (err.status === 401) {
			await ctx.fail(err);
			return;
		}
		if (err.code === 'network' && want !== undefined) {
			try {
				const now = await getService(ctx.ref);
				ctx.service = now;
				publicPort = now.public_port ? String(now.public_port) : '';
				if ((now.public_port ?? null) === want) {
					pNotice = `The connection dropped while the proxy reloaded. ServerDock checked: the public port is saved as ${want ? appAddress(want) : 'not exposed'}. Whether the proxy applied it is not confirmed: use Try again, and check that the app answers.`;
					pSummary = 'The connection dropped while the proxy reloaded. See the note below, then use Try again.';
				} else {
					pSummary = 'The connection dropped and the public port was not changed. Nothing is applied.';
				}
				await Promise.resolve();
				pAlerts?.focus();
				return;
			} catch {
				/* the server cannot be read either: say so below */
			}
		}
		if (err.status === 400 || err.status === 409) {
			pError = mutationMessage(err).replace(/^[a-z_.]+:\s*/i, '');
			pSummary = 'The public port was not changed. Fix the field below and try again.';
		} else pSummary = mutationMessage(err);
		await Promise.resolve();
		pAlerts?.focus();
	}

	async function savePort(e: SubmitEvent) {
		e.preventDefault();
		if (pBusy) return;
		pStatus = '';
		pSummary = null;
		pError = '';
		const n = intField(publicPort, 1024, 65535);
		if (n === undefined) {
			pError = svc.public_port ? 'Enter a port, or use "Stop exposing" to make the app private.' : 'Enter a port from 1024 to 65535.';
		} else if (n === 'bad' || n === 80 || n === 443) pError = 'Enter a port from 1024 to 65535 (not 80 or 443).';
		else if (!svc.container_port) pError = 'A public port needs the port your app listens on. Set it under Service settings first.';
		else if (n === svc.public_port) pError = `The app is already exposed on port ${n}.`;
		if (pError) {
			pSummary = 'The public port was not changed. Fix the field below and try again.';
			await Promise.resolve();
			pAlerts?.focus();
			return;
		}
		const port = n as number;
		const old = svc.public_port;
		const ok = await confirmDialog?.ask({
			title: old ? `Change the public port of ${ctx.ref} to ${port}?` : `Expose ${ctx.ref} on port ${port}?`,
			lines: [
				`Applied now: exposes the app on the internet at ${appAddress(port)}, unless your host's firewall blocks the port.`,
				...(old ? [`It is no longer reachable at ${appAddress(old)}.`] : []),
				'The app is served over plain HTTP on the same address as this panel. Browsers share cookies by host name, not by port.'
			],
			confirm: old ? `Change to port ${port}` : `Expose on port ${port}`
		});
		if (!ok) {
			pStatus = 'Cancelled. The public port was not changed.';
			return;
		}
		pBusy = true;
		try {
			const out = await runAction<ServiceChange>('services.set_public_port', { service: ctx.ref, public_port: port });
			await report(out, `Applied now: the app is exposed at ${appAddress(port)}.`);
		} catch (err) {
			await portFailure(err, port);
		} finally {
			pBusy = false;
		}
	}

	async function clearPort() {
		if (pBusy || !svc.public_port) return;
		pStatus = '';
		pSummary = null;
		pError = '';
		const old = svc.public_port;
		const ok = await confirmDialog?.ask({
			title: `Stop exposing ${ctx.ref}?`,
			lines: [`Applied now: the app is no longer reachable at ${appAddress(old)}. It keeps running, and other apps on the server can still reach it.`],
			confirm: 'Stop exposing'
		});
		if (!ok) {
			pStatus = 'Cancelled. The public port was not changed.';
			return;
		}
		pBusy = true;
		try {
			const out = await runAction<ServiceChange>('services.set_public_port', { service: ctx.ref, clear: true });
			await report(out, `Applied now: the app is no longer reachable at ${appAddress(old)}.`);
		} catch (err) {
			await portFailure(err, null);
		} finally {
			pBusy = false;
		}
	}

	async function tryAgain() {
		if (pBusy) return;
		pBusy = true;
		pStatus = '';
		pSummary = null;
		pError = '';
		try {
			const out = await runAction<ServiceChange>('services.retry_port', { service: ctx.ref });
			await report(out, 'Applied now: the proxy matches the saved routes.');
			await ctx.load(false);
		} catch (err) {
			await portFailure(err);
		} finally {
			pBusy = false;
		}
	}

	// ---- delete (services.delete) -----------------------------------------------
	let dStatus = $state('');
	let dSummary: string | null = $state(null);
	let dBusy = $state(false);
	let again = $state(false);
	let dAlerts: Alerts | undefined = $state();
	const deleteAgain = $derived(again || svc.needs_attention === 'delete_interrupted');

	async function deleteService() {
		if (dBusy) return;
		dStatus = '';
		dSummary = null;
		const ref = ctx.ref;
		const r = await formDialog?.ask({
			title: `Delete ${ref}?`,
			lines: [
				`This stops and removes the containers of ${ref}, so the app goes offline, and deletes its deploy history and logs. Anything stored inside its containers is lost. If it is the last service in project ${ctx.project}, the project goes too. This cannot be undone.`,
				'Queued and running deploys of this service are cancelled first. Your next step is a fresh authenticator code.'
			],
			label: `Type ${ref} to confirm`,
			value: '',
			confirm: deleteAgain ? 'Delete service again' : 'Delete service',
			match: ref,
			invalid: `Type ${ref} exactly as shown, then choose Delete service.`
		});
		if (!r?.ok) {
			dStatus = 'Cancelled. Nothing was deleted.';
			return;
		}
		dBusy = true;
		try {
			// The exact target goes in SD-Confirm; a fresh code is asked for if the 5-minute window has passed.
			const out = await withStepUp(() => runAction<{ cancelled?: number; flagged?: number }>('services.delete', { service: ref }, undefined, ref));
			listNotice.message = `Delete of ${ref} queued. It runs in the background and the service leaves this list when it is done${out.flagged ? ' (a running deploy was told to stop first)' : ''}. If it is still listed after a few minutes, open it and use Delete again.`;
			await goto('/services');
		} catch (err) {
			if (err instanceof Cancelled) {
				dStatus = 'Cancelled. Nothing was deleted.';
				return;
			}
			if (!(err instanceof ApiError)) throw err;
			if (err.status === 401) {
				await ctx.fail(err);
				return;
			}
			if (err.needsStepUp || err.message.includes(CONFIRM_AGAIN_FRAGMENT)) {
				again = true;
				dSummary = 'Confirming again is needed: the 5-minute confirmation window passed before the delete could start. Nothing was deleted. Choose Delete again.';
			} else dSummary = mutationMessage(err);
			await Promise.resolve();
			dAlerts?.focus();
		} finally {
			dBusy = false;
		}
	}
</script>

<ConfirmDialog bind:this={confirmDialog} />
<FormDialog bind:this={formDialog} />
<StepUpDialog bind:this={stepUp} />

<div class="settings">
	<section aria-labelledby="h-fields" class="block">
		<h2 id="h-fields">Service settings</h2>
		<p class="note">Changes are saved now and used by the next deploy.</p>
		<Alerts error={fSummary} links={fLinks} status={fStatus} bind:this={fAlerts} />
		<form novalidate onsubmit={saveFields} oninput={clearError}>
			<Field id={idOf('containerPort')} label="Port your app listens on" bind:value={containerPort} inputmode="numeric" error={fErrors.containerPort} hint="Where the app listens inside its container. A port can be changed here, not removed." />
			<fieldset>
				<legend>Health check</legend>
				<div class="choices">
					<label class="choice"><input type="radio" name="health" value="tcp" bind:group={healthKind} /> TCP: the port accepts connections</label>
					<label class="choice"><input type="radio" name="health" value="http" bind:group={healthKind} /> HTTP: a path answers with success</label>
					<label class="choice"><input type="radio" name="health" value="running" bind:group={healthKind} /> Stays running: no port check</label>
				</div>
				{#if healthKind === 'http'}
					<Field id={idOf('healthPath')} label="Health check path" bind:value={healthPath} error={fErrors.healthPath} hint="Starts with /, for example /health." />
				{/if}
				<Field id={idOf('timeoutS')} label="Health timeout (seconds)" bind:value={timeoutS} inputmode="numeric" error={fErrors.timeoutS} hint="How long the new version has to become healthy. 5 to 600." />
				<Field id={idOf('passes')} label="Health passes" bind:value={passes} inputmode="numeric" error={fErrors.passes} hint="Consecutive passing checks needed. 1 to 10." />
			</fieldset>
			<fieldset>
				<legend>Strategy</legend>
				<div class="choices">
					<div class="choice">
						<label><input type="radio" name="strategy" value="blue_green" aria-describedby="st-bg" bind:group={strategy} /> Blue-green</label>
						<p id="st-bg" class="hint">Starts the new version beside the old one and switches when it is healthy. The app stays up.</p>
					</div>
					<div class="choice">
						<label><input type="radio" name="strategy" value="stop_first" aria-describedby="st-sf" bind:group={strategy} /> Stop-first</label>
						<p id="st-sf" class="hint">Stops the old version, then starts the new one. The app is briefly down, and only one version ever runs.</p>
					</div>
				</div>
			</fieldset>
			<Field id={idOf('drainS')} label="Drain time (seconds)" bind:value={drainS} inputmode="numeric" error={fErrors.drainS} hint="Blue-green: how long the old version keeps finishing requests. 10 to 300." />
			<Field id={idOf('graceS')} label="Stop grace (seconds)" bind:value={graceS} inputmode="numeric" error={fErrors.graceS} hint="How long a stopping container may take before it is killed. 0 to 300." />
			<div class="actions"><button type="submit" class="primary" aria-disabled={fBusy ? 'true' : undefined}>{fBusy ? 'Saving…' : 'Save settings'}</button></div>
		</form>
	</section>

	<section aria-labelledby="h-source" class="block">
		<h2 id="h-source">Image repository</h2>
		<p class="note">Where the image comes from. To deploy another tag or digest of the same repository, use Deploy instead.</p>
		<Alerts error={iSummary} status={iStatus} bind:this={iAlerts} />
		<form novalidate onsubmit={saveSource} oninput={() => (iError = '')}>
			<Field id="s-image" label="Image" bind:value={image} error={iError} hint="Any repository or registry. It is used by the next deploy; nothing changes until you deploy." />
			<div class="actions"><button type="submit" aria-disabled={iBusy ? 'true' : undefined}>{iBusy ? 'Saving…' : 'Change image repository'}</button></div>
		</form>
	</section>

	<section aria-labelledby="h-port" class="block" id="port">
		<h2 id="h-port">Public port</h2>
		<p class="note">{svc.public_port ? `Exposed at ${appAddress(svc.public_port)} (plain HTTP).` : 'Not exposed: the app is private.'} A change here is applied at once, after you confirm.</p>
		{#if svc.route_error}
			<p class="banner" data-testid="port-route-error"><span aria-hidden="true">▲</span> Route problem: {svc.route_error}</p>
		{/if}
		{#if svc.public_port && !svc.routes_ready}
			<p class="note"><span aria-hidden="true">◌</span> Routes are not served yet: ServerDock is still reading the container list.</p>
		{/if}
		{#if pNotice}
			<p class="banner" data-testid="port-notice"><span aria-hidden="true">▲</span> {pNotice}</p>
		{/if}
		<Alerts error={pSummary} status={pStatus} bind:this={pAlerts} />
		<form novalidate onsubmit={savePort} oninput={() => (pError = '')}>
			<Field
				id="s-publicPort"
				label="Public port"
				bind:value={publicPort}
				inputmode="numeric"
				error={pError}
				describedby="port-sec6"
				hint={typedPort
					? `Exposes the app on the internet at http://${panelHost()}:${typedPort}, unless your host's firewall blocks the port.`
					: "A public port exposes the app on the internet at this server's address, unless your host's firewall blocks the port. 1024 to 65535, not 80 or 443."}
			/>
			<p id="port-sec6" class="hint sec6">
				<span aria-hidden="true">▲</span>
				{sharesPanelDraft(typedPort)}
				<a href={SEC6_DOCS_URL} target="_blank" rel="noopener noreferrer">Read about this (opens GitHub)</a>
			</p>
			<div class="actions">
				<button type="submit" class="primary" aria-disabled={pBusy ? 'true' : undefined}>{svc.public_port ? 'Change public port' : 'Expose on this port'}</button>
				{#if svc.public_port}<button type="button" onclick={clearPort} aria-disabled={pBusy ? 'true' : undefined}>Stop exposing</button>{/if}
				{#if canRetry}<button type="button" onclick={tryAgain} aria-disabled={pBusy ? 'true' : undefined}>Try again</button>{/if}
			</div>
		</form>
	</section>

	<section aria-labelledby="h-danger" class="block danger">
		<h2 id="h-danger">Danger zone</h2>
		<p>Delete {ctx.ref}: stops and removes its containers and deletes its deploy history and logs. This cannot be undone.</p>
		<Alerts error={dSummary} status={dStatus} bind:this={dAlerts} />
		<div class="actions"><button type="button" class="danger-button" onclick={deleteService} aria-disabled={dBusy ? 'true' : undefined}>{deleteAgain ? 'Delete again' : 'Delete service'}</button></div>
	</section>
</div>

<style>
	.settings {
		max-width: 40rem;
	}
	.block {
		margin: 0 0 2rem;
	}
	fieldset {
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		margin: 0 0 1rem;
		padding: 0.75rem 1rem;
		min-width: 0;
	}
	legend {
		font-weight: 600;
		padding: 0 0.25rem;
	}
	.choices {
		display: grid;
		gap: 0.25rem;
	}
	.choice {
		display: block;
		padding: 0.5rem 0;
		min-height: var(--target);
	}
	.choice input {
		width: 1.25rem;
		height: 1.25rem;
		margin: 0 0.5rem 0 0;
		vertical-align: middle;
	}
	.choice label {
		display: inline-block;
		min-height: var(--target);
		line-height: var(--target);
	}
	.choice .hint {
		margin: 0 0 0 1.75rem;
	}
	.sec6 {
		border-left: 4px solid var(--border);
		padding-left: 0.75rem;
	}
	.danger {
		border: 2px solid var(--danger);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
	}
	.danger-button {
		border: 2px solid var(--danger);
		font-weight: 700;
	}
</style>
