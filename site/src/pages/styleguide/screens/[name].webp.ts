// A 1x1 WebP for the styleguide's Screen figures (test builds only), so the real component is
// exercised by axe and the layout tests before any real screenshot is committed.
const WEBP = 'UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==';

export function getStaticPaths() {
	if (process.env.SITE_STYLEGUIDE !== '1') return [];
	return ['dark-800', 'dark-1600', 'light-800', 'light-1600', 'phone-dark-300', 'phone-dark-600', 'phone-light-300', 'phone-light-600'].map((name) => ({ params: { name } }));
}

export function GET() {
	return new Response(Uint8Array.from(atob(WEBP), (c) => c.charCodeAt(0)), { headers: { 'Content-Type': 'image/webp' } });
}
