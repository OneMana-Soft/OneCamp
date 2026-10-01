#!/bin/bash
# ═══════════════════════════════════════════════════════════════════════════════
# OneCamp — Find media already stored in OpenSearch
# ═══════════════════════════════════════════════════════════════════════════════
# Usage: ./opensearch-find-media.sh [OPENSEARCH_URL]
#
# Examples:
#   ./opensearch-find-media.sh
#   ./opensearch-find-media.sh http://localhost:9200
#   OPENSEARCH_USER=admin OPENSEARCH_PASS=secret ./opensearch-find-media.sh https://os:9200
#
# WHY THIS EXISTS
#   Every write path now strips embedded media before it reaches the cluster, so
#   nothing NEW can carry an image into the index. Documents indexed BEFORE that
#   change are untouched, and they are the ones that hurt: re-indexing one of them
#   is what OOM-killed the beta node. This finds them, so they get handled
#   deliberately instead of being discovered by an outage.
#
# HOW IT DETECTS
#   By searching the ANALYSED text fields for the tokens media leaves behind.
#   Deliberately NOT a wildcard on a .keyword subfield: OneCamp maps these fields
#   as plain "text" with no .keyword at all, and even where one exists it defaults
#   to ignore_above:256 — so a keyword wildcard would silently skip exactly the
#   oversized documents being hunted.
#
#   A data URI like "data:image/png;base64,iVBOR..." tokenises to
#   [data, image, png, base64, iVBOR...], so the token "base64" is a reliable
#   marker. Inline SVG yields the token "svg".
#
#   Consequence worth knowing: a document that merely discusses "base64" or "SVG"
#   in prose is reported too. That is the safe direction for a read-only report —
#   it over-reports rather than missing a live hazard. Check the samples.
#
# WHAT IT DOES NOT DO
#   Read-only. It counts and samples; it changes nothing. Remediation is printed
#   at the end for you to run when you choose.
#
# EXIT CODES
#   0  no affected documents found
#   1  affected documents found (details printed)
#   2  could not reach the cluster
# ═══════════════════════════════════════════════════════════════════════════════

set -uo pipefail

OS_URL="${1:-${OPENSEARCH_URL:-http://localhost:9200}}"
OS_URL="${OS_URL%/}"

AUTH=()
if [[ -n "${OPENSEARCH_USER:-}" ]]; then
  AUTH=(-u "${OPENSEARCH_USER}:${OPENSEARCH_PASS:-}")
fi

curl_os() {
  curl --silent --show-error --max-time 30 \
    -k "${AUTH[@]}" -H 'Content-Type: application/json' "$@"
}

if ! curl_os --fail "${OS_URL}" >/dev/null 2>&1; then
  echo "ERROR: cannot reach OpenSearch at ${OS_URL}" >&2
  echo "       Pass the URL as the first argument, or set OPENSEARCH_URL." >&2
  echo "       If the cluster has security enabled, set OPENSEARCH_USER and OPENSEARCH_PASS." >&2
  exit 2
fi

echo "OpenSearch: ${OS_URL}"
echo "Scanning the fields that can hold user prose. Everything else in these"
echo "indexes is an id, a name or a timestamp and cannot carry a payload."
echo

# index:field — field names taken from the json tags in models/openSearch/struct.go
# and the mappings in initializers/opensearchInit/connectOpensearch.go.
TARGETS=(
  "docs:doc_body"
  "docs:doc_snippet"
  "posts:post_body"
  "chats:chat_body"
  "comments:comment_body"
  "tasks:task_desc"
  "tasks:task_name"
  "boards:board_snippet"
  "ai_embeddings:content_text"
)

media_query() {
  local field="$1"
  cat <<JSON
{
  "size": 3,
  "_source": false,
  "query": {
    "bool": {
      "should": [
        { "match": { "${field}": "base64" } },
        { "match": { "${field}": "svg" } }
      ],
      "minimum_should_match": 1
    }
  }
}
JSON
}

TOTAL_AFFECTED=0
FOUND_DETAIL=""
ANY_QUERY_RAN=0

for target in "${TARGETS[@]}"; do
  index="${target%%:*}"
  field="${target##*:}"

  if ! curl_os --fail "${OS_URL}/${index}" >/dev/null 2>&1; then
    printf '  %-14s %-16s index not present, skipped\n' "${index}" "${field}"
    continue
  fi

  resp="$(curl_os -X POST "${OS_URL}/${index}/_search" -d "$(media_query "${field}")" 2>/dev/null)"

  if [[ -z "${resp}" ]] || printf '%s' "${resp}" | grep -q '"error"'; then
    printf '  %-14s %-16s QUERY FAILED (field not mapped?)\n' "${index}" "${field}"
    continue
  fi

  ANY_QUERY_RAN=1

  # hits.total.value, without requiring jq.
  count="$(printf '%s' "${resp}" \
    | tr '{},' '\n\n\n' \
    | grep -o '"value":[0-9]*' \
    | head -1 \
    | grep -o '[0-9]*' || true)"
  count="${count:-0}"

  if [[ "${count}" -gt 0 ]]; then
    printf '  %-14s %-16s %s document(s) AFFECTED\n' "${index}" "${field}" "${count}"
    TOTAL_AFFECTED=$((TOTAL_AFFECTED + count))
    ids="$(printf '%s' "${resp}" | grep -o '"_id":"[^"]*"' | cut -d'"' -f4 | tr '\n' ' ')"
    FOUND_DETAIL+="    ${index}.${field}: ${ids}"$'\n'
  else
    printf '  %-14s %-16s clean\n' "${index}" "${field}"
  fi
done

echo

if [[ "${ANY_QUERY_RAN}" -eq 0 ]]; then
  echo "No index was queryable. Nothing was checked — do not read this as 'clean'." >&2
  exit 2
fi

if [[ "${TOTAL_AFFECTED}" -eq 0 ]]; then
  echo "No stored media found. Nothing to remediate."
  exit 0
fi

echo "${TOTAL_AFFECTED} document match(es) hold a media marker."
echo "Sample ids (up to 3 per field):"
printf '%s' "${FOUND_DETAIL}"
cat <<'REMEDY'

REMEDIATION
  These documents are safe to leave in place. They are only dangerous when
  RE-INDEXED, and every write path now sanitises on the way out, so the next save
  of each one cleans it. Two options:

  1. Let it heal (no action). Each document is cleaned the next time it is edited.
     Reasonable when the count is small and the docs are active.

  2. Force it, one index at a time, off-peak:
       POST /<index>/_update_by_query?wait_for_completion=false&requests_per_second=50

     Keep requests_per_second low and do ONE index at a time. An unthrottled
     _update_by_query across documents containing large payloads reproduces the
     exact load pattern that killed the node.

  For ai_embeddings, prefer re-embedding over an update: the stored vector was
  computed from the polluted text, so it is meaningless until regenerated.
  Content re-embeds on its next edit, or via the admin reindex path.

  Re-run this script afterwards to confirm.
REMEDY

exit 1
