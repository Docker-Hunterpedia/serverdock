<script lang="ts">
	// The QR code. `uqr` is imported here, on demand, so its code is a separate chunk
	// that only the enrolment screens ever load. Always dark modules on a light
	// background with a 4-module quiet zone, in every theme (see .qr in app.css).
	import { onMount } from 'svelte';

	let { text, label }: { text: string; label: string } = $props();
	let path = $state('');
	let size = $state(0);

	onMount(async () => {
		const { encode } = await import('uqr');
		const { data } = encode(text, { ecc: 'M', border: 0 });
		const quiet = 4;
		size = data.length + quiet * 2;
		let d = '';
		data.forEach((row, y) => {
			for (let x = 0; x < row.length; ) {
				if (!row[x]) {
					x++;
					continue;
				}
				let run = 0;
				while (x + run < row.length && row[x + run]) run++;
				d += `M${x + quiet} ${y + quiet}h${run}v1h-${run}z`;
				x += run;
			}
		});
		path = d;
	});
</script>

{#if path}
	<svg class="qr" role="img" aria-label={label} viewBox="0 0 {size} {size}" shape-rendering="crispEdges">
		<rect class="qr-bg" width={size} height={size} />
		<path class="qr-fg" d={path} />
	</svg>
{:else}
	<p class="hint">Loading the QR code…</p>
{/if}
