#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
MAKE_BIN="${MAKE:-make}"
LOG_ROOT="${LUMINA_INSTALL_LOG_DIR:-$REPO_ROOT/tmp/install-logs}"
RUN_ID="$(date -u '+%Y%m%dT%H%M%SZ')-$$"
LOG_FILE="$LOG_ROOT/install-$RUN_ID.log"
CURRENT_STAGE="startup"
CURRENT_STAGE_LOG=""

mkdir -p "$LOG_ROOT"
: > "$LOG_FILE"

report_failure() {
    status="$1"
    reason="$2"
    {
        printf '\nLuminaCode installation failed.\n'
        printf '  stage: %s\n' "$CURRENT_STAGE"
        printf '  exit code: %s\n' "$status"
        printf '  error: %s\n' "$reason"
        printf '  log: %s\n' "$LOG_FILE"
        printf '  application: the deploy transaction did not complete; app.new is cleaned and a swapped app is restored automatically\n'
        printf 'Fix the reported error and run make install again.\n'
    } | tee -a "$LOG_FILE" >&2
    exit "$status"
}

stage_error_summary() {
    stage_log="$1"
    awk '
        {
            line = $0
            sub(/\r$/, "", line)
            if (line ~ /^[[:space:]]*$/) {
                next
            }
            lower = tolower(line)
            last = line
            make_wrapper = (lower ~ /(^|\/)(g?make)(\[[0-9]+\])?:[[:space:]]+\*\*\*/)
            if (!make_wrapper) {
                last_detail = line
            }
            if (!make_wrapper && lower ~ /(^|[^[:alpha:]])(error|fatal|failed|failure|cannot|unable|not found|no such file|permission denied|checksum|timed out|timeout)([^[:alpha:]]|$)/) {
                relevant = line
            }
        }
        END {
            if (relevant != "") {
                print relevant
            } else if (last_detail != "") {
                print last_detail
            } else if (last != "") {
                print last
            } else {
                print "installation command exited without an error message"
            }
        }
    ' "$stage_log"
}

on_signal() {
    report_failure 130 "installation was interrupted"
}
trap on_signal HUP INT TERM

run_stage() {
    CURRENT_STAGE="$1"
    target="$2"
    rc_file="$LOG_ROOT/.install-$RUN_ID.rc"
    CURRENT_STAGE_LOG="$LOG_ROOT/.install-$RUN_ID.stage.log"
    rm -f "$rc_file"
    : > "$CURRENT_STAGE_LOG"

    {
        printf '\n==> %s\n' "$CURRENT_STAGE"
        printf '    target: %s\n' "$target"
    } | tee -a "$LOG_FILE"

    set +e
    (
        "$MAKE_BIN" --no-print-directory "$target"
        command_status=$?
        printf '%s\n' "$command_status" > "$rc_file"
        exit "$command_status"
    ) 2>&1 | tee -a "$LOG_FILE" "$CURRENT_STAGE_LOG"
    pipeline_status=$?
    set -e

    if [ -f "$rc_file" ]; then
        command_status="$(cat "$rc_file")"
    else
        command_status="$pipeline_status"
    fi
    rm -f "$rc_file"

    if [ "$command_status" -ne 0 ]; then
        error_summary="$(stage_error_summary "$CURRENT_STAGE_LOG")"
        rm -f "$CURRENT_STAGE_LOG"
        CURRENT_STAGE_LOG=""
        report_failure "$command_status" "$error_summary"
    fi
    if [ "$pipeline_status" -ne 0 ]; then
        rm -f "$CURRENT_STAGE_LOG"
        CURRENT_STAGE_LOG=""
        report_failure "$pipeline_status" "could not write the installation log"
    fi
    rm -f "$CURRENT_STAGE_LOG"
    CURRENT_STAGE_LOG=""
}

printf 'LuminaCode installation log: %s\n' "$LOG_FILE" | tee -a "$LOG_FILE"
run_stage "hardware and model preflight" "_install-preflight"
run_stage "application and native runtime build" "_install-build"
run_stage "model publication and atomic deployment" "_install-deploy"

CURRENT_STAGE="complete"
{
    printf '\nLuminaCode installation completed successfully.\n'
    printf '  log: %s\n' "$LOG_FILE"
} | tee -a "$LOG_FILE"
