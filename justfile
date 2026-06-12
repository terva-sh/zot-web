# zot-web dev tasks. Run `just` to list.
set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

# Maintainer-only release-cut targets (release-cut/-verify/-publish/…).
# Optional import: the public tree ships without release.just and this
# justfile still works there.
import? 'release.just'

# Default SearXNG instance for `just configure-searxng` (override by passing a URL).
SEARXNG_URL := "http://127.0.0.1:11984"

default:
    @just --list

# Build the extension binary (loaded by run.sh / copied in by `just install`).
# Offline build against vendor/ — mirrors what run.sh does on first launch.
build:
    go build -mod=vendor -o zot-web .
    @echo "built ./zot-web"

# Refresh the committed vendor/ tree after changing dependencies.
#
# We vendor so run.sh's build-on-first-launch is a fast OFFLINE compile: zot
# blocks its whole startup until the extension sends `hello`, and a network
# module download there would stall (or, if it hangs, freeze zot). Re-evaluate
# this approach if vendor/ grows large (currently ~6 MB / a handful of deps) —
# at some point committing prebuilt per-platform binaries (goreleaser) becomes
# the better trade-off than carrying a big vendor tree in the repo.
vendor:
    go mod tidy
    go mod vendor
    @echo "vendor/ refreshed — commit it alongside go.mod/go.sum"

# Build, then (re)install into $ZOT_HOME so the latest binary is loaded.
# Preserves an existing config.json across the reinstall.
install: build
    #!/usr/bin/env bash
    set -euo pipefail
    name="$(basename "$PWD")"
    # Install dir is the last column of `zot ext list`; the path can contain
    # spaces, so take everything from the first '/'.
    resolve_dir() { local l; l="$(zot ext list | grep -E "/${name}\$" || true)"; [[ -n "$l" ]] && printf '/%s' "${l#*/}"; }

    # Stash the current config.json (if any) before remove wipes the dir.
    saved=""
    olddir="$(resolve_dir || true)"
    if [[ -n "$olddir" && -f "$olddir/config.json" ]]; then
      saved="$(mktemp)"; cp "$olddir/config.json" "$saved"
      echo "preserving existing config.json"
    fi

    zot ext remove "$name" -y || true                 # -y skips the confirm; matches the dir basename
    zot ext install "$PWD"

    dir="$(resolve_dir || true)"
    [[ -n "$dir" ]] || { echo "install: could not find installed dir in 'zot ext list'" >&2; exit 1; }
    # `zot ext install` copies git-aware and skips .gitignore'd files — which
    # includes the built ./zot-web binary (extension.json's exec target). Copy it
    # in explicitly so the installed extension can actually run.
    cp -f zot-web "$dir/zot-web"
    echo "copied binary -> $dir/zot-web"

    if [[ -n "$saved" ]]; then
      cp "$saved" "$dir/config.json"; rm -f "$saved"
      echo "restored config.json"
    fi
    zot ext list

# Point the installed extension at a SearXNG backend (default: SEARXNG_URL).
configure-searxng url=SEARXNG_URL:
    #!/usr/bin/env bash
    set -euo pipefail
    # Resolve the installed data dir from `zot ext list` — portable across OSes
    # and a custom $ZOT_HOME. The dir is the last column; the path may contain
    # spaces, so take everything from the first '/'. URL defaults to SEARXNG_URL;
    # pass one to override (a bare host:port gets an http:// prefix).
    url="{{url}}"
    [[ "$url" == *://* ]] || url="http://$url"
    name="$(basename "$PWD")"
    line="$(zot ext list | grep -E "/${name}\$" || true)"
    [[ -n "$line" ]] || { echo "extension not installed; run \`just install\` first" >&2; exit 1; }
    dir="/${line#*/}"

    host="${url#*://}"; host="${host%%/*}"; host="${host%@*}"
    if [[ "$host" == \[*\] ]]; then host="${host#[}"; host="${host%]}"; else host="${host%%:*}"; fi
    if [[ "$host" == "localhost" || "$host" == "127."* || "$host" == "::1" ]]; then
      cat > "$dir/config.json" <<JSON
    {
      "search_backend": "searxng",
      "searxng_url": "$url",
      "allow_local_hosts": ["localhost", "127.0.0.1", "::1"]
    }
    JSON
    else
      cat > "$dir/config.json" <<JSON
    {
      "search_backend": "searxng",
      "searxng_url": "$url"
    }
    JSON
    fi
    echo "configured searxng -> $url"
    echo "wrote $dir/config.json"

# Vet + gofmt check. gofmt walks the filesystem, so exclude the vendored
# third-party tree (go vet ./... already skips vendor/ in module mode).
lint:
    go vet ./...
    @test -z "$(gofmt -l $(find . -name '*.go' -not -path './vendor/*') | tee /dev/stderr)" || { echo "gofmt issues (run \`just fmt\`)"; exit 1; }

# Format sources (excluding the vendored tree).
fmt:
    gofmt -w $(find . -name '*.go' -not -path './vendor/*')

# Run tests.
test *ARGS:
    go test ./... {{ARGS}}

# Everything the Forgejo CI gate runs (.forgejo/workflows/ci.yml mirrors this).
ci: lint
    go test -race ./...
    go mod vendor
    git diff --exit-code -- go.mod go.sum vendor/

# Build and load into a one-off zot session for manual testing.
try DIR=".": build
    zot --ext "$PWD" --cwd "{{DIR}}"

# Print the version string the binary would report, built from source.
version:
    @go run -mod=vendor . --version

# Remove build output.
clean:
    rm -f zot-web
