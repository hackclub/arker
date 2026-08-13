#!/usr/bin/env bash
# Social-contract E2E: submits real archives to a LOCAL Arker (make dev, :8080),
# then validates the unified API contract end-to-end, including actual nonzero
# media bytes. Free/native paths only — never Instagram, never X, never Bright Data.
#
# Usage: BASE=http://localhost:8080 ./scripts/social-contract-e2e.sh
# Requires: curl, jq. Local stack: `make dev` (admin/admin).
set -u
BASE="${BASE:-http://localhost:8080}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-admin}"
JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT
POLL_TIMEOUT_SECS="${POLL_TIMEOUT_SECS:-420}"

# --- probe URLs (free, anonymous; override via env when a default rots) ---
URL_ORDINARY="${URL_ORDINARY:-https://example.com/}"
URL_YT_VIDEO="${URL_YT_VIDEO:-https://www.youtube.com/watch?v=jNQXAC9IVRw}"   # "Me at the zoo"
URL_YT_VIDEO_ALT="${URL_YT_VIDEO_ALT:-https://youtu.be/jNQXAC9IVRw}"          # same video, alt spelling (canonical identity check)
URL_YT_SHORT="${URL_YT_SHORT:-https://www.youtube.com/shorts/K6rUDI0MVXI}"  # verified: serves at /shorts/ with no redirect
URL_VIMEO="${URL_VIMEO:-https://vimeo.com/22439234}"                          # "The Mountain" — DRM-free (76979871 has DRM audio: explicit-failure case)
URL_REDDIT_GALLERY="${URL_REDDIT_GALLERY:-}"  # set at run time: stable public gallery post
URL_BLUESKY_IMAGE="${URL_BLUESKY_IMAGE:-https://bsky.app/profile/bsky.app/post/3msqpuobiwk2t}"  # verified via gallery-dl --simulate
URL_IMGUR_ALBUM="${URL_IMGUR_ALBUM:-}"        # optional; covered by fixtures
URL_FLICKR_PHOTO="${URL_FLICKR_PHOTO:-https://www.flickr.com/photos/library_of_congress/2163445674/}"  # verified via gallery-dl --simulate

pass=0; fail=0; declare -a results=()
ok()   { pass=$((pass+1)); results+=("PASS  $1"); }
bad()  { fail=$((fail+1)); results+=("FAIL  $1"); }
note() { results+=("NOTE  $1"); }

# --- login + API key ---
curl -sS -c "$JAR" -o /dev/null "$BASE/login"
curl -sS -b "$JAR" -c "$JAR" -o /dev/null -X POST "$BASE/login" \
  -d "username=$ADMIN_USER&password=$ADMIN_PASS"
KEY_JSON=$(curl -sS -b "$JAR" -X POST "$BASE/admin/api-keys" \
  -H 'Content-Type: application/json' \
  -d '{"username":"e2e","app_name":"contract-e2e","environment":"test"}')
API_KEY=$(echo "$KEY_JSON" | jq -r '.api_key // .key // empty')
if [ -z "$API_KEY" ]; then echo "Could not create API key: $KEY_JSON"; exit 1; fi
AUTH=(-H "Authorization: Bearer $API_KEY")

submit() { # $1=url -> echoes short_id
  curl -sS "${AUTH[@]}" -X POST "$BASE/api/v1/archive" \
    -H 'Content-Type: application/json' -d "{\"url\":\"$1\"}" | jq -r '.short_id // empty'
}

wait_done() { # $1=short_id -> echoes final result JSON (or empty on timeout)
  local start=$(date +%s) res
  while :; do
    res=$(curl -sS "${AUTH[@]}" "$BASE/api/v1/archive/$1")
    [ "$(echo "$res" | jq -r '.capture_done')" = "true" ] && { echo "$res"; return; }
    [ $(( $(date +%s) - start )) -gt "$POLL_TIMEOUT_SECS" ] && { echo "$res"; return; }
    sleep 10
  done
}

media_bytes_ok() { # $1=result json, $2=label — every media URL must serve >0 bytes
  local n i murl size ctype
  n=$(echo "$1" | jq '.social_post.media | length')
  [ "$n" -gt 0 ] || { bad "$2: no media entries"; return 1; }
  for i in $(seq 0 $((n-1))); do
    murl=$(echo "$1" | jq -r ".social_post.media[$i].url")
    size=$(curl -sSL "${AUTH[@]}" -o /dev/null -w '%{size_download}' "$murl")
    ctype=$(curl -sSL "${AUTH[@]}" -o /dev/null -w '%{content_type}' -r 0-1024 "$murl" 2>/dev/null || true)
    if [ "${size:-0}" -gt 1000 ]; then ok "$2: media[$i] $size bytes ($ctype)"; else bad "$2: media[$i] only ${size:-0} bytes"; fi
  done
}

