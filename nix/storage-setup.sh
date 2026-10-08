set -euo pipefail

fail() {
  echo "Fireactions storage: $*" >&2
  exit 1
}

has_tag() {
  [[ ",$1," == *,fireactions,* ]]
}

device=$(readlink -f "$device")
[[ -b "$device" ]] || fail "not a block device: $device"

# Tags identify interrupted provisioning without accepting unrelated LVM storage.
if pvs "$device" >/dev/null 2>&1; then
  group=$(pvs --noheadings -o vg_name "$device")
  group=${group//[[:space:]]/}
  [[ "$group" == "$volumeGroup" ]] || fail "physical volume is not managed by Fireactions: $device"
else
  ! vgs "$volumeGroup" >/dev/null 2>&1 || fail "volume group already exists: $volumeGroup"
  mapfile -t devices < <(lsblk --noheadings --raw --paths --output NAME "$device")
  [[ ${#devices[@]} == 1 ]] || fail "device has partitions or holders: $device"
  [[ -z "$(lsblk --noheadings --output MOUNTPOINTS "$device")" ]] || fail "device is mounted: $device"
  [[ -z "$(wipefs --no-act --noheadings --output TYPE "$device")" ]] || fail "device has existing signatures: $device"
  # ponytail: Interrupted vgcreate can leave an orphan PV. Add a provisioning journal if automatic recovery is required.
  vgcreate --addtag fireactions "$volumeGroup" "$device"
fi

IFS='|' read -r count tags < <(vgs --noheadings --separator '|' -o pv_count,vg_tags "$volumeGroup")
count=${count//[[:space:]]/}
tags=${tags//[[:space:]]/}
[[ "$count" == 1 ]] && has_tag "$tags" || fail "volume group does not match the managed device: $volumeGroup"

if lvs "$volumeGroup/$logicalVolume" >/dev/null 2>&1; then
  IFS='|' read -r kind tags < <(lvs --noheadings --separator '|' -o segtype,lv_tags "$volumeGroup/$logicalVolume")
  kind=${kind//[[:space:]]/}
  tags=${tags//[[:space:]]/}
  [[ "$kind" == thin-pool ]] && has_tag "$tags" || fail "logical volume is not a managed thin pool: $volumeGroup/$logicalVolume"
else
  [[ -z "$(lvs --noheadings -o lv_name "$volumeGroup")" ]] || fail "volume group contains other logical volumes: $volumeGroup"
  lvcreate --type thin-pool --name "$logicalVolume" --addtag fireactions \
    --poolmetadatasize "$metadataSize" --extents "$initialSize%FREE" \
    --metadataprofile fireactions --monitor y "$volumeGroup"
fi

# Activation preserves existing snapshots. Rebuilds never resize or remove the pool.
lvchange --activate y --monitor y "$volumeGroup/$logicalVolume"
dmsetup table "$poolName" | grep -qw thin-pool || fail "mapper is not a thin pool: $poolName"
