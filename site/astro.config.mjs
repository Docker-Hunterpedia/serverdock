import { defineConfig, passthroughImageService } from 'astro/config';

// Static output, no client islands, no image processing (so `sharp` never runs).
export default defineConfig({
	site: 'https://serverdock.oday-bakkour.com',
	output: 'static',
	trailingSlash: 'always',
	build: { inlineStylesheets: 'never', format: 'directory' },
	image: { service: passthroughImageService() },
	devToolbar: { enabled: false },
	telemetry: false,
});
