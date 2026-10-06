#!/bin/sh
set -e

if [ "$#" -eq 0 ]; then
  exec /usr/bin/fireactions
fi

if [ "${1#-}" != "$1" ] || [ "$1" = "fireactions" ]; then
  if [ "$1" = "fireactions" ]; then
    shift
  fi
  exec /usr/bin/fireactions "$@"
fi

exec "$@"
