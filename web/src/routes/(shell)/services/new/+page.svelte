<script lang="ts">
	import { goto } from '$app/navigation';
	import { onDestroy, onMount, tick } from 'svelte';
	import { ApiError, NETWORK_MESSAGE, runAction, signedOutReason } from '../../../../lib/api';
	import Alerts from '../../../../lib/Alerts.svelte';
	import Field from '../../../../lib/Field.svelte';
	import { created } from '../../../../lib/flow.svelte';
	import PageHeading from '../../../../lib/PageHeading.svelte';
	import {
		SEC6_DOCS_URL,
		actionMessage,
		appAddress,
		deployPath,
		listProjects,
		panelHost,
		servicePath,
		sharesPanelDraft,
		suggestedPort,
		type CreateInput,
		type CreateOutput
	} from '../../../../lib/services';

	let project = $state('default');
	let name = $state('');
	let image = $state('');
	let containerPort = $state('');
	let publicPort = $state('');
	let healthKind: 'tcp' | 'http' | 'running' | undefined = $state(); // undefined: follows the port field until chosen
	let healthPath = $state('/');
	let strategy = $state('blue_green');
	let timeoutS = $state('');
	let passes = $state('');
	let drainS = $state('');
	let graceS = $state('');
	let advancedOpen = $state(false);

	let projects: string[] = $state([]);
	let errors: Record<string, string> = $state({});
	let summary: string | null = $state(null);
	let links: { id: string; text: string }[] = $state([]);
	let status = $state('');
	let busy = $state(false);
	let alerts: Alerts | undefined = $state();
	let ctrl: AbortController | undefined;

	onMount(async () => {
		ctrl = new AbortController();
		try {
			projects = await listProjects(ctrl.signal);
		} catch {
			/* only a convenience: the field still takes any name */
		}
	});
	onDestroy(() => ctrl?.abort());

	const suggestion = $derived(suggestedPort(image));
	const effectiveKind = $derived(healthKind ?? (containerPort.trim() ? 'tcp' : 'running'));
	const publicNumber = $derived(/^\d+$/.test(publicPort.trim()) ? Number(publicPort.trim()) : null);
	const host = panelHost();

	const FIELD_LABELS: Record<string, string> = {
		project: 'Project',
		name: 'Name',
		image: 'Image',
		containerPort: 'Port your app listens on',
		publicPort: 'Public port',
		healthPath: 'Health check path',
		timeoutS: 'Health timeout',
		passes: 'Health passes',
		drainS: 'Drain time',
		graceS: 'Stop grace'
	};
	const FIELD_IDS: Record<string, string> = {
		project: 'f-project',
		name: 'f-name',
		image: 'f-image',
		containerPort: 'f-container-port',
		publicPort: 'f-public-port',
		healthPath: 'f-health-path',
		timeoutS: 'f-timeout',
		passes: 'f-passes',
		drainS: 'f-drain',
		graceS: 'f-grace'
	};

	function int(v: string, lo: number, hi: number): number | 'bad' | undefined {
		const t = v.trim();
		if (t === '') return undefined;
		if (!/^\d{1,5}$/.test(t)) return 'bad';
		const n = Number(t);
		return n < lo || n > hi ? 'bad' : n;
	}

	/** Checks what the browser can check; the server stays the authority and its answers are mapped back to fields. */
	function validate(): { input?: CreateInput; errs: Record<string, string> } {
		const errs: Record<string, string> = {};
		if (!/^[a-z][a-z0-9-]{0,29}$/.test(project.trim()) || project.trim().endsWith('-')) errs.project = 'Use 1 to 30 lowercase letters, digits or -, starting with a letter.';
		if (!/^[a-z][a-z0-9-]{0,29}$/.test(name.trim()) || name.trim().endsWith('-')) errs.name = 'Use 1 to 30 lowercase letters, digits or -, starting with a letter.';
		if (!image.trim()) errs.image = 'Enter the image to run, for example nginx:1.27.';
		const cp = int(containerPort, 1, 65535);
		if (cp === 'bad') errs.containerPort = 'Enter a port from 1 to 65535, or leave it empty.';
		const pp = int(publicPort, 1024, 65535);
		if (pp === 'bad' || pp === 80 || pp === 443) errs.publicPort = 'Enter a port from 1024 to 65535 (not 80 or 443), or leave it empty.';
		else if (pp !== undefined && cp === undefined) errs.publicPort = 'A public port needs the port your app listens on. Fill that in first.';
		const t = int(timeoutS, 5, 600);
		if (t === 'bad') errs.timeoutS = 'Enter a number of seconds from 5 to 600, or leave it empty.';
		const pa = int(passes, 1, 10);
		if (pa === 'bad') errs.passes = 'Enter a number from 1 to 10, or leave it empty.';
		const dr = int(drainS, 10, 300);
		if (dr === 'bad') errs.drainS = 'Enter a number of seconds from 10 to 300, or leave it empty.';
		const gr = int(graceS, 0, 300);
		if (gr === 'bad') errs.graceS = 'Enter a number of seconds from 0 to 300, or leave it empty.';
		const kind = effectiveKind;
		if (kind !== 'running' && cp === undefined) errs.containerPort = 'This health check needs the port your app listens on. Fill it in, or choose "Stays running".';
		if (kind === 'http' && !/^\/\S{0,255}$/.test(healthPath.trim())) errs.healthPath = 'Enter a path that starts with /, for example /health.';
		if (Object.keys(errs).length) return { errs };
		const input: CreateInput = {
			project: project.trim(),
			name: name.trim(),
			image_ref: image.trim(),
			strategy,
			deploy: true,
			health: { kind, ...(kind === 'http' ? { path: healthPath.trim() } : {}), ...(typeof t === 'number' ? { timeout_s: t } : {}), ...(typeof pa === 'number' ? { passes: pa } : {}) }
		};
		if (typeof cp === 'number') input.container_port = cp;
		if (typeof pp === 'number') input.public_port = pp;
		if (typeof dr === 'number') input.drain_s = dr;
		if (typeof gr === 'number') input.stop_grace_s = gr;
		return { input, errs };
	}

	/** Which field a server message is about (its text starts with the field it checked). */
	function fieldOf(msg: string): string | undefined {
		const m = msg.toLowerCase();
		if (m.startsWith('project:')) return 'project';
		if (m.startsWith('name:') || m.startsWith('service ') && m.includes('already exists')) return 'name';
		if (m.startsWith('image_ref:')) return 'image';
		if (m.startsWith('health.path')) return 'healthPath';
		if (m.includes('public port') || m.startsWith('public_port') || m.startsWith('port ') && m.includes('already used')) return 'publicPort';
		if (m.includes('container port') || m.startsWith('container_port')) return 'containerPort';
		return undefined;
	}

	async function fail(msg: string, errs: Record<string, string>) {
		errors = errs;
		summary = msg;
		links = Object.keys(errs).map((k) => ({ id: FIELD_IDS[k], text: `${FIELD_LABELS[k]}: ${errs[k]}` }));
		// A field inside the closed Advanced disclosure must be reachable from the summary.
		if (['timeoutS', 'passes', 'drainS', 'graceS'].some((k) => k in errs)) advancedOpen = true;
		await tick();
		alerts?.focus();
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (busy) return;
		const { input, errs } = validate();
		if (!input) {
			await fail(Object.keys(errs).length === 1 ? 'One field needs fixing before the service can be created.' : `${Object.keys(errs).length} fields need fixing before the service can be created.`, errs);
			return;
		}
		busy = true;
		errors = {};
		summary = null;
		links = [];
		status = 'Creating the service and queueing the first deploy…';
		try {
			const out = await runAction<CreateOutput>('services.create', input, undefined);
			created.service = out.service.service;
			created.warnings = out.service.warnings;
			created.port = out.service.public_port;
			status = '';
			const d = out.deploy;
			await goto(d ? deployPath(out.service.project, out.service.name, d.deploy) : servicePath(out.service));
		} catch (err) {
			status = '';
			if (!(err instanceof ApiError)) throw err;
			if (err.status === 401) {
				await goto(`/signed-out?reason=${signedOutReason(err.code)}`);
				return;
			}
			const msg = err.code === 'network' ? NETWORK_MESSAGE : actionMessage(err.message);
			const f = err.status === 400 || err.status === 409 ? fieldOf(err.message) : undefined;
			await fail(msg, f ? { [f]: msg } : {});
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Deploy a Docker image · ServerDock</title>
</svelte:head>

<PageHeading>Deploy a Docker image</PageHeading>
<p class="note">ServerDock pulls the image, starts it, checks that it is healthy and only then sends traffic to it. Git, Compose and databases come in later versions.</p>

<Alerts error={summary} {links} {status} bind:this={alerts} />

<form novalidate onsubmit={submit} class="form">
	<Field id="f-project" label="Project" bind:value={project} list="project-list" error={errors.project} hint="Projects group services. A new name creates a project." />
	<datalist id="project-list">{#each projects as p (p)}<option value={p}></option>{/each}</datalist>
	<Field id="f-name" label="Name" bind:value={name} error={errors.name} hint="Lowercase letters, digits and -, for example web." />
	<Field id="f-image" label="Image" bind:value={image} error={errors.image} hint="A tag or digest from a registry, for example nginx:1.27. Pin a version rather than latest." />
	<Field
		id="f-container-port"
		label="Port your app listens on (optional)"
		bind:value={containerPort}
		inputmode="numeric"
		error={errors.containerPort}
		describedby={suggestion ? 'port-suggestion' : undefined}
		hint="Leave empty for a worker that serves no web traffic."
	/>
	{#if suggestion}
		<p id="port-suggestion" class="hint suggestion">
			Suggested for {suggestion.image}: port {suggestion.port}.
			{#if containerPort.trim() !== String(suggestion.port)}
				<button type="button" onclick={() => (containerPort = String(suggestion.port))}>Use {suggestion.port}</button>
			{/if}
		</p>
	{/if}
	<Field
		id="f-public-port"
		label="Public port (optional)"
		bind:value={publicPort}
		inputmode="numeric"
		error={errors.publicPort}
		describedby="public-sec6"
		hint="Exposes the app on the internet at http://{host}:{publicNumber ?? '<port>'}, unless your host's firewall blocks the port. Leave empty to keep it private."
	/>
	<p id="public-sec6" class="hint sec6">
		<span aria-hidden="true">▲</span>
		{sharesPanelDraft(publicNumber)}
		<a href={SEC6_DOCS_URL} rel="noopener">Read about this (opens GitHub)</a>
	</p>
	{#if publicNumber}<p class="hint">After the deploy the app will be at <code>{appAddress(publicNumber)}</code>.</p>{/if}

	<fieldset aria-describedby="health-hint">
		<legend>Health check</legend>
		<p class="hint" id="health-hint">How ServerDock decides the new version is ready before it takes traffic.</p>
		<div class="choices">
			<label class="choice"><input type="radio" name="health" value="tcp" checked={effectiveKind === 'tcp'} onchange={() => (healthKind = 'tcp')} /> TCP: the port accepts connections</label>
			<label class="choice"><input type="radio" name="health" value="http" checked={effectiveKind === 'http'} onchange={() => (healthKind = 'http')} /> HTTP: a path answers with success</label>
			<label class="choice"><input type="radio" name="health" value="running" checked={effectiveKind === 'running'} onchange={() => (healthKind = 'running')} /> Stays running: no port check</label>
		</div>
		{#if effectiveKind === 'http'}
			<Field id="f-health-path" label="Health check path" bind:value={healthPath} error={errors.healthPath} hint="Starts with /, for example /health." />
		{/if}
	</fieldset>

	<fieldset>
		<legend>Strategy</legend>
		<div class="choices">
			<label class="choice">
				<input type="radio" name="strategy" value="blue_green" bind:group={strategy} /> Blue-green
				<span class="hint">Starts the new version beside the old one and switches when it is healthy. The app stays up.</span>
			</label>
			<label class="choice">
				<input type="radio" name="strategy" value="stop_first" bind:group={strategy} /> Stop-first
				<span class="hint">Stops the old version, then starts the new one. The app is briefly down, and only one version ever runs. Use it for apps that cannot run twice.</span>
			</label>
		</div>
	</fieldset>

	<details bind:open={advancedOpen}>
		<summary>Advanced</summary>
		<Field id="f-timeout" label="Health timeout (seconds)" bind:value={timeoutS} inputmode="numeric" error={errors.timeoutS} hint="How long the new version has to become healthy. 5 to 600, default 60." />
		<Field id="f-passes" label="Health passes" bind:value={passes} inputmode="numeric" error={errors.passes} hint="Consecutive passing checks needed. 1 to 10, default 3." />
		<Field id="f-drain" label="Drain time (seconds)" bind:value={drainS} inputmode="numeric" error={errors.drainS} hint="Blue-green: how long the old version keeps finishing requests. 10 to 300, default 10." />
		<Field id="f-grace" label="Stop grace (seconds)" bind:value={graceS} inputmode="numeric" error={errors.graceS} hint="How long a stopping container may take before it is killed. 0 to 300, default 10." />
	</details>

	<div class="actions">
		<button type="submit" class="primary" aria-disabled={busy ? 'true' : undefined}>Create and deploy</button>
		<a class="button" href="/services">Cancel</a>
	</div>
</form>

<style>
	.form {
		max-width: 40rem;
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
	.choice .hint {
		display: block;
		margin: 0.25rem 0 0 1.75rem;
	}
	.sec6 {
		border-left: 4px solid var(--border);
		padding-left: 0.75rem;
	}
	.suggestion {
		margin-top: -0.5rem;
	}
	details {
		margin: 0 0 1rem;
	}
	summary {
		min-height: var(--target);
		display: flex;
		align-items: center;
		cursor: pointer;
		font-weight: 600;
	}
</style>
