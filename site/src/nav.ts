// The main navigation, rendered twice (wide and narrow) by Header.astro.
export const MAIN_NAV = [
	{ href: '/features/', label: 'Features' },
	{ href: '/benchmarks/', label: 'Benchmarks' },
	{ href: '/roadmap/', label: 'Roadmap' },
	{ href: '/docs/', label: 'Docs' },
] as const;
