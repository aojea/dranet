#!/bin/sh
# Test post-configuration hook returned by tests/webhook.py. It runs on the
# node as an OCI createRuntime hook once per pod, before the pod's first
# container, after DRANET attached the pod's devices. It may only change
# network state in the pod's namespace: it records what it received, and how
# many times it ran, in the interface alias, which the e2e reads from the pod.
set -eu

# The container state on stdin is not for us; drain it.
cat >/dev/null

iface=$(printf '%s' "$DRANET_CLAIMS" | jq -r '.[0].devices[0].config.interface.name')
data=$(printf '%s' "$DRANET_CLAIMS" | jq -c '.[0].devices[0].data')
addresses=$(printf '%s' "$DRANET_CLAIMS" | jq -c '.[0].devices[0].config.interface.addresses')
# The devices DRANET attached, as seen from the pod's network namespace.
devices=$(nsenter --net="$DRANET_NETNS" -- ip -br link show type dummy | awk '{print $1}' | sort | tr '\n' ',')

previous=$(nsenter --net="$DRANET_NETNS" -- ip -j link show dev "$iface" | jq -r '.[0].ifalias // ""')
runs=$(printf '%s' "$previous" | sed -n 's/.*runs=\([0-9]*\).*/\1/p')
runs=$((${runs:-0} + 1))

nsenter --net="$DRANET_NETNS" -- ip link set dev "$iface" alias \
  "post-hook runs=$runs pod=$DRANET_POD_NAMESPACE/$DRANET_POD_NAME devices=$devices data=$data addresses=$addresses"

fail_until=$(printf '%s' "$data" | jq -r '.failUntil // 0')
if [ "$fail_until" -eq -1 ] || [ "$runs" -le "$fail_until" ]; then
  printf 'post-configuration failed for %s/%s\n' "$DRANET_POD_NAMESPACE" "$DRANET_POD_NAME" >&2
  exit 1
fi
if [ "$(printf '%s' "$data" | jq -r '.timeout // false')" = true ]; then
  printf 'post-configuration waiting for runtime timeout\n' >&2
  exec sleep 60
fi
