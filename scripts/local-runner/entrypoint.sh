#!/bin/bash
set -euo pipefail
# JIT is delivered over stdin, never Docker metadata, a host mount, or argv.
IFS= read -r jit
if [[ -z "$jit" ]]; then exit 2; fi
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
unset jit ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT
# Official CommandSettings consumes/removes ACTIONS_RUNNER_INPUT_* before jobs.
exec /home/runner/bin/Runner.Listener run
