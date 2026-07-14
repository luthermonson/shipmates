#!/usr/bin/env bash
set -euo pipefail

# Build and install the current Shipmates Codex-adaptation checkout for the
# current WSL/Linux user. This intentionally does not use sudo or edit shell
# startup files: the install stays reversible and user-scoped.

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir=${SHIPMATES_BIN_DIR:-"$HOME/.local/bin"}
configure_shell=true
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

for arg in "$@"; do
  case "$arg" in
    --no-shell-config) configure_shell=false ;;
    -h|--help)
      printf '%s\n' 'Usage: install-codex-adaptation.sh [--no-shell-config]'
      exit 0
      ;;
    *)
      printf 'Unknown option: %s\n' "$arg" >&2
      exit 2
      ;;
  esac
done

if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'Go 1.26.5 or newer is required. Install Go, then run this script again.' >&2
  exit 1
fi

go_version=$(go env GOVERSION)
minimum=go1.26.5
if [[ "$go_version" != devel* ]] && [[ $(printf '%s\n%s\n' "$minimum" "$go_version" | sort -V | head -n1) != "$minimum" ]]; then
  printf 'Shipmates requires Go 1.26.5 or newer; found %s.\n' "$go_version" >&2
  exit 1
fi

revision=$(git -C "$root" rev-parse --short HEAD 2>/dev/null || printf 'local')
version="codex-adaptation-${revision}"

printf 'Building Shipmates %s...\n' "$version"
(
  cd "$root"
  go build -ldflags "-X main.version=$version" -o "$tmp_dir/shipmates" .
)

mkdir -p "$bin_dir"
install -m 0755 "$tmp_dir/shipmates" "$bin_dir/shipmates"

printf 'Installed: %s\n' "$bin_dir/shipmates"
"$bin_dir/shipmates" --version

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *)
    if "$configure_shell"; then
      shell_rc="$HOME/.bashrc"
      marker='# shipmates installer: local binary path'
      touch "$shell_rc"
      if ! grep -Fqx "$marker" "$shell_rc"; then
        {
          printf '\n%s\n' "$marker"
          printf 'export PATH=%q:"$PATH"\n' "$bin_dir"
        } >> "$shell_rc"
        printf 'Added %s to PATH in %s. Open a new WSL shell to use shipmates by name.\n' "$bin_dir" "$shell_rc"
      fi
    else
      printf '\n%s\n' "Add this to your shell profile to make shipmates available in new terminals:"
      printf '  export PATH="%s:$PATH"\n' "$bin_dir"
    fi
    ;;
esac

if command -v codex >/dev/null 2>&1; then
  if codex login status >/dev/null 2>&1; then
    printf 'Codex CLI: ready\n'
  else
    printf 'Codex CLI: installed but not logged in; run: codex login\n'
  fi
else
  printf 'Codex CLI: not found; install it before using Shipmates personas.\n'
fi
