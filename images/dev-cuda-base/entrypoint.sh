#!/bin/sh
set -e
if [ -n "$FLEXCTL_AUTHORIZED_KEYS" ]; then
  printf '%s\n' "$FLEXCTL_AUTHORIZED_KEYS" > /home/dev/.ssh/authorized_keys
  chmod 600 /home/dev/.ssh/authorized_keys
  chown dev:dev /home/dev/.ssh/authorized_keys
fi
exec /usr/sbin/sshd -D -e
