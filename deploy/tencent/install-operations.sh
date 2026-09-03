#!/usr/bin/env bash
set -euo pipefail

backend_dir=/opt/tellyouwhat/backend
deploy_user=$(stat -c '%U' "$backend_dir/.env.production")
test "$deploy_user" != root
test -f "$backend_dir/deploy/tencent/operations.py"
unit_dir=$(mktemp -d)
trap 'rm -rf "$unit_dir"' EXIT

cat > "$unit_dir/tellyouwhat-operation@.service" <<EOF
[Unit]
Description=TellYouWhat backend %i
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=$deploy_user
WorkingDirectory=$backend_dir
ExecStart=/usr/bin/python3 $backend_dir/deploy/tencent/operations.py %i
TimeoutStartSec=20min
UMask=0077
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
EOF

make_timer() {
  local operation="$1" calendar="$2"
  cat > "$unit_dir/tellyouwhat-$operation.timer" <<EOF
[Unit]
Description=Scheduled TellYouWhat $operation

[Timer]
OnCalendar=$calendar
Persistent=true
RandomizedDelaySec=120
Unit=tellyouwhat-operation@$operation.service

[Install]
WantedBy=timers.target
EOF
}
make_timer backup '*-*-* 03:05:00 Asia/Shanghai'
make_timer maintenance '*-*-* 03:25:00 Asia/Shanghai'
make_timer restore 'Sun *-*-* 04:05:00 Asia/Shanghai'
sudo -n systemd-analyze verify "$unit_dir"/*.service "$unit_dir"/*.timer
sudo -n install -m 644 "$unit_dir"/*.service "$unit_dir"/*.timer /etc/systemd/system/
sudo -n systemctl daemon-reload
sudo -n systemctl enable --now tellyouwhat-backup.timer tellyouwhat-maintenance.timer tellyouwhat-restore.timer
systemctl list-timers 'tellyouwhat-*' --no-pager
