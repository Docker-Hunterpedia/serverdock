<script lang="ts">
	// One labelled input: a visible label, a hint paragraph (never placeholder text)
	// and an error, both linked with aria-describedby.
	import type { HTMLInputAttributes } from 'svelte/elements';

	type Props = {
		id: string;
		label: string;
		value: string;
		type?: string;
		hint?: string;
		error?: string;
		autocomplete?: HTMLInputAttributes['autocomplete'];
		inputmode?: 'numeric' | 'text';
		maxlength?: number;
		readonly?: boolean;
		el?: HTMLInputElement;
	};
	let { id, label, value = $bindable(), type = 'text', hint, error, autocomplete, inputmode, maxlength, readonly, el = $bindable() }: Props = $props();
	const describedby = $derived([hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter(Boolean).join(' ') || undefined);
</script>

<div class="field">
	<label for={id}>{label}</label>
	{#if hint}<p id="{id}-hint" class="hint">{hint}</p>{/if}
	<input
		bind:this={el}
		{id}
		{type}
		{value}
		oninput={(e) => (value = e.currentTarget.value)}
		{autocomplete}
		{inputmode}
		{maxlength}
		{readonly}
		spellcheck="false"
		autocapitalize="none"
		aria-describedby={describedby}
		aria-invalid={error ? 'true' : undefined}
	/>
	{#if error}<p id="{id}-error" class="error"><span aria-hidden="true">▲</span> {error}</p>{/if}
</div>
