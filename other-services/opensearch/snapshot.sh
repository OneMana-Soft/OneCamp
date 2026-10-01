#!/bin/sh
# The search indices in a backup, and back out of one.
#
#   COMPOSE="docker compose ..." snapshot.sh save    <backup dir>
#   COMPOSE="docker compose ..." snapshot.sh restore <backup dir>
#
# WHY. A backup held Postgres, Dgraph and the files but not OpenSearch, so a
# restore left search as it was: full of entries for content that no longer
# existed after a restore to an earlier point, and empty after a restore onto a
# new machine, with the AI's embeddings gone too. Rebuilding them would mean
# re-deriving eleven index shapes and paying to re-embed everything; a snapshot
# taken beside the database dumps brings back exactly what matched them.
#
# The repository is emptied before each backup, so the archive of it that
# lands in the backup is that one snapshot and nothing older.
#
# Never fails a backup: a server whose OpenSearch predates path.repo (updated
# but not yet recreated) gets a warning and a backup without search, which is
# what every backup was before.
set -eu

mode=${1:-}
dir=${2:-}
if [ -z "$mode" ] || [ -z "$dir" ] || [ -z "${COMPOSE:-}" ]; then
	echo "usage: COMPOSE=\"docker compose ...\" $0 save|restore <backup dir>" >&2
	exit 2
fi

repo_path=/usr/share/opensearch/snapshots
repo=onecamp-backup
archive="$dir/opensearch.tar.gz"

# The service is opensearch-node1 in the customer stack and
# opensearch-onecamp-node1 on the demo host; find it rather than assume.
svc=$($COMPOSE config --services 2>/dev/null | grep -E '^opensearch.*node1$' | head -1 || true)
if [ -z "$svc" ]; then
	echo "    skip  no search service in this stack"
	exit 0
fi

# os METHOD PATH [BODY] prints the response body, then the status on the last
# line. Runs inside the container so the admin password never leaves it; tries
# https (security plugin on) and then http (off).
os() {
	$COMPOSE exec -T "$svc" sh -c '
		for base in https://localhost:9200 http://localhost:9200; do
			if [ -n "$3" ]; then
				out=$(curl -sk -u "admin:${OPENSEARCH_INITIAL_ADMIN_PASSWORD:-}" -X "$1" \
					-H "Content-Type: application/json" -d "$3" -w "\n%{http_code}" "$base$2") || continue
			else
				out=$(curl -sk -u "admin:${OPENSEARCH_INITIAL_ADMIN_PASSWORD:-}" -X "$1" \
					-w "\n%{http_code}" "$base$2") || continue
			fi
			printf "%s\n" "$out"; exit 0
		done
		exit 1' _ "$1" "$2" "${3:-}"
}

status() { printf '%s\n' "$1" | tail -1; }
body() { printf '%s\n' "$1" | sed '$d'; }

# The snapshot directory is a bind mount Docker may have created as root; the
# node runs as 1000.
own_repo() {
	$COMPOSE exec -T -u root "$svc" sh -c "mkdir -p $repo_path && chown -R 1000:1000 $repo_path"
}

register() {
	r=$(os PUT "/_snapshot/$repo" "{\"type\":\"fs\",\"settings\":{\"location\":\"$repo_path\"}}") || return 1
	[ "$(status "$r")" = 200 ] || { body "$r" | head -c 300 >&2; echo >&2; return 1; }
}

snapshots() {
	r=$(os GET "/_snapshot/$repo/_all") || return 0
	body "$r" | grep -o '"snapshot":"[^"]*"' | cut -d'"' -f4
}

case "$mode" in
save)
	echo "    search"
	own_repo
	# Every backup starts from an empty repository. It only ever holds the one
	# snapshot being taken (the archive in the backup is the copy that lasts),
	# and starting clean means nothing left in it by an interrupted run, or by
	# a restore, can make this snapshot fail.
	os DELETE "/_snapshot/$repo" > /dev/null 2>&1 || true
	$COMPOSE exec -T -u root "$svc" sh -c "rm -rf $repo_path/* && chown -R 1000:1000 $repo_path"
	if ! register 2>/dev/null; then
		echo "    warn  search was not backed up: this OpenSearch has no snapshot directory yet."
		echo "          Recreate it once (make update does) and the next backup includes it."
		exit 0
	fi
	name="backup-$(date -u +%Y%m%dt%H%M%Sz)"
	# Every index but the system ones (security, task results): those belong to
	# the node, and restoring them over a live cluster fails or overwrites its users.
	r=$(os PUT "/_snapshot/$repo/$name?wait_for_completion=true" '{"indices":"*,-.*","include_global_state":false}') || {
		echo "    warn  search snapshot could not run"; exit 0; }
	if ! body "$r" | grep -q '"state":"SUCCESS"'; then
		echo "    warn  search snapshot did not succeed:"
		body "$r" | head -c 300; echo
		exit 0
	fi
	$COMPOSE exec -T "$svc" tar -czf - -C /usr/share/opensearch snapshots > "$archive.partial"
	gzip -t "$archive.partial"
	mv "$archive.partial" "$archive"
	echo "    ok    search ($(du -sh "$archive" | cut -f1))"
	;;
restore)
	if [ ! -s "$archive" ]; then
		echo "    skip  no search in this backup; results for content it no longer has are filtered out as they are met"
		exit 0
	fi
	gzip -t "$archive"
	own_repo
	# Unregister before the files change under it. OpenSearch caches what a
	# registered repository holds, so swapping its files in place (restore
	# takes a backup of the current state first, which writes a newer
	# snapshot there) left it reading the old listing: "holds no snapshot".
	os DELETE "/_snapshot/$repo" > /dev/null || true
	$COMPOSE exec -T -u root "$svc" sh -c "rm -rf $repo_path/* && tar -xzf - -C /usr/share/opensearch && chown -R 1000:1000 $repo_path" < "$archive"
	register || { echo "    FAIL  this OpenSearch has no snapshot directory; recreate it (make update) and restore again"; exit 1; }
	name=$(snapshots | tail -1)
	[ -n "$name" ] || { echo "    FAIL  the search archive holds no snapshot"; exit 1; }
	r=$(os GET "/_snapshot/$repo/$name")
	indices=$(body "$r" | grep -o '"indices":\[[^]]*\]' | head -1 | sed 's/^"indices":\[//; s/\]$//; s/"//g')
	# Only what the snapshot brings back is replaced; anything else is left alone.
	[ -n "$indices" ] && os DELETE "/$indices?ignore_unavailable=true" > /dev/null
	r=$(os POST "/_snapshot/$repo/$name/_restore?wait_for_completion=true" '{"indices":"*,-.*","include_global_state":false}')
	if [ "$(status "$r")" != 200 ]; then
		echo "    FAIL  search restore:"
		body "$r" | head -c 300; echo
		exit 1
	fi
	echo "    ok    search restored"
	;;
*)
	echo "unknown mode $mode" >&2
	exit 2
	;;
esac
