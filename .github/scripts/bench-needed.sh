#!/bin/sh
# Decide whether the CI bench must run (plan 0002, M1 step 1).
#
# Inputs (environment only, never interpolated into shell by the workflow):
#   EVENT   github.event_name
#   ACTION  github.event.action (pull_request only)
#   BEFORE  github.event.before (pull_request synchronize only)
#   AFTER   github.event.after
#   BASE    pull request base sha, or push "before"
#   SHA     github.sha (the commit that is checked out)
#   GH_TOKEN, and GH_REPO if the checkout has no usable origin (used by gh)
# Output: "bench=true|false" and "reason=..." appended to $GITHUB_OUTPUT.
#
# Reuse rule: bench=false only when the increment before..after of a normal
# (non-force) PR push is docs-only AND the latest bench-gate job of the ci.yml
# workflow on BEFORE concluded success. Any doubt means the full diff against
# the base. Check runs from other apps or workflows are never consulted.
set -u

ZERO=0000000000000000000000000000000000000000
OUT=${GITHUB_OUTPUT:-/dev/stdout}

emit() { # bench reason
	printf 'bench=%s\nreason=%s\n' "$1" "$2" >>"$OUT"
	exit 0
}

has_commit() {
	[ -n "$1" ] && [ "$1" != "$ZERO" ] && git cat-file -e "$1^{commit}" 2>/dev/null
}

# docs_only succeeds only when EVERY path changed between $1 and $2 is under
# docs/, ends in .md, or is .github/CODEOWNERS, and no .go file changed anywhere
# (docs/ may hold Go code that `go vet ./...` compiles). It uses git pathspecs
# and exit codes, never parsed path text, so quoting, newlines and non-ASCII
# names cannot hide a file. Any other result, including a git error, is "not
# docs-only", i.e. bench.
docs_only() {
	git diff --quiet "$1" "$2" -- ':(glob)**/*.go' || return 1
	git diff --quiet "$1" "$2" -- ':(exclude)docs' ':(exclude,glob)**/*.md' ':(exclude).github/CODEOWNERS' || return 1
}

# full_diff decides from the whole PR (or push) diff against the base.
full_diff() {
	if ! has_commit "${BASE:-}"; then
		emit true "unknown base: running the bench to be safe"
	fi
	if docs_only "$BASE" "${SHA:-HEAD}"; then
		emit false "docs-only against the base"
	fi
	emit true "bench-relevant files changed against the base"
}

# reusable reports whether the latest ci.yml bench-gate on $1 succeeded.
reusable() {
	runs=$(gh run list --workflow ci.yml --commit "$1" --event pull_request --limit 1 --json databaseId 2>/dev/null) || return 1
	id=$(printf '%s' "$runs" | jq -r '.[0].databaseId // empty' 2>/dev/null) || return 1
	[ -n "$id" ] || return 1
	jobs=$(gh run view "$id" --json jobs 2>/dev/null) || return 1
	concl=$(printf '%s' "$jobs" | jq -r '[.jobs[] | select(.name == "bench-gate")] | last | .conclusion // empty' 2>/dev/null) || return 1
	[ "$concl" = success ]
}

if [ "${EVENT:-}" = pull_request ] && [ "${ACTION:-}" = synchronize ] &&
	has_commit "${BEFORE:-}" && has_commit "${AFTER:-}" &&
	git merge-base --is-ancestor "$BEFORE" "$AFTER" 2>/dev/null; then
	if docs_only "$BEFORE" "$AFTER" && reusable "$BEFORE"; then
		emit false "reused bench-gate result from $BEFORE"
	fi
fi
full_diff