check_social() { # $1=url $2=label $3=expect_fulfilled(yes/no)
  [ -z "$1" ] && { note "$2: no probe URL configured, skipped"; return; }
  local sid res st ful
  sid=$(submit "$1"); [ -z "$sid" ] && { bad "$2: submit failed"; return; }
  res=$(wait_done "$sid")
  st=$(echo "$res" | jq -r '.social_post.status // "null"')
  ful=$(echo "$res" | jq -r '.social_post.fulfilled // false')
  echo "$res" | jq -e '.social_post' >/dev/null 2>&1 || { bad "$2: social_post missing entirely"; return; }
  if [ "$3" = yes ]; then
    [ "$ful" = "true" ] && ok "$2: fulfilled" || bad "$2: expected fulfilled, got status=$st fulfilled=$ful"
    media_bytes_ok "$res" "$2"
    echo "$res" | jq -e '.social_post.post | (.id != null or .title != null or .text != null)' >/dev/null && \
      ok "$2: normalized post present" || bad "$2: normalized post empty"
    echo "$res" | jq -e '.social_post.raw_metadata | length > 0' >/dev/null && \
      ok "$2: raw metadata link present" || bad "$2: raw metadata missing"
    rmurl=$(echo "$res" | jq -r '.social_post.raw_metadata[0].url // empty')
    if [ -n "$rmurl" ]; then
      rbytes=$(curl -sSL "${AUTH[@]}" -o /dev/null -w '%{size_download}' "$rmurl")
      [ "${rbytes:-0}" -gt 50 ] && ok "$2: raw metadata retrievable ($rbytes bytes)" || bad "$2: raw metadata not retrievable"
    fi
    [ "$(echo "$res" | jq -r '.cost.total_usd')" = "0" ] && ok "$2: native cost \$0" || bad "$2: unexpected nonzero cost"
  else
    [ "$ful" = "false" ] && ok "$2: not-fulfilled correctly explicit (status=$st)" || bad "$2: false green"
    echo "$res" | jq -e '.social_post.failure.code' >/dev/null && ok "$2: failure code present" || bad "$2: no failure code"
  fi
}

# --- ordinary URL: compatible behavior, no social_post ---
sid=$(submit "$URL_ORDINARY")
res=$(wait_done "$sid")
if [ "$(echo "$res" | jq -r '.social_post')" = "null" ]; then ok "ordinary: no social_post"; else bad "ordinary: unexpected social_post"; fi
echo "$res" | jq -e '[.items[] | select(.status=="completed")] | length >= 2' >/dev/null && \
  ok "ordinary: mhtml+screenshot completed" || bad "ordinary: base archives incomplete"

# --- social probes ---
check_social "$URL_YT_VIDEO"       "youtube-video"   yes
check_social "$URL_YT_SHORT"       "youtube-short"   yes
check_social "$URL_VIMEO"          "vimeo"           yes
check_social "$URL_REDDIT_GALLERY" "reddit-gallery"  yes
check_social "$URL_BLUESKY_IMAGE"  "bluesky-image"   yes
check_social "$URL_IMGUR_ALBUM"    "imgur-album"     yes

# --- find-or-create: canonical identity + join/find ---
if [ -n "$URL_YT_VIDEO" ]; then
  r1=$(curl -sS "${AUTH[@]}" -X POST "$BASE/api/v1/archive/find-or-create" -H 'Content-Type: application/json' -d "{\"url\":\"$URL_YT_VIDEO\"}")
  a1=$(echo "$r1" | jq -r '.action'); s1=$(echo "$r1" | jq -r '.short_id')
  r2=$(curl -sS "${AUTH[@]}" -X POST "$BASE/api/v1/archive/find-or-create" -H 'Content-Type: application/json' -d "{\"url\":\"$URL_YT_VIDEO_ALT\"}")
  a2=$(echo "$r2" | jq -r '.action'); s2=$(echo "$r2" | jq -r '.short_id')
  if [ "$a2" = "found" ] || [ "$a2" = "in_progress" ]; then ok "find-or-create: alt spelling joined ($a1/$a2)"; else bad "find-or-create: alt spelling created new capture (canonical identity gap) ($a1/$a2 $s1/$s2)"; fi
fi

echo; echo "=== social-contract E2E results ==="
printf '%s\n' "${results[@]}"
echo "PASS=$pass FAIL=$fail"
[ "$fail" -eq 0 ]
