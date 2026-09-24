#!/bin/sh
# Fixed installation for the user-authorized Nasdir live trial.
set -eu
[ "${ANANAS_LEGACY_PROFILE_CONFIGURED:-}" = yes ] || { echo 'Historical template: configure and review targets before explicit opt-in.' >&2; exit 2; }
ANANAS_BASE=/share/EXAMPLE_VOLUME/.ananas-service
ANANAS_PID=/var/run/ananas-nasdir.pid
case "${1:-}" in
  start)
    # Rules only affect the dedicated helper TCP port. Retain them on stop.
    # PC and NAS addresses may change through DHCP: the subnet is allowed, the
    # launcher follows eth0's current address and clients are pinned by TLS.
    if ! /sbin/iptables -nL ANANAS_OUT >/dev/null 2>&1; then
      /sbin/iptables -N ANANAS_OUT
      /sbin/iptables -A ANANAS_OUT -d 10.23.42.0/24 -o eth0 -j ACCEPT
      /sbin/iptables -A ANANAS_OUT -j DROP
      /sbin/iptables -I OUTPUT 1 -p tcp --sport 8742 -j ANANAS_OUT
    fi
    if ! /sbin/iptables -nL ANANAS_IN >/dev/null 2>&1; then
      /sbin/iptables -N ANANAS_IN
      /sbin/iptables -A ANANAS_IN -s 10.23.42.0/24 -i eth0 -j ACCEPT
      /sbin/iptables -A ANANAS_IN -j DROP
      /sbin/iptables -I INPUT 1 -p tcp --dport 8742 -j ANANAS_IN
    fi
    /bin/busybox start-stop-daemon -S -b -m -p "$ANANAS_PID" -x "$ANANAS_BASE/launcher" -- \
      -helper "$ANANAS_BASE/helper" -config "$ANANAS_BASE/helper.json" \
      -listen :8742 -interface eth0 -prefix 10.23.42.0/24 \
      -peer 10.23.42.0/24 -uid 1000 -gid 100
    ;;
  stop)
    /bin/busybox start-stop-daemon -K -p "$ANANAS_PID" -x "$ANANAS_BASE/launcher" -s TERM
    ;;
  status)
    /bin/busybox start-stop-daemon -K -t -p "$ANANAS_PID" -x "$ANANAS_BASE/launcher"
    ;;
  *) echo "Usage: $0 start|stop|status" >&2; exit 2;;
esac
