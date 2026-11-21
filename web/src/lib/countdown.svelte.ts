// A back-off countdown. It updates visually every second but is announced only at
// the start, at each whole minute, at 10 seconds and at the end (A2), through the
// `announce` callback (the page's status region).
export function humanDuration(seconds: number): string {
	if (seconds >= 60) {
		const m = Math.ceil(seconds / 60);
		return `${m} minute${m === 1 ? '' : 's'}`;
	}
	return `${seconds} second${seconds === 1 ? '' : 's'}`;
}

export class Countdown {
	remaining = $state(0);
	#timer: ReturnType<typeof setInterval> | undefined;
	#announce: (message: string) => void;

	constructor(announce: (message: string) => void) {
		this.#announce = announce;
	}

	get active() {
		return this.remaining > 0;
	}

	/** mm:ss for the visible text. */
	get text() {
		const m = Math.floor(this.remaining / 60);
		const s = this.remaining % 60;
		return `${m}:${String(s).padStart(2, '0')}`;
	}

	start(seconds: number) {
		this.stop();
		this.remaining = Math.max(1, Math.ceil(seconds));
		this.#announce(`Too many attempts. Try again in ${humanDuration(this.remaining)}.`);
		this.#timer = setInterval(() => {
			this.remaining -= 1;
			const r = this.remaining;
			if (r <= 0) {
				this.stop();
				this.#announce('You can try again now.');
			} else if (r === 10) {
				this.#announce('Try again in 10 seconds.');
			} else if (r % 60 === 0) {
				this.#announce(`Try again in ${humanDuration(r)}.`);
			}
		}, 1000);
	}

	stop() {
		if (this.#timer !== undefined) clearInterval(this.#timer);
		this.#timer = undefined;
		this.remaining = Math.max(0, this.remaining);
		if (this.remaining < 0) this.remaining = 0;
	}
}
