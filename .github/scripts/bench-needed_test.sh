#!/bin/sh
# Tests for bench-needed.sh: scratch git repo plus a stubbed gh and a fixed
# lookup table (no network). Run: .github/scripts/bench-needed_test.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
script=$here/bench-needed.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

# gh stub. Reads $GATE_DIR/<sha> files: "<workflow> <conclusion>" for the
# run on that commit. It records the arguments it was called with.
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'STUB'
#!/bin/sh
echo "$*" >>"$GH_LOG"
case "$1 $2" in
"run list")
	wf= commit=
	while [ $# -gt 0 ]; do
		case "$1" in
		--workflow) wf=$2; shift ;;
		--commit) commit=$2; shift ;;
		esac
		shift
	done
	f=$GATE_DIR/$commit
	[ -f "$f" ] || { echo '[]'; exit 0; }
	read -r w c <"$f"
	# Only the requested workflow's runs are visible.
	[ "$w" = "$wf" ] || { echo '[]'; exit 0; }
	echo "[{\"databaseId\": $(printf '%s' "$commit" | cksum | cut -d' ' -f1)}]"
	;;
"run view")
	id=$3
	for f in "$GATE_DIR"/*; do
		[ -f "$f" ] || continue
		[ "$(printf '%s' "$(basename "$f")" | cksum | cut -d' ' -f1)" = "$id" ] || continue
		read -r w c <"$f"
		echo "{\"jobs\":[{\"name\":\"lint-test\",\"conclusion\":\"success\"},{\"name\":\"bench-gate\",\"conclusion\":\"$c\"}]}"
		exit 0
	done
	exit 1
	;;
esac
STUB
chmod +x "$tmp/bin/gh"

repo=$tmp/repo
git init -q "$repo"
cd "$repo"
git config user.email t@example.invalid
git config user.name t
git config commit.gpgsign false
mkdir -p docs cmd .github/scripts
echo base >docs/a.md
echo 'package main' >cmd/main.go
git add -A && git commit -qm base
BASE=$(git rev-parse HEAD)

commit() { # file content
	mkdir -p "$(dirname "$1")"
	printf '%s\n' "$2" >"$1"
	git add -A && git commit -qm "c $1 $2"
	git rev-parse HEAD
}

GH_LOG=$tmp/gh.log
GATE_DIR=$tmp/gates
export GH_LOG GATE_DIR
mkdir "$GATE_DIR"
PATH=$tmp/bin:$PATH
export PATH

# run <name> <expected bench> <expected reason substring> env...
run() {
	name=$1 want=$2 reason=$3
	shift 3
	: >"$tmp/out"
	: >"$GH_LOG"
	env -i PATH="$PATH" HOME="$HOME" GH_LOG="$GH_LOG" GATE_DIR="$GATE_DIR" \
		GITHUB_OUTPUT="$tmp/out" "$@" sh "$script"
	got=$(sed -n 's/^bench=//p' "$tmp/out")
	why=$(sed -n 's/^reason=//p' "$tmp/out")
	case "$why" in *"$reason"*) rok=1 ;; *) rok=0 ;; esac
	if [ "$got" = "$want" ] && [ "$rok" = 1 ]; then
		echo "ok   $name"
	else
		echo "FAIL $name: bench=$got reason=$why (want $want / $reason)"
		fails=$((fails + 1))
	fi
}

# sync <name> <want> <reason> <before> <after> [env...]: a synchronize event.
sync() {
	n=$1 w=$2 r=$3 b=$4 a=$5
	shift 5
	run "$n" "$w" "$r" EVENT=pull_request ACTION=synchronize BEFORE="$b" AFTER="$a" BASE="$BASE" SHA="$a" "$@"
}

# A: Go change. B, C: docs only.
A=$(commit cmd/main.go 'package main // a')
B=$(commit docs/b.md b)
C=$(commit docs/c.md c)

# opened (not synchronize): full diff
run "go change vs base -> bench" true "bench-relevant" EVENT=pull_request ACTION=opened BASE="$BASE" SHA="$A"
run "docs-only vs base -> skip" false "docs-only against" EVENT=pull_request ACTION=opened BASE="$A" SHA="$B"

# A's gate passed; B and C reuse
echo "ci.yml success" >"$GATE_DIR/$A"
sync "A->B docs, A gate ok -> reuse" false "reused bench-gate result from $A" "$A" "$B"
echo "ci.yml success" >"$GATE_DIR/$B"
sync "B->C docs, B gate ok -> reuse" false "reused bench-gate result from $B" "$B" "$C"
if grep -q -- '--workflow ci.yml' "$GH_LOG"; then
	echo "ok   lookup is pinned to ci.yml"
else
	echo "FAIL lookup not pinned to ci.yml"
	fails=$((fails + 1))
fi

# B's gate failed: C does a full diff against the base (Go change in A -> bench)
echo "ci.yml failure" >"$GATE_DIR/$B"
sync "B gate failed -> full diff" true "bench-relevant" "$B" "$C"

# Cancelled / skipped gate is not success
echo "ci.yml cancelled" >"$GATE_DIR/$B"
sync "B gate cancelled -> full diff" true "bench-relevant" "$B" "$C"
echo "ci.yml skipped" >"$GATE_DIR/$B"
sync "B gate skipped -> full diff" true "bench-relevant" "$B" "$C"

# A gate from another workflow is ignored
echo "other.yml success" >"$GATE_DIR/$B"
sync "gate from another workflow ignored" true "bench-relevant" "$B" "$C"
echo "ci.yml success" >"$GATE_DIR/$B"

# no gate run at all for before
rm "$GATE_DIR/$B"
sync "no run on before -> full diff" true "bench-relevant" "$B" "$C"
echo "ci.yml success" >"$GATE_DIR/$B"

# gh unavailable/erroring -> full diff
mkdir "$tmp/badbin"
printf '#!/bin/sh\nexit 1\n' >"$tmp/badbin/gh"
chmod +x "$tmp/badbin/gh"
sync "gh failing -> full diff" true "bench-relevant" "$B" "$C" PATH="$tmp/badbin:$PATH"

# force-push: before is not an ancestor of after
git checkout -q -b alt "$BASE"
F=$(commit docs/f.md f)
git checkout -q -
echo "ci.yml success" >"$GATE_DIR/$F"
run "force-push (not ancestor) -> full diff" false "docs-only against" EVENT=pull_request ACTION=synchronize BEFORE="$C" AFTER="$F" BASE="$BASE" SHA="$F"
echo "ci.yml success" >"$GATE_DIR/$C"
run "force-push after Go change -> full diff" true "bench-relevant" EVENT=pull_request ACTION=synchronize BEFORE="$F" AFTER="$C" BASE="$BASE" SHA="$C"

# Odd names must never read as docs-only (git quotes them in --name-only).
nl='
'
tab=$(printf '\t')
for name in 'cmd/ü.go' 'bench/a"b' "bench/tab${tab}x" "cmd/nl${nl}x" 'cmd/ü' go.work vendor/x tool.s docs/x.go web/src/routes/+page.svelte web/package-lock.json web/package.json; do
	P=$(commit "$name" odd)
	echo "ci.yml success" >"$GATE_DIR/$P"
	sync "odd path $(printf '%s' "$name" | tr '\t\n' '??') (increment)" true "bench-relevant" "$C" "$P"
	run "odd path $(printf '%s' "$name" | tr '\t\n' '??') (vs base)" true "bench-relevant" EVENT=pull_request ACTION=opened BASE="$BASE" SHA="$P"
	git reset -q --hard "$C"
done

# The public website (site/, plan 0014) is excluded from the bench. Each case commits on
# top of $C and compares against $C, so the Go change in A never decides the result.
put() { mkdir -p "$(dirname "$1")" && printf '%s\n' "$2" >"$1"; }
site_commit() { git add -A && git commit -qm "site case" && git rev-parse HEAD; }
site_case() { # name want reason
	X=$(site_commit)
	run "$1" "$2" "$3" EVENT=pull_request ACTION=opened BASE="$C" SHA="$X"
	git reset -q --hard "$C"
}
put site/src/pages/index.astro 'x'
site_case "site only -> skip" false "docs-only against"
put site/x.go 'package x'
site_case "site/x.go -> bench" true "bench-relevant"
put site/a.txt a; put web/b.txt b
site_case "site + web -> bench" true "bench-relevant"
put site/a.txt a; put .github/workflows/site.yml 'x: 1'
site_case "site + .github/workflows/site.yml -> bench" true "bench-relevant"
put site/a.txt a; put .github/workflows/ci.yml 'x: 1'
site_case "site + .github/workflows/ci.yml -> bench" true "bench-relevant"
put sitex/a 'x'
site_case "sitex/a -> bench" true "bench-relevant"
put web/site/a 'x'
site_case "web/site/a -> bench" true "bench-relevant"
# a symlink under site/ is a path under site/ (git does not follow it)
mkdir -p site && ln -s ../cmd/main.go site/link
site_case "symlink under site/ -> site only" false "docs-only against"
# a rename out of a bench-relevant path into site/ still shows the deleted path
I=$(commit internal/x 'x')
mkdir -p site && git mv internal/x site/x
X=$(site_commit)
run "rename internal/x -> site/x -> bench" true "bench-relevant" EVENT=pull_request ACTION=opened BASE="$I" SHA="$X"
git reset -q --hard "$C"
# a site-only increment after a passing gate reuses it
put site/a.txt a
X=$(site_commit)
echo "ci.yml success" >"$GATE_DIR/$C"
sync "site-only increment, gate ok -> reuse" false "reused bench-gate result from $C" "$C" "$X"
git reset -q --hard "$C"

# a missing before
MISSING=1111111111111111111111111111111111111111
run "missing before -> full diff" true "bench-relevant" EVENT=pull_request ACTION=synchronize BEFORE="$MISSING" AFTER="$C" BASE="$BASE" SHA="$C"
run "zero before -> full diff" true "bench-relevant" EVENT=pull_request ACTION=synchronize BEFORE=0000000000000000000000000000000000000000 AFTER="$C" BASE="$BASE" SHA="$C"
run "unknown base -> bench" true "unknown base" EVENT=pull_request ACTION=opened BASE="$MISSING" SHA="$C"
run "push, empty base -> bench" true "unknown base" EVENT=push SHA="$C"

# .github/scripts and workflow changes are bench-relevant, even on a reusable chain
S=$(commit .github/scripts/x.sh 'echo x')
sync ".github/scripts change -> bench" true "bench-relevant" "$C" "$S"
W=$(commit .github/workflows/x.yml 'x: 1')
echo "ci.yml success" >"$GATE_DIR/$S"
sync ".github/workflows change -> bench" true "bench-relevant" "$S" "$W"
# docs under .github (not scripts/workflows) are docs-only
D=$(commit .github/CODEOWNERS '* @x')
echo "ci.yml success" >"$GATE_DIR/$W"
sync ".github/CODEOWNERS only -> reuse" false "reused" "$W" "$D"

# values are data, not code: a hostile ACTION must not execute
run "hostile env is inert" true "bench-relevant" EVENT=pull_request ACTION="\$(touch $tmp/pwned)" BASE="$BASE" SHA="$C"
[ ! -e "$tmp/pwned" ] || { echo "FAIL command injection"; fails=$((fails + 1)); }

if [ "$fails" -ne 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
