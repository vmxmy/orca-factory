#!/usr/bin/env bash
# Provider boundary: legacy shell envelopes, native driver mutations in GitHub mode.
set -Eeuo pipefail
case "${AGENT_ISSUE_BACKEND:-linear}" in
  linear)
    { printf '%s coordinator\n' "$(date +%s)" >> "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"; } 2>/dev/null || true
    exec orca linear "$@" ;;
  github) : "${AGENT_GH_REPO:?AGENT_GH_REPO required}" ;;
  *) echo 'issues: unsupported AGENT_ISSUE_BACKEND' >&2; exit 2 ;;
esac
fail() { echo "issues: $*" >&2; exit 2; }
# Only reshape the public CLI schema; ownership and metadata stay in the driver.
normalize='
  def complete_issue($comments):
    if .complete != true or (.identifier | type != "string") or (.id | type != "string")
      or (.state.type | IN("backlog","unstarted","started","completed","canceled","duplicate") | not)
      or (.labels.nodes | type != "array") or (.comments.nodes | type != "array")
      or (.relations.nodes | type != "array") or (.inverseRelations.nodes | type != "array")
      or .labels.pageInfo.hasNextPage == true
      or ($comments and (.commentsComplete != true or .comments.pageInfo.hasNextPage == true))
      or .relations.pageInfo.hasNextPage == true or .inverseRelations.pageInfo.hasNextPage == true
    then error("incomplete issue data") else . end;
  def issue($comments): complete_issue($comments) | .labels = .labels.nodes | .comments = .comments.nodes
    | .blockedBy = [.inverseRelations.nodes[] | select(.type == "blocks") | .issue];
  if .ok != true then error("issue read failed") else . end |
'
command="${1:-}"; [[ $# == 0 ]] || shift
case "$command" in
  list-issues)
    label=''; query=''
    while (( $# )); do
      case "$1" in
        --label) label="${2:?label}"; shift 2 ;;
        --query) query="${2:?query}"; shift 2 ;;
        --team|--project|--order-by) [[ $# -ge 2 ]] || fail "missing $1 value"; shift 2 ;;
        --json|--include-archived) shift ;;
        *) fail "unsupported list option $1" ;;
      esac
    done
    out=$(orca-factory-driver issues snapshot --json) || exit $?
    jq -e --arg label "$label" --arg query "$query" "$normalize"'
      if .result.complete != true or (.result.issues | type != "array") then error("incomplete snapshot") else . end
      | .result.issues |= map(issue(false) | select($label == "" or any(.labels[]; .name == $label or .id == $label))
        | select($query == "" or (.title | contains($query))))' <<< "$out"
    ;;
  issue)
    id="${1:?issue identity}"; shift
    for arg in "$@"; do case "$arg" in --comments|--relations|--json) ;; *) fail "unsupported issue option $arg" ;; esac; done
    out=$(orca-factory-driver issues read --id "$id" --json) || exit $?
    jq -e "$normalize"'
      .result.issue |= issue(true)
      | .result.comments = .result.issue.comments
      | .result.relations = [.result.issue.blockedBy[] | {relationship:"blockedBy",relatedIssue:.}]
      | .result.complete = true' <<< "$out"
    ;;
  status|label|comment)
    operation="${1:?operation}"; id="${2:?issue identity}"; shift 2
    args=(issues update --id "$id")
    case "$command:$operation" in status:set|label:add|label:remove|comment:add) ;; *) fail 'unsupported mutation' ;; esac
    body=''; body_file=''
    while (( $# )); do
      case "$command:$1" in
        status:--to) args+=(--state "${2:?state}"); shift 2 ;;
        label:--label) args+=("--$operation-label" "${2:?label}"); shift 2 ;;
        comment:--body) body="${2-}"; shift 2 ;;
        comment:--body-file) body_file="${2:?file}"; shift 2 ;;
        *:--json) shift ;;
        *) fail "unsupported mutation option $1" ;;
      esac
    done
    if [[ "$command" == comment ]]; then
      if [[ -n "$body_file" ]]; then exec orca-factory-driver issues comment --id "$id" --body-file "$body_file"; fi
      printf '%s' "$body" | orca-factory-driver issues comment --id "$id" --body-file -
    else exec orca-factory-driver "${args[@]}"; fi
    ;;
  snapshot|read|create|update|validate) exec orca-factory-driver issues "$command" "$@" ;;
  *) fail 'use list-issues, issue, status, label, comment or driver snapshot/read/create/update/validate' ;;
esac
