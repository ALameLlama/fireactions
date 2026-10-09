#!/usr/bin/env bash
set -Eeuo pipefail

image=${1:?Usage: guest_sudo_test.sh IMAGE}

docker run --rm --network none --user ci --entrypoint /bin/sh "$image" -eu -c '
  test "$(id -u)" = 1000
  test "$(sudo -n id -u)" = 0
  sudo -n sh -c "umask 077; printf root-only > /root/fireactions-sudo-check"
  test ! -r /root/fireactions-sudo-check
  test "$(sudo -n cat /root/fireactions-sudo-check)" = root-only
'

docker run --rm --network none --user 65534:65534 --entrypoint /bin/sh "$image" -eu -c '
  command -v sudo
  test "$(id -u)" = 65534
  if sudo -n true; then
    printf "An unauthorized user gained root access.\n" >&2
    exit 1
  fi
'

docker run --rm --network none --tmpfs /run --entrypoint /bin/sh "$image" -eu -c '
  if test -f /etc/tmpfiles.d/fireactions-sudo.conf; then
    systemd-tmpfiles --create /etc/tmpfiles.d/fireactions-sudo.conf
  fi
  test "$(sudo -n -u ci sudo -n id -u)" = 0
'

printf 'Guest sudo checks passed for %s.\n' "$image"
