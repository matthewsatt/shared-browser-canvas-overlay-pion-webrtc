#!/usr/bin/env bash
# Checks a server before `docker compose up -d --build`: the .env settings,
# Docker, and what the host must provide (user namespaces for Chromium's
# sandbox, DNS, ports, IMDSv2 on EC2). Changes nothing. From the repo folder:
#
#   ./deploy/preflight.sh            (reads ./.env)
#   ENV_FILE=other.env ./deploy/preflight.sh
set -u
cd "$(dirname "$0")/.."
ENV_FILE="${ENV_FILE:-.env}"
errors=0
warnings=0
fail() { echo "  FAIL  $*"; errors=$((errors + 1)); }
warn() { echo "  WARN  $*"; warnings=$((warnings + 1)); }
ok() { echo "  ok    $*"; }

if [ ! -f "$ENV_FILE" ]; then
  echo "No $ENV_FILE here. Start with: cp .env.example .env"
  exit 1
fi
# Read KEY=VALUE lines without executing the file.
get() { grep -E "^[[:space:]]*$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/'; }
each() { echo "$1" | tr ',' '\n' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -v '^$'; }

echo "Settings ($ENV_FILE)"
case ",$(get COMPOSE_PROFILES)," in *,https,*) ok "HTTPS (Caddy) profile on" ;; *) fail "COMPOSE_PROFILES must include https, or nothing serves HTTPS" ;; esac

DOMAIN="$(get DOMAIN)"
if [ -z "$DOMAIN" ] || [ "$DOMAIN" = "collab.example.com" ]; then fail "DOMAIN is not set to your hostname"; else ok "DOMAIN=$DOMAIN"; fi

PUBLIC_IP="$(get PUBLIC_IP)"
if ! echo "$PUBLIC_IP" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || [ "$PUBLIC_IP" = "203.0.113.10" ]; then
  fail "PUBLIC_IP must be this server's public IP (on EC2, its Elastic IP)"
else ok "PUBLIC_IP=$PUBLIC_IP"; fi

TOKEN="$(get API_TOKEN)"
if [ "${#TOKEN}" -lt 32 ]; then fail "API_TOKEN must be at least 32 characters (generate: openssl rand -base64 32)"; else ok "API_TOKEN set (${#TOKEN} characters)"; fi

ORIGINS="$(get ALLOWED_ORIGINS)"
if [ -z "$ORIGINS" ] || [ "$ORIGINS" = "https://app.example.com" ]; then
  fail "ALLOWED_ORIGINS must list your app's address(es), e.g. https://app.yourcompany.com"
else
  for o in $(each "$ORIGINS"); do
    case "$o" in https://*) ok "allowed origin $o" ;; *) fail "ALLOWED_ORIGINS entry $o must use https://" ;; esac
  done
fi

PREFIXES="$(get ALLOWED_URL_PREFIXES)"
if [ -z "$PREFIXES" ] || echo "$PREFIXES" | grep -q 'example.com'; then
  fail "ALLOWED_URL_PREFIXES must list the pages the shared browser may show"
else
  for p in $(each "$PREFIXES"); do
    case "$p" in https://*) ok "allowed page prefix $p" ;; *) fail "ALLOWED_URL_PREFIXES entry $p must use https://" ;; esac
  done
fi

if [ "$(get HTTP_PUBLISH)" = "127.0.0.1:8080" ]; then ok "plain HTTP only on localhost"; else warn "set HTTP_PUBLISH=127.0.0.1:8080 so port 8080 isn't reachable from outside"; fi
case "$(get ALLOW_PRIVATE_NETWORKS)" in true | 1) fail "ALLOW_PRIVATE_NETWORKS must not be on in production" ;; esac
MAX_SESSIONS="$(get MAX_SESSIONS)"
MAX_SESSIONS="${MAX_SESSIONS:-10}"

echo "Docker"
if ! command -v docker >/dev/null 2>&1; then
  fail "docker is not installed (see DEPLOY.md)"
else
  if docker info >/dev/null 2>&1; then ok "docker daemon reachable"; else fail "can't reach the docker daemon (is it running? are you in the docker group?)"; fi
  if v="$(docker compose version --short 2>/dev/null)"; then ok "docker compose $v"; else fail "the docker compose plugin is not installed (see DEPLOY.md)"; fi
  if docker buildx version >/dev/null 2>&1; then ok "docker buildx available"; else fail "the docker buildx plugin is not installed; compose needs it to build (see DEPLOY.md)"; fi
fi

echo "Host"
if [ "$(uname -s)" != "Linux" ]; then
  warn "not Linux: host checks skipped (run this on the server)"
else
  n="$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo 0)"
  if [ "$n" -gt 0 ]; then ok "user namespaces available (Chromium sandbox)"; else fail "user namespaces are disabled (user.max_user_namespaces=0); Chromium's sandbox can't start"; fi
  r=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
  if [ -f "$r" ] && [ "$(cat $r)" = "1" ]; then
    fail "AppArmor blocks unprivileged user namespaces (Ubuntu 23.10+), so Chromium's sandbox can't start. Use Amazon Linux 2023, or: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 (and persist it in /etc/sysctl.d/)"
  fi
  cpus="$(nproc)"
  mem_gb="$(awk '/MemTotal/ {printf "%d", $2 / 1048576}' /proc/meminfo)"
  if [ "$((cpus * 4))" -lt "$MAX_SESSIONS" ]; then warn "$cpus CPUs may be too few for MAX_SESSIONS=$MAX_SESSIONS (rough guide: 1 CPU per 4 sessions; load-test your pages)"; else ok "$cpus CPUs"; fi
  if [ "$mem_gb" -lt "$((MAX_SESSIONS + 2))" ]; then warn "${mem_gb} GB RAM may be too little for MAX_SESSIONS=$MAX_SESSIONS (rough guide: 1 GB per session + 2 GB)"; else ok "${mem_gb} GB RAM"; fi
  for port in 80 443; do
    if command -v ss >/dev/null 2>&1 && ss -ltnH "sport = :$port" 2>/dev/null | grep -q .; then
      warn "something already listens on port $port (fine if it's this stack's Caddy)"
    fi
  done
  # On EC2: IMDSv1 answers without a token. Require IMDSv2 instead.
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' http://169.254.169.254/latest/meta-data/ 2>/dev/null || true)"
  case "$code" in
    200) warn "EC2 metadata answers without a token (IMDSv1). Require IMDSv2: aws ec2 modify-instance-metadata-options --instance-id <id> --http-tokens required --http-put-response-hop-limit 1" ;;
    401) ok "EC2 metadata requires IMDSv2" ;;
  esac
fi

if [ -n "$DOMAIN" ] && [ "$DOMAIN" != "collab.example.com" ]; then
  resolved="$( (getent ahostsv4 "$DOMAIN" 2>/dev/null || true) | awk 'NR==1 {print $1}')"
  [ -z "$resolved" ] && resolved="$(dig +short A "$DOMAIN" 2>/dev/null | tail -1)"
  if [ -z "$resolved" ]; then warn "$DOMAIN doesn't resolve yet; Caddy can't get a certificate until it does"
  elif [ "$resolved" != "$PUBLIC_IP" ]; then warn "$DOMAIN resolves to $resolved, not PUBLIC_IP=$PUBLIC_IP"
  else ok "$DOMAIN resolves to $PUBLIC_IP"; fi
fi

echo
if [ "$errors" -gt 0 ]; then
  echo "$errors problem(s), $warnings warning(s). Fix the FAIL lines before deploying."
  exit 1
fi
echo "Ready: $warnings warning(s). Deploy with: docker compose up -d --build"
