#!/usr/bin/env bash
# End-to-end smoke test of a running radio-strangler (compose, Render, anything with a URL).
# It exercises every migration stage against real services: legacy pass-through, shadow
# comparisons, admin auth, a live switch to Go with a contract diff, and the rollback.
#
#   BASE_URL=http://localhost:8080 ADMIN_TOKEN=... DEMO_INJECT_BUGS=true scripts/smoke.sh
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
ADMIN_TOKEN="${ADMIN_TOKEN:-dev-admin-token}"
# Must match the server: with the demo bug on, shadow is *expected* to report "[] vs null".
DEMO_INJECT_BUGS="${DEMO_INJECT_BUGS:-true}"
READY_TIMEOUT="${READY_TIMEOUT:-120}"

step() { echo "→ $*"; }
fail() {
	echo "✗ $*" >&2
	exit 1
}

for cmd in curl jq diff; do
	command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
programs_changed=false
cleanup() {
	# Never leave the shared environment with /api/programs on Go because a check failed midway.
	if [[ "$programs_changed" == true ]]; then
		echo "  (restoring /api/programs to legacy)" >&2
		admin -X PUT -d '{"mode":"legacy"}' "$BASE_URL/admin/routes/api/programs" >/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT

# --fail-with-body: a 401/500 from the admin API must stop the test, not be parsed as empty data.
admin() {
	curl -sS --fail-with-body --max-time 30 -H "Authorization: Bearer $ADMIN_TOKEN" "$@"
}

# fetch PATH OUT_PREFIX: saves headers and body, prints nothing.
fetch() {
	curl -sS --max-time 90 -D "$2.headers" -o "$2.body" "$BASE_URL$1"
}

# header FILE NAME: value of a response header, case-insensitive.
header() {
	tr -d '\r' <"$1" | awk -F': ' -v name="$(echo "$2" | tr '[:upper:]' '[:lower:]')" \
		'tolower($1) == name { print $2; exit }'
}

latest_comparison_id() {
	admin "$BASE_URL/admin/comparisons?route=$1&limit=1" | jq -r '.data[0].id // 0' ||
		fail "cannot read /admin/comparisons (is ADMIN_TOKEN right?)"
}

# wait_for_comparison ROUTE AFTER_ID: polls until a comparison newer than AFTER_ID exists.
wait_for_comparison() {
	local route="$1" after="$2" body
	for _ in $(seq 1 20); do
		body="$(admin "$BASE_URL/admin/comparisons?route=$route&limit=1")"
		if [[ "$(jq -r '.data[0].id // 0' <<<"$body")" -gt "$after" ]]; then
			jq -c '.data[0]' <<<"$body"
			return 0
		fi
		sleep 1
	done
	fail "no shadow comparison recorded for $route after 20s"
}

# check_comparison ROUTE JSON: the outcome must match what the server is configured to do.
check_comparison() {
	local route="$1" cmp="$2"
	[[ "$(jq -r '.error' <<<"$cmp")" == "" ]] || fail "$route: shadow error: $(jq -r '.error' <<<"$cmp")"
	if jq -e '.diffs[] | select(contains("generated_at"))' <<<"$cmp" >/dev/null; then
		fail "$route: ignored field generated_at was compared: $(jq -c '.diffs' <<<"$cmp")"
	fi
	if [[ "$DEMO_INJECT_BUGS" == true ]]; then
		# The only acceptable differences are the injected empty-tags bug.
		jq -e '.match == false and (.diffs | length > 0) and all(.diffs[]; test("\\.tags: legacy=\\[\\] go=null$"))' \
			<<<"$cmp" >/dev/null || fail "$route: expected only the injected tags bug, got $(jq -c '{match, diffs}' <<<"$cmp")"
		echo "  shadow caught the injected bug: $(jq -c '.diffs' <<<"$cmp")"
	else
		[[ "$(jq -r '.match' <<<"$cmp")" == true ]] || fail "$route: expected match=true, got $(jq -c '{match, diffs}' <<<"$cmp")"
		echo "  shadow match=true"
	fi
}

# wait_for_backend PATH BACKEND: polls until X-Served-By equals BACKEND.
wait_for_backend() {
	local got=""
	for _ in $(seq 1 20); do
		fetch "$1" "$tmp/poll"
		got="$(header "$tmp/poll.headers" X-Served-By)"
		[[ "$got" == "$2" ]] && return 0
		sleep 0.5
	done
	fail "$1 still served by '$got', expected '$2'"
}

step "1. waiting for $BASE_URL/readyz (up to ${READY_TIMEOUT}s)"
deadline=$((SECONDS + READY_TIMEOUT))
until curl -fsS --max-time 10 "$BASE_URL/readyz" >/dev/null 2>&1; do
	((SECONDS < deadline)) || fail "service not ready after ${READY_TIMEOUT}s"
	sleep 2
done

step "2. /api/programs is served by legacy"
fetch /api/programs "$tmp/programs"
served="$(header "$tmp/programs.headers" X-Served-By)"
[[ "$served" == legacy ]] || fail "/api/programs served by '$served', expected legacy"
jq -S . "$tmp/programs.body" >"$tmp/programs.legacy.json" || fail "/api/programs did not return JSON"
echo "  $(jq '.data | length' "$tmp/programs.body") programs"

# Results are assigned before use: with set -e, a failing $(...) aborts an assignment but would be
# silently ignored if passed straight as an argument.
step "3. /api/tracks in shadow mode: legacy answers, comparison is recorded"
before="$(latest_comparison_id /api/tracks)"
fetch /api/tracks "$tmp/tracks"
[[ "$(header "$tmp/tracks.headers" X-Route-Mode)" == shadow ]] || fail "/api/tracks is not in shadow mode"
[[ "$(header "$tmp/tracks.headers" X-Served-By)" == legacy ]] || fail "/api/tracks in shadow must be served by legacy"
cmp="$(wait_for_comparison /api/tracks "$before")"
check_comparison /api/tracks "$cmp"

step "4. /api/now-playing comparison ignores generated_at"
before="$(latest_comparison_id /api/now-playing)"
fetch /api/now-playing "$tmp/now"
cmp="$(wait_for_comparison /api/now-playing "$before")"
check_comparison /api/now-playing "$cmp"

step "5. admin API rejects requests without a token"
code="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/admin/routes")"
[[ "$code" == 401 ]] || fail "GET /admin/routes without token returned $code, expected 401"

step "6. switch /api/programs to go and compare the contract"
programs_changed=true
admin -X PUT -d '{"mode":"go"}' "$BASE_URL/admin/routes/api/programs" | jq -e '.data.mode == "go"' >/dev/null ||
	fail "PUT /admin/routes/api/programs to go failed"
wait_for_backend /api/programs go
jq -S . "$tmp/poll.body" >"$tmp/programs.go.json"
if ! diff -u "$tmp/programs.legacy.json" "$tmp/programs.go.json"; then
	fail "Go response for /api/programs differs from legacy"
fi
echo "  Go and legacy JSON are identical"

step "7. roll back /api/programs to legacy"
admin -X PUT -d '{"mode":"legacy"}' "$BASE_URL/admin/routes/api/programs" | jq -e '.data.mode == "legacy"' >/dev/null ||
	fail "PUT /admin/routes/api/programs to legacy failed"
wait_for_backend /api/programs legacy
programs_changed=false

echo "✓ smoke test passed"
