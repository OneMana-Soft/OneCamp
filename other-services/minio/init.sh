#!/bin/bash
# The bucket uploads go to, and the access key the API signs in with.
#
# Run by the minio-init service in the compose file, once MinIO answers its
# health check. Nothing waits for it: the API starts without files.
#
# WHY. Nothing ever made either. .sample.env names an access key
# (MINIO_ACCESS_KEY_ID) and a bucket (USER_UPLOAD_BUCKET_NAME), and MinIO starts
# knowing only its root user. So on every fresh install, uploads, call
# recordings and imports all failed ("The Access Key Id you provided does not
# exist in our records") until an operator made both by hand.
#
# Safe on every start, and on an install that was set up by hand:
#   - a bucket that exists is left as it is;
#   - an access key that exists, as a user or a service account, is left as it
#     is. Its secret is never changed and never printed. `mc admin user add` on
#     an existing user would replace its secret, so a key is made only when
#     MinIO says there is no such user AND no such service account;
#   - when files live in an attached bucket (make storage-attach), they aren't
#     this machine's to set up, so nothing is done.
#
# Exits non-zero only when it can't sign in to MinIO as root, which means the
# object store itself is broken. A bucket or key MinIO refuses is reported, and
# the API still starts: everything but files works, and `make storage-check`
# (part of make verify) says what is wrong.
set -u
umask 077

MC_CONFIG_DIR=$(mktemp -d)
export MC_CONFIG_DIR
trap 'rm -rf "$MC_CONFIG_DIR"' EXIT

say() { printf 'minio-init: %s\n' "$*"; }

endpoint=${MINIO_ENDPOINT:-http://minio:9000}
root_user=${MINIO_ROOT_USER:-}
bucket=${UPLOAD_BUCKET:-}
key=${API_ACCESS_KEY:-}

if [ -n "${STORAGE_ATTACHED_BUCKET:-}" ]; then
	say "files live in the attached bucket $STORAGE_ATTACHED_BUCKET (make storage-status); nothing to set up here"
	exit 0
fi

# Credentials go in on stdin, so no secret is ever in a process list. Tried
# for half a minute: compose starts this once MinIO is healthy, but `docker
# compose restart` (make restore runs one) restarts the two together.
tries=${MINIO_INIT_TRIES:-15}
until printf '%s\n%s\n' "$root_user" "${MINIO_ROOT_PASSWORD:-}" | mc alias set local "$endpoint" >/dev/null 2>&1; do
	tries=$((tries - 1))
	if [ "$tries" -le 0 ]; then
		say "FAILED: couldn't sign in to MinIO at $endpoint as its root user (MINIO_USER and MINIO_PASSWORD in .env)"
		exit 1
	fi
	sleep "${MINIO_INIT_PAUSE:-2}"
done

if [ -z "$bucket" ]; then
	say "NOT SET UP: USER_UPLOAD_BUCKET_NAME is empty, so there's no bucket to make. Set it in .env, then restart."
	exit 0
fi
if out=$(mc mb --ignore-existing "local/$bucket" 2>&1); then
	say "bucket $bucket is ready"
else
	say "NOT SET UP: MinIO refused to make bucket $bucket (USER_UPLOAD_BUCKET_NAME): $out"
	exit 0
fi

if [ -z "$key" ]; then
	say "NOT SET UP: MINIO_ACCESS_KEY_ID is empty, so the API has no access key to sign in with"
	exit 0
fi
if [ "$key" = "$root_user" ]; then
	say "the API signs in as MinIO's root user, so there's no access key to make"
	exit 0
fi

if mc admin user svcacct info local "$key" >/dev/null 2>&1; then
	say "access key $key exists (a service account); left as it is"
	exit 0
fi
if user_info=$(mc admin user info local "$key" 2>&1); then
	say "access key $key exists (a user); left as it is"
	exit 0
fi
svcacct_info=$(mc admin user svcacct info local "$key" 2>&1)
case "$user_info" in
*"does not exist"*) ;;
*)
	say "couldn't tell whether access key $key exists, so it's left alone: $user_info"
	exit 0
	;;
esac
case "$svcacct_info" in
*"not found"* | *"does not exist"*) ;;
*)
	say "couldn't tell whether access key $key exists, so it's left alone: $svcacct_info"
	exit 0
	;;
esac

secret=${API_SECRET_KEY:-}
if ! out=$(printf '%s\n%s\n' "$key" "$secret" | mc admin user add local 2>&1); then
	[ -n "$secret" ] && out=${out//"$secret"/<secret>}
	say "NOT SET UP: MinIO refused to make access key $key: $out"
	exit 0
fi
# readwrite is every S3 action and nothing administrative: the key can't make
# users or change policies, and it keeps working if the bucket is renamed.
if ! out=$(mc admin policy attach local readwrite --user "$key" 2>&1); then
	# Taken back, so the next start makes it again rather than finding a key
	# with no access and leaving it.
	mc admin user remove local "$key" >/dev/null 2>&1
	say "NOT SET UP: couldn't give access key $key the readwrite policy: $out"
	exit 0
fi
# The proof is the key itself opening the bucket, as the API will.
if printf '%s\n%s\n' "$key" "$secret" | mc alias set api "$endpoint" >/dev/null 2>&1 &&
	mc ls "api/$bucket" >/dev/null 2>&1; then
	say "made access key $key for the API (policy readwrite); it opens bucket $bucket"
else
	say "made access key $key for the API (policy readwrite), but it can't open bucket $bucket yet"
fi
