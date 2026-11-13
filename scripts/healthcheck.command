#!/bin/zsh
set -euo pipefail

project_dir="/Users/akkeup/test"
cd "$project_dir"
set -a
source .env
set +a

curl -fsS --max-time 20 "https://api.telegram.org/bot${BOT_TOKEN}/getMe" >/dev/null
echo "Telegram API: OK"

lks_status=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' https://lks.bmstu.ru/lks-back/api/v1/schedules/current || true)
case "$lks_status" in
	2*|3*|4*) echo "LKS API: reachable (HTTP $lks_status)" ;;
	*) echo "LKS API: unavailable (HTTP $lks_status)" >&2; exit 1 ;;
esac

if launchctl print "gui/$(id -u)/com.bmstu.baumanets" 2>/dev/null | grep -q 'state = running'; then
	printf '%s\n' "Bot process: OK"
else
	printf '%s\n' "Bot process: NOT RUNNING" >&2
	exit 1
fi
