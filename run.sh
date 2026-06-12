#!/usr/bin/env bash
# zot-web launcher.
#
# zot installs an extension by cloning the repo and running the `exec` from
# extension.json verbatim — it never compiles Go sources. This wrapper bridges
# that gap: on first launch (and after any source change) it builds the binary,
# then execs it so zot speaks the extension protocol to the real process. That
# lets `zot ext install <git-url>` work on any platform with a Go toolchain,
# without committing a platform-specific binary to the repo.
#
# IMPORTANT: stdout is the protocol wire. Every byte of build chatter must go to
# stderr (zot captures it to $ZOT_HOME/logs/ext-web.log); a stray stdout write
# would corrupt the JSON stream.
set -euo pipefail
cd "$(dirname "$0")"

bin="./zot-web"

needs_build() {
	[ -x "$bin" ] || return 0
	# Rebuild if any Go source or the module files are newer than the binary,
	# so a `git pull` that changes sources doesn't run a stale build.
	if [ -n "$(find . -name '*.go' -newer "$bin" -print -quit 2>/dev/null)" ]; then
		return 0
	fi
	if [ go.mod -nt "$bin" ] || [ go.sum -nt "$bin" ]; then
		return 0
	fi
	return 1
}

if needs_build; then
	if ! command -v go >/dev/null 2>&1; then
		echo "[zot-web] Go toolchain not found on PATH; cannot build the extension." >&2
		echo "[zot-web] Install Go 1.25+ (https://go.dev/dl/) and relaunch zot." >&2
		exit 1
	fi
	echo "[zot-web] building $bin (first launch or sources changed)…" >&2
	# -mod=vendor builds against the committed vendor/ tree: no network, no
	# module download — so the first launch is a quick offline compile and can't
	# hang on a stalled module fetch. (zot blocks startup until we send `hello`.)
	go build -mod=vendor -o "$bin" . >&2
	echo "[zot-web] build complete." >&2
fi

exec "$bin" "$@"
