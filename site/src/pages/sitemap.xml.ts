import { SITE } from '../config';

// Pages that exist. Later steps add their routes here.
const PATHS = ['/', '/features/', '/benchmarks/', '/roadmap/', '/docs/', '/security/', '/contributing/'];

export function GET() {
	const urls = PATHS.map((p) => `<url><loc>${SITE}${p}</loc></url>`).join('');
	const body = `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${urls}</urlset>\n`;
	return new Response(body, { headers: { 'Content-Type': 'application/xml' } });
}
