#!/usr/bin/env bash
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

BINARY=""
CONFIG=""
SETUP_HOST=0
SNAPSHOTTER_DEVICE=""
FORMAT_DEVICE=0

usage() {
  cat <<'EOF'
Install Fireactions using a fork-built binary on an already prepared Linux host.

Usage:
  sudo ./install.sh --setup-host --binary ./fireactions --config examples/fireactions.yaml

Options:
  --binary PATH                         Fork-built Fireactions executable (required)
  --config PATH                         Fireactions configuration (required)
  --setup-host                          Explicitly prepare missing host dependencies
  --containerd-snapshotter-device PATH  Device for a new Containerd devmapper pool
  --format-device                       Permit formatting the selected device (destructive)
  -h, --help                            Show this help

The default install only installs Fireactions files and units. It never installs
or replaces Containerd, CNI, Firecracker, a kernel, or storage. Host preparation
may download host dependencies. Formatting the selected storage device destroys
all data on that device and requires an interactive confirmation.
EOF
}

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need_value() { (($# >= 2)) && [[ -n "$2" ]] || fail "missing value for $1"; }

download_checked() {
  local url=$1 checksum=$2 destination=$3
  curl -fsSL "$url" -o "$destination" || return
  printf '%s  %s\n' "$checksum" "$destination" | sha256sum -c -
}

install_host_setup() {
  [[ "$(uname -s)" == Linux ]] || fail "host setup is supported only on Linux"
  [[ -e /dev/kvm ]] || fail "KVM is unavailable (/dev/kvm is missing)"
  if ! command -v containerd >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
      apt-get update -qq
      apt-get install -y containerd lvm2 curl tar
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y containerd lvm2 curl tar
    elif command -v yum >/dev/null 2>&1; then
      yum install -y containerd lvm2 curl tar
    else
      fail "no supported package manager found; prepare Containerd manually (NixOS hosts need no package manager for default install)"
    fi
  fi
  command -v curl >/dev/null 2>&1 || fail "curl is required by --setup-host"
  command -v tar >/dev/null 2>&1 || fail "tar is required by --setup-host"
  if [[ -n "$SNAPSHOTTER_DEVICE" ]] && ! command -v pvcreate >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
      apt-get install -y lvm2
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y lvm2
    elif command -v yum >/dev/null 2>&1; then
      yum install -y lvm2
    else
      fail "lvm2 (pvcreate) is required for explicit storage setup"
    fi
  fi
  command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required by --setup-host"
  local arch fc_arch fc_sha cni_sha tap_sha kernel_sha
  case "$(uname -m)" in
    x86_64)
      arch=amd64; fc_arch=x86_64
      fc_sha=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
      cni_sha=682b49ff8933a997a52107161f1745f8312364b4c7f605ccdf7a77499130d89d
      tap_sha=acf91949dcd23f2986ed0b36de04afd3e64293c44a2990558be4425b67c7b2ff
      kernel_sha=27a8310b9a727517e9eb02044524b6ceb77de5728e3491b6974d5c846227ecc8
      ;;
    aarch64)
      arch=arm64; fc_arch=aarch64
      fc_sha=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
      cni_sha=db09ab057ecf60b05ba05cbec38d55b95cc139c7f1078e2e4857cc13af158cee
      tap_sha=0c97adc85646354c2cf86bc1ef5d96e56b50402d83c937fdad1f0a2b6360af3f
      kernel_sha=cb1291c66bca75bc11cb9c8357fcef9965bb1786dffcb42a60923c3e0e49f319
      ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
  local temp
  temp=$(mktemp -d)
  trap 'rm -rf "$temp"; trap - RETURN' RETURN
  if ! command -v firecracker >/dev/null 2>&1; then
    [[ ! -e /usr/local/bin/firecracker && ! -L /usr/local/bin/firecracker ]] || fail "/usr/local/bin/firecracker exists but is not usable; refusing to replace it"
    download_checked "https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-${fc_arch}.tgz" "$fc_sha" "$temp/firecracker.tgz"
    tar -xzf "$temp/firecracker.tgz" -C "$temp" --strip-components=1
    install -D -m 0755 "$temp/firecracker-v1.17.0-${fc_arch}" /usr/local/bin/firecracker
  fi
  if [[ ! -x /opt/cni/bin/bridge ]]; then
    if [[ -d /opt/cni/bin ]] && find /opt/cni/bin -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
      fail "/opt/cni/bin contains existing plugins; refusing to replace them while setting up missing CNI plugins"
    fi
    mkdir -p /opt/cni/bin
    download_checked "https://github.com/containernetworking/plugins/releases/download/v1.6.0/cni-plugins-linux-${arch}-v1.6.0.tgz" "$cni_sha" "$temp/cni.tgz"
    tar -xzf "$temp/cni.tgz" -C /opt/cni/bin
  fi
  if [[ ! -x /opt/cni/bin/tc-redirect-tap ]]; then
    [[ ! -e /opt/cni/bin/tc-redirect-tap && ! -L /opt/cni/bin/tc-redirect-tap ]] || fail "CNI plugin /opt/cni/bin/tc-redirect-tap exists but is not executable; refusing to replace it"
    download_checked "https://github.com/hostinger/tc-redirect-tap/releases/download/v0.0.1/tc-redirect-tap-${arch}" "$tap_sha" "$temp/tc-redirect-tap"
    install -m 0755 "$temp/tc-redirect-tap" /opt/cni/bin/tc-redirect-tap
  fi
  local cni_file has_fireactions_cni=0
  if [[ -d /etc/cni/net.d ]]; then
    while IFS= read -r -d '' cni_file; do
      if grep -Eq '"name"[[:space:]]*:[[:space:]]*"fireactions"' "$cni_file"; then
        has_fireactions_cni=1
        break
      fi
    done < <(find /etc/cni/net.d -maxdepth 1 -type f \( -name '*.conf' -o -name '*.conflist' -o -name '*.json' \) -print0)
  fi
  if [[ "$has_fireactions_cni" == 0 ]]; then
    [[ ! -e /etc/cni/net.d/10-fireactions.conflist && ! -L /etc/cni/net.d/10-fireactions.conflist ]] || fail "10-fireactions.conflist exists with another network; refusing to replace it"
    install -d -m 0755 /etc/cni/net.d
    cat > /etc/cni/net.d/10-fireactions.conflist <<'EOF'
{"cniVersion":"1.0.0","name":"fireactions","plugins":[{"type":"bridge","bridge":"fireactions-br0","isDefaultGateway":true,"ipMasq":true,"hairpinMode":true,"ipam":{"type":"host-local","subnet":"192.168.128.0/24","resolvConf":"/etc/resolv.conf"}},{"type":"firewall"},{"type":"tc-redirect-tap"}]}
EOF
  fi
  if [[ ! -e /var/lib/fireactions/kernels/6.1/vmlinux && ! -L /var/lib/fireactions/kernels/6.1/vmlinux ]]; then
    install -d -m 0755 /var/lib/fireactions/kernels/6.1
    download_checked "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/${fc_arch}/vmlinux-6.1.128" "$kernel_sha" "$temp/vmlinux"
    install -m 0644 "$temp/vmlinux" /var/lib/fireactions/kernels/6.1/vmlinux
  fi
  if [[ -n "$SNAPSHOTTER_DEVICE" ]]; then
    pvcreate -f "$SNAPSHOTTER_DEVICE"
    vgcreate containerd "$SNAPSHOTTER_DEVICE"
    lvcreate --type thin-pool -n thinpool --poolmetadatasize 1G -l 95%VG containerd
    systemctl enable containerd
    systemctl restart containerd
  else
    systemctl enable --now containerd
  fi
}

main() {
  while (($#)); do
    case "$1" in
      --binary) need_value "$@"; BINARY=$2; shift 2 ;;
      --config) need_value "$@"; CONFIG=$2; shift 2 ;;
      --setup-host) SETUP_HOST=1; shift ;;
      --containerd-snapshotter-device) need_value "$@"; SNAPSHOTTER_DEVICE=$2; shift 2 ;;
      --format-device) FORMAT_DEVICE=1; shift ;;
      -h|--help) usage; return 0 ;;
      *) fail "unknown option: $1" ;;
    esac
  done
  [[ "$(id -u)" == 0 ]] || fail "run as root (for example, with sudo)"
  if [[ "$SETUP_HOST" != 1 && ( -n "$SNAPSHOTTER_DEVICE" || "$FORMAT_DEVICE" == 1 ) ]]; then
    fail "storage setup options require --setup-host"
  fi
  [[ -n "$BINARY" && -n "$CONFIG" ]] || fail "--binary and --config are required"
  [[ -f "$BINARY" && -x "$BINARY" ]] || fail "binary must be an executable file: $BINARY"
  [[ -f "$CONFIG" && -r "$CONFIG" ]] || fail "config must be a readable file: $CONFIG"
  [[ "$(uname -s)" == Linux ]] || fail "installation is supported only on Linux"
  local socket_group
  socket_group=$("$BINARY" validate --print-socket-group "$CONFIG") || fail "configuration validation failed"
  local binary_dest=/usr/local/bin/fireactions config_dest=/etc/fireactions/config.yaml
  local unit_dir=/etc/systemd/system
  [[ ! -e "$binary_dest" && ! -L "$binary_dest" ]] || fail "$binary_dest already exists; refusing to replace it"
  [[ ! -e "$config_dest" && ! -L "$config_dest" ]] || fail "$config_dest already exists; refusing to replace it"
  [[ ! -e "$unit_dir/fireactions.service" && ! -L "$unit_dir/fireactions.service" ]] || fail "fireactions.service already exists; refusing to replace it"
  [[ ! -e "$unit_dir/fireactions-reaper.service" && ! -L "$unit_dir/fireactions-reaper.service" && ! -e "$unit_dir/fireactions-reaper.timer" && ! -L "$unit_dir/fireactions-reaper.timer" ]] || fail "Fireactions reaper unit already exists; refusing to replace it"
  if [[ -n "$SNAPSHOTTER_DEVICE" ]]; then
    [[ "$SETUP_HOST" == 1 ]] || fail "storage setup options require --setup-host"
    [[ "$FORMAT_DEVICE" == 1 ]] || fail "storage initialization requires --format-device"
    [[ -b "$SNAPSHOTTER_DEVICE" ]] || fail "snapshotter device is not a block device: $SNAPSHOTTER_DEVICE"
    [[ ! -e /etc/containerd/config.toml && ! -L /etc/containerd/config.toml ]] || fail "refusing to format a device when an existing Containerd config cannot safely be updated"
    [[ -t 0 ]] || fail "destructive storage setup requires an interactive terminal"
    printf 'DESTRUCTIVE: all data on %s will be erased. Type ERASE %s to continue: ' "$SNAPSHOTTER_DEVICE" "$SNAPSHOTTER_DEVICE" >&2
    local approval
    read -r approval
    [[ "$approval" == "ERASE $SNAPSHOTTER_DEVICE" ]] || fail "storage setup was not approved"
    install -d -m 0755 /etc/containerd
    cat > /etc/containerd/config.toml <<'EOF'
