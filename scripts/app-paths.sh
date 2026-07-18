#!/usr/bin/env sh

lumina_resolve_app_root() {
  override="${1:-${LUMINA_APP_ROOT:-}}"
  home_dir="${2:-${HOME:-}}"
  if [ -n "$override" ]; then
    case "$override" in
      "~") root="$home_dir" ;;
      "~/"*|"~\\"*) root="$home_dir/${override#??}" ;;
      *) root="$override" ;;
    esac
  else
    if [ -z "$home_dir" ]; then
      printf 'error: cannot resolve LuminaCode AppRoot: HOME is empty\n' >&2
      return 1
    fi
    root="$home_dir/.lumina"
  fi
  case "$root" in
    /*) ;;
    *) printf 'error: LUMINA_APP_ROOT must be absolute: %s\n' "$root" >&2; return 1 ;;
  esac
  while [ "$root" != "/" ] && [ "${root%/}" != "$root" ]; do root="${root%/}"; done
  if [ "$root" = "/" ]; then
    printf 'error: refusing unsafe LuminaCode AppRoot: %s\n' "$root" >&2
    return 1
  fi
  printf '%s\n' "$root"
}
