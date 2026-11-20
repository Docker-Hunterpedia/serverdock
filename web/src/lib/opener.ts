// Which control opened a dialog, so focus can go back to it. Safari does not focus
// a button when it is clicked, so document.activeElement is not enough: the last
// clicked button, link or role=button is tracked as well.
let last: HTMLElement | null = null;

export function trackOpener(): () => void {
	const onClick = (e: Event) => {
		const el = (e.target as HTMLElement | null)?.closest?.('button, a, [role="button"]') as HTMLElement | null;
		if (el && !el.closest('dialog')) last = el;
	};
	document.addEventListener('click', onClick, true);
	return () => document.removeEventListener('click', onClick, true);
}

export function currentOpener(): HTMLElement | null {
	// A control that has focus is the opener. Otherwise (Safari does not focus a clicked
	// button, and a click may focus an ancestor with tabindex="-1" instead) it is the
	// last control that was clicked.
	const a = document.activeElement as HTMLElement | null;
	return a && a.matches('button, a[href], input, select, textarea, [role="button"]') && !a.closest('dialog') ? a : last;
}