version = 2

[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "containerd-thinpool"
  root_path = "/var/lib/containerd/devmapper"
  base_image_size = "30GB"
  discard_blocks = true
EOF
  elif [[ "$FORMAT_DEVICE" == 1 ]]; then
    fail "--format-device requires --containerd-snapshotter-device"
  elif [[ "$SETUP_HOST" == 1 && ! -e /etc/containerd/config.toml ]]; then
    fail "--setup-host needs an existing configured Containerd host, or an explicit snapshotter device plus --format-device"
  fi
  if [[ "$SETUP_HOST" == 1 ]]; then install_host_setup; fi
  "$BINARY" validate --host "$CONFIG"

  getent group "$socket_group" >/dev/null || groupadd --system "$socket_group" || fail "cannot create configured socket group: $socket_group"
  if [[ "$socket_group" != fireactions ]]; then
    getent group fireactions >/dev/null || groupadd --system fireactions || fail "cannot create configuration ownership group: fireactions"
  fi
  install -D -m 0755 "$BINARY" "$binary_dest"
  install -D -m 0640 -o root -g fireactions "$CONFIG" "$config_dest"
  if [[ ! -e /etc/sysctl.d/99-fireactions.conf && ! -L /etc/sysctl.d/99-fireactions.conf ]]; then
    install -m 0644 /dev/null /etc/sysctl.d/99-fireactions.conf
    cat > /etc/sysctl.d/99-fireactions.conf <<'EOF'
net.ipv4.conf.all.forwarding=1
net.ipv4.ip_forward=1
EOF
  fi
  install -m 0644 "$SCRIPT_DIR/packaging/systemd/fireactions.service" "$unit_dir/fireactions.service"
  install -m 0644 "$SCRIPT_DIR/packaging/systemd/fireactions-reaper.service" "$unit_dir/fireactions-reaper.service"
  install -m 0644 "$SCRIPT_DIR/packaging/systemd/fireactions-reaper.timer" "$unit_dir/fireactions-reaper.timer"
  sysctl -p /etc/sysctl.d/99-fireactions.conf >/dev/null
  systemctl daemon-reload
  systemctl enable fireactions.service
  systemctl enable --now fireactions-reaper.timer
  systemctl start fireactions.service
  printf 'Fireactions installed from %s.\n' "$BINARY"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
