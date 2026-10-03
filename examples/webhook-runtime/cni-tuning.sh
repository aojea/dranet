#!/bin/sh
set -eu

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
cat >/dev/null

: "${DRANET_POD_UID:?}"
: "${DRANET_NETNS:?}"
: "${DRANET_CLAIMS:?}"
plugin=${1:?CNI tuning binary path required}

devices=$(printf '%s' "$DRANET_CLAIMS" |
  jq -c '.[] | .devices[] | select(.data.delegate == "tuning")')
[ -n "$devices" ] || exit 0
printf '%s\n' "$devices" |
  while IFS= read -r device; do
    ifname=$(printf '%s' "$device" | jq -er '.config.interface.name')
    sysctl=$(printf '%s' "$device" | jq -ce '.data.sysctl | objects')
    config=$(jq -cn --arg ifname "$ifname" --arg netns "$DRANET_NETNS" --argjson sysctl "$sysctl" '
      {cniVersion: "1.0.0", name: "dranet-tuning", type: "tuning", sysctl: $sysctl,
       prevResult: {cniVersion: "1.0.0", interfaces: [{name: $ifname, sandbox: $netns}]}}')
    if ! result=$(printf '%s' "$config" |
      CNI_COMMAND=ADD CNI_CONTAINERID="$DRANET_POD_UID" CNI_NETNS="$DRANET_NETNS" \
      CNI_IFNAME="$ifname" CNI_PATH="$(dirname "$plugin")" \
      CNI_ARGS="IgnoreUnknown=1;K8S_POD_NAMESPACE=$DRANET_POD_NAMESPACE;K8S_POD_NAME=$DRANET_POD_NAME" \
      "$plugin"); then
      printf 'CNI tuning failed for %s: %s\n' "$ifname" "$result" >&2
      exit 1
    fi
  done