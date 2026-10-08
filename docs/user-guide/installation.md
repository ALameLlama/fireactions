# Installation

This guide installs the Fireactions host service on Linux and connects a Forgejo Runner. Choose the NixOS module or the shell installer. The NixOS module configures the host dependencies. The shell installer uses existing host dependencies unless you select optional host setup.

## Requirements

Use a Linux host with root or `sudo` access. The host needs usable `/dev/kvm`, containerd with devmapper configured, a CNI network for Fireactions, the configured Firecracker binary and guest kernel, and the CNI plugins named by that network.

Configure a host resolver file with usable nameservers. The example uses `/run/systemd/resolve/resolv.conf`. Set `network.resolver_path` to the real resolver file on your host. The guest network must reach Forgejo and any package registries or services that workflows use.

The host service needs privileges to use KVM, containerd, CNI, host networking, and guest root filesystems. This is privileged host execution. Fireactions does not add a Firecracker jailer or a stronger host isolation boundary.

The supplied guest profiles use Ubuntu 24.04, even when the host uses NixOS. The `ubuntu-24.04` profile uses 2 CPUs and 4 GiB of memory. The `ubuntu-24.04-large` profile uses 4 CPUs and 8 GiB. Each profile needs a bootable guest image, a readable kernel, and an executable Firecracker binary.

## Register Forgejo Runner on the host

Use Forgejo Runner 13.2 on Linux. Register the Runner on the Forgejo server with the Forgejo CLI. Run the command as the Forgejo service user, and replace the work path, configuration path, owner, and secret path with values for that server:

```bash
install -d -m 0700 /secure/path
umask 077
openssl rand -hex 20 | tr -d '\n' > /secure/path/runner-token
forgejo --work-path /path/to/forgejo-work --config /path/to/app.ini \
  forgejo-cli actions register --name fireactions --scope OWNER \
  --secret-file /secure/path/runner-token
```

Use `OWNER/REPO` for a repository-scoped Runner. The Forgejo command reads the existing 40-character token file and returns a registration UUID. Keep the UUID and token private. Do not print the token. Transfer the token securely to the host that runs Forgejo Runner. For a manually managed Runner, store it under the Runner OS user with mode `0600`. The [NixOS module](#install-on-nixos) uses a root-owned token file and systemd credentials instead.

For the NixOS module, continue with [Install on NixOS](#install-on-nixos) after registration. The module generates the Runner configuration and manages its user. For a manually managed Runner, follow the remaining steps in this section.

Configure Runner with the Forgejo server URL, the registration UUID from Forgejo, and the host socket `unix:///run/fireactions/plugin.sock`. Set the token file path in a `file://` `token_url` field. Use the Fireactions label format from [`examples/forgejo-runner.yaml`](https://github.com/ALameLlama/fireactions/blob/main/examples/forgejo-runner.yaml), for example `firecracker:firecracker://ubuntu-24.04`. The example shows the Runner configuration shape:

```yaml
server:
  connections:
    forgejo:
      url: "https://forgejo.example/"
      uuid: "RUNNER_REGISTRATION_UUID"
      token_url: "file:///home/runner/.config/forgejo-runner/token"
plugins:
  firecracker:
    address: "unix:///run/fireactions/plugin.sock"
```

Replace the example URL and UUID with your registration values. Create the token file as the Runner OS user and restrict access to that user. Do not copy the token into the plugin, guest image, or job environment.

Add the Runner OS user to the configured `socket_group`. The example uses `fireactions`. For the example user, run:

```bash
sudo usermod -aG fireactions runner
```

Log out and back in to apply the new group membership, then restart the service that runs Forgejo Runner. Start Forgejo Runner with the completed host configuration:

```bash
forgejo-runner daemon --config examples/forgejo-runner.yaml
```

The host socket grants access to the Fireactions plugin. Do not expose it to guest jobs.

A label selects a configured Fireactions profile. It does not select an arbitrary registry image. Configure a separate label for each profile that Runner can request.

## Install on NixOS

Use the flake from `github:ALameLlama/fireactions` to import `nixosModules.default`, also exported as `nixosModules.fireactions`. This installs the fork-built daemon, guest kernel, Firecracker, containerd with devmapper, and CNI network plugins. CNI connects each guest to the host network.

The module manages the root daemon and an independent reaper timer that runs every 10 seconds. It generates `/etc/fireactions/config.yaml` and links the CNI executables under `/opt/cni/bin`. It also configures IP forwarding, firewall forwarding rules, and the host resolver through `systemd-resolved`. It does not enable Docker on the host.

Complete the first installation in this order:

1. Keep the existing NixOS hardware and boot configuration.
2. Prepare and activate the existing thin pool, or create one on a known empty spare device.
3. Register Forgejo Runner separately and prepare its token file, if you enable the optional Runner.
4. Build the guest image and place its archive on the host.
5. Add the module configuration and run `nixos-rebuild switch`.

### Keep the machine configuration

Start with an installed NixOS host and working `/dev/kvm`. Keep its `configuration.nix` and `hardware-configuration.nix`. Keep the hardware import, bootloader, filesystems, and `system.stateVersion` unchanged. Replacing an Ubuntu host with NixOS does not change the supplied Ubuntu 24.04 guest image.

Copy [`examples/nixos/flake.nix`](https://github.com/ALameLlama/fireactions/blob/main/examples/nixos/flake.nix) to `/etc/nixos/flake.nix`, beside your existing `configuration.nix`. If you already use a host flake, add the input and module to that flake instead. This example imports your existing machine configuration:

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    fireactions = {
      url = "github:ALameLlama/fireactions";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { nixpkgs, fireactions, ... }: {
    nixosConfigurations.fireactions-host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux"; # Use aarch64-linux for an ARM64 host.
      modules = [
        ./configuration.nix # Keep its hardware-configuration.nix import.
        fireactions.nixosModules.default
        {
          services.fireactions = {
            enable = true;
            devmapper.poolName = "containerd-thinpool";
            imageArchive = "/var/lib/fireactions-images/ubuntu-24.04.tar";
            runner = {
              enable = true;
              url = "https://forgejo.example.org/";
              uuid = "REPLACE_WITH_RUNNER_REGISTRATION_UUID";
              tokenFile = "/var/lib/fireactions-secrets/runner-token";
              capacity = 1;
              labels = [ "firecracker:firecracker://ubuntu-24.04" ];
            };
          };
        }
      ];
    };
  };
}
```

Replace the Forgejo URL and UUID with the registration values. Do not put the token value in the flake. If you manage Runner separately, set `runner.enable = false` and omit its registration fields. The Runner is disabled by default.

Use a root `nixpkgs` input from `nixos-unstable`, preferably the revision recorded in this project's `flake.lock`. Keep `fireactions.inputs.nixpkgs.follows = "nixpkgs"`. The module builds packages with your host's `pkgs`. It needs `go_1_26` and Forgejo Runner 13.2 or newer when Runner is enabled.

For a local checkout, replace the Fireactions input URL with `path:/absolute/path/to/fireactions`. This includes local changes without waiting for a Git push. Before using a Git flake URI, track and commit the module and package files. Push those changes before consumers use the GitHub URL.

Enable Nix's `nix-command` and `flakes` experimental features on the installation or build machine. For persistent support, set `nix.settings.experimental-features = [ "nix-command" "flakes" ];` in your existing host configuration. For individual Nix commands before that change takes effect, use `nix --extra-experimental-features 'nix-command flakes' ...`. The rebuild command below passes these features explicitly. Keep the generated host `flake.lock` with your host configuration.

### Prepare the thin pool

A thin pool is LVM storage for containerd snapshots. `devmapper.poolName` must name an existing, activated device-mapper pool, not a disk path. A volume group named `containerd` with a logical volume named `thinpool` normally maps to `containerd-thinpool`.

Do not run pool creation commands on the root disk or any device that contains data. These commands overwrite storage metadata. The module never creates, formats, deletes, or resizes storage. Use separate manual provisioning, or your installer-stage disko configuration, to create storage before the first switch.

If you already have a suitable pool, skip creation. Make sure that its mapper table has type `thin-pool`. If LVM tools are not installed yet, open a temporary shell on the NixOS host:

```bash
nix shell nixpkgs#lvm2 --command bash
```

Run the storage commands inside that shell. First inspect the host's disks, mount points, and existing LVM volumes:

```bash
findmnt /
lsblk -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINTS,MODEL,SERIAL
sudo pvs
sudo vgs
sudo lvs -a -o vg_name,lv_name,lv_size,segtype,devices
```

Only after you identify a spare device that is known to be empty, replace the placeholder below. Use its stable `/dev/disk/by-id/` path. Make sure that it is not the root disk, a mounted device, or part of an existing volume group:

```bash
device=/dev/disk/by-id/REPLACE_WITH_VERIFIED_EMPTY_SPARE_DEVICE
lsblk -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINTS "$device"
sudo wipefs --no-act "$device"
```

Do not continue if the device or its contents are uncertain. An empty signature listing alone does not prove that a disk is safe. Only for the selected, known empty spare device, create the example pool once:

```bash
sudo pvcreate "$device"
sudo vgcreate containerd "$device"
sudo lvcreate --type thin-pool --name thinpool --extents 80%FREE containerd
sudo lvchange --activate y containerd/thinpool
sudo dmsetup info containerd-thinpool
sudo dmsetup table containerd-thinpool
```

Make sure that the last command reports a `thin-pool` target. Use that mapper name in `devmapper.poolName`. Do not rerun creation commands for an existing pool. The example reserves free volume-group space for later storage maintenance. Monitor pool data and metadata usage with `sudo lvs -a`.

The module enables NixOS LVM support and thin-pool activation during boot. It loads `dm_thin_pool` and keeps the existing pool separate from Fireactions configuration changes. After a reboot, make sure that the pool is active before expecting guests to start. `devmapper.baseImageSize` defaults to `"30GB"` for each base image.

### Prepare the Runner token

If you enable Runner, first complete [Forgejo registration](#register-forgejo-runner-on-the-host) on the Forgejo server. The module does not register a Runner or create its token. Securely transfer the registered token to a private upload location on the NixOS host.

On the NixOS host, install that transferred file outside the Nix store and outside any repository:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/fireactions-secrets
sudo install -o root -g root -m 0600 /secure-upload/runner-token \
  /var/lib/fireactions-secrets/runner-token
```

Replace `/secure-upload/runner-token` with the transferred file's private location. Remove the upload copy after installation. Keep the installed file root-owned with mode `0600`. Use an absolute quoted string for `runner.tokenFile`, never a Nix path such as `./runner-token`. Nix paths copy files into the Nix store.

The root systemd manager reads the file through `LoadCredential`. Runner reads `/run/credentials/fireactions-runner.service/token` through its generated `token_url`. Runner runs as `fireactions-runner`, with private state at `/var/lib/fireactions-runner` and mode `0700`. It does not need direct access to the source token file.

After replacing the source token, restart `fireactions-runner.service` to load the new credential. Do not put tokens in `services.fireactions.settings`, a guest image, or job environment variables.

### Build and place the guest archive

Build the image on a machine with Nix and Docker. Docker is required on that build machine only, not on the NixOS runtime host. Use the same architecture for the host, guest kernel, guest image, and static agent binary.

From the Fireactions repository root on an x86_64 build machine, run:

```bash
nix build path:/absolute/path/to/fireactions#fireactions --out-link result
cp result/bin/fireactions ./fireactions
docker build --platform linux/amd64 -f images/ubuntu-24.04/Dockerfile \
  -t localhost/fireactions-guest:ubuntu-24.04 .
docker save -o fireactions-guest.tar localhost/fireactions-guest:ubuntu-24.04
```

Replace `/absolute/path/to/fireactions` with this checkout's path. The package supplies the static binary that the Docker build copies into the guest. See [Images](images.md) for the guest contract. For a native ARM64 build machine, use Docker's `--platform linux/arm64` and an `aarch64-linux` host configuration.

The flake exports Linux packages for `x86_64-linux` and `aarch64-linux`: `default`, `fireactions`, `tc-redirect-tap`, and `guest-kernel`. ARM64 host configurations were evaluated, but live ARM64 guest jobs were not tested. Privileged LVM activation and a live Forgejo job were not exercised by the package smoke checks.

If you build elsewhere, upload the archive to the host over SSH:

```bash
scp fireactions-guest.tar operator@nixos-host:fireactions-guest.tar
```

On the NixOS host, place the archive at the configured runtime path:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/fireactions-images
sudo install -o root -g root -m 0600 ./fireactions-guest.tar \
  /var/lib/fireactions-images/ubuntu-24.04.tar
```

Use an absolute quoted string for `imageArchive`, not a Nix path. The module imports that archive with devmapper before daemon startup and after containerd restarts. Replacing the archive triggers another import. The archive stays outside the Nix store.

Default profiles reference `localhost/fireactions-guest:ubuntu-24.04` with `image_pull_policy: Never`, in the `fireactions` containerd namespace. `Never` requires a local image. If you change the namespace or image reference, import matching images there.

If you set `imageArchive = null`, import images manually after containerd starts:

```bash
sudo ctr --namespace fireactions images import --local --snapshotter devmapper \
  /var/lib/fireactions-images/ubuntu-24.04.tar
sudo systemctl restart fireactions.service
```

### Switch and inspect the services

Before the first switch, make sure that the pool is active, the guest archive exists, and the optional Runner token is installed. Replace the registration placeholders in your host flake. Then apply it:

```bash
sudo nixos-rebuild switch --flake path:/etc/nixos#fireactions-host \
  --option experimental-features 'nix-command flakes'
sudo fireactions validate --host /etc/fireactions/config.yaml
sudo systemctl status fireactions.service fireactions-reaper.timer
sudo systemctl status fireactions-runner.service
```

Omit the last command if Runner is disabled. The generated `/etc/fireactions/config.yaml` is a symlink to a file in the Nix store. Change `services.fireactions.settings` and rebuild instead of editing that file. After fixing a missing pool or image that prevented startup, run `sudo systemctl restart fireactions.service` to retry.

The default `ubuntu-24.04` pool keeps one clean idle VM with 2 CPUs and 4 GiB of memory. The large profile uses 4 CPUs and 8 GiB, with zero idle replicas. Budget memory for the idle VM, active jobs, replacement idle capacity, and the host. Runner capacity defaults to one concurrent job.

The module gives jobs a 30-minute timeout and allows 35 minutes for graceful Runner shutdown. The systemd stop limit is 36 minutes. Stopping Runner stops new polling and lets active jobs finish before the shutdown limit. For maintenance, wait for no claimed guests, stop Runner, and check again before stopping Fireactions.

Attribute overrides in `services.fireactions.settings` merge with the generated defaults. Setting `settings.pools` replaces the entire default pool list. Include each complete profile you need, with its image, pull policy, user, Firecracker path, kernel, and machine size. Keep `runner.labels` aligned with the profile names. `guestKernel` accepts a kernel-file derivation override, and `tcRedirectTapPackage` accepts a plugin package override.

The CNI bridge defaults to `fireactions-br0` with subnet `192.168.128.0/24`. Use `networking.bridge` and `networking.subnet` under `services.fireactions` to change them. Choose a subnet that does not overlap host, VPN, or other container networks. The module supports the host's iptables or nftables firewall. Guests must reach Forgejo and the services used by workflows.

The default resolver file is `/run/systemd/resolve/resolv.conf`. If you disable `systemd-resolved`, set `settings.network.resolver_path` to a usable host resolver file. The root daemon uses private state at `/var/lib/fireactions` with mode `0700`. Its socket directory, `/run/fireactions`, uses mode `0750` and ownership `root:fireactions` by default. The socket is `/run/fireactions/plugin.sock`. This module does not add a Firecracker jailer or change the privileged execution boundary.

## Prepare the configuration and image for the shell installer

Start from [`examples/fireactions.yaml`](https://github.com/ALameLlama/fireactions/blob/main/examples/fireactions.yaml). Set the containerd socket, namespace, Firecracker path, kernel path, and host resolver path to values that exist on your host. Use an image name that you import into the same containerd namespace. The example namespace is `fireactions`.

Build and export the guest image as a Docker image archive. Import that archive with the devmapper snapshotter into the configured namespace. The example commands use the `fireactions` namespace:

```bash
sudo ctr --namespace fireactions images import --local --snapshotter devmapper fireactions-guest.tar
sudo ctr --namespace fireactions images list
```

The image name in the configuration must match the imported image reference. See [Images](images.md) for the guest image contract and build/export/import flow.

## Install Fireactions with the shell installer

Build the static Linux amd64 `fireactions` binary from the repository root as described in [Images](images.md). Then use that fork-built binary. The default installer preflights the host and installs Fireactions without replacing the existing containerd service or configuration, LVM, or CNI configuration.

```bash
sudo ./install.sh --binary ./fireactions --config examples/fireactions.yaml
```

The installer reads `socket_group` through the binary's validated YAML parser. It creates that group if it is missing. It also keeps the `fireactions` group for ownership of `/etc/fireactions/config.yaml`. If group creation fails, the installer stops before it installs the binary, configuration, or systemd units.

The installer installs the Fireactions binary and configuration, creates systemd units, writes the documented IP-forwarding sysctl setting, and reloads systemd. It enables the main service and independent reaper timer. It does not download an upstream Fireactions binary.

If any installation destination already exists, the installer stops before optional host or storage setup. Use the [upgrade guide](upgrade-guide.md) to replace an existing installation. Both host services require containerd and start after it.

Check host prerequisites with the same binary and configuration. Host validation needs root access to inspect the configured host resources:

```bash
sudo ./fireactions validate --host examples/fireactions.yaml
```

The regular `validate CONFIG` command checks configuration without the host preflight.

Apply the IP-forwarding sysctl configuration and reload systemd after installation:

```bash
sudo sysctl --system
sudo systemctl daemon-reload
sudo systemctl enable --now fireactions.service fireactions-reaper.timer
```

The installer enables the main service and independent reaper timer. Keep `fireactions-reaper.timer` enabled when the main service stops or crashes. The timer performs independent cleanup of expired or abandoned VM resources.

## Run the host daemon in a container

The top-level `Dockerfile` builds the host daemon image, not a guest image. Build the Linux `fireactions` binary for the host architecture at the repository root first. The host build context requires `linux/amd64/fireactions` or `linux/arm64/fireactions`. Keep the root `fireactions` binary for guest image builds. The image creates `SOCKET_GROUP=fireactions` with numeric `SOCKET_GID=1000` by default.

The container and host must use the same numeric group ID for the shared socket directory. The group name in the image must match `socket_group` in your configuration. Read that name with the binary, then read its numeric ID from the host group database.

The commands below use an amd64 binary. For an ARM64 host, use an arm64 binary and replace both `linux/amd64` values with `linux/arm64`:

```bash
socket_group=$(./fireactions validate --print-socket-group examples/fireactions.yaml)
socket_gid=$(getent group "$socket_group" | cut -d: -f3)
test -n "$socket_gid" || { echo "Create the configured socket group on the host first" >&2; exit 1; }
install -D -m0755 ./fireactions linux/amd64/fireactions
docker build --platform linux/amd64 --build-arg SOCKET_GROUP="$socket_group" \
  --build-arg SOCKET_GID="$socket_gid" -t fireactions-host .
docker run --rm --entrypoint getent fireactions-host group "$socket_group"
```

Do not assume that the default GID matches your host. The build fails if the selected group name or GID is already in use in the base image.

Run this image as root with host networking, the host PID namespace, and host privileges, for example `--privileged`. Share `/dev`, `/run/containerd/containerd.sock`, `/etc/cni/net.d`, `/opt/cni/bin`, and `/var/run/netns` with an `rshared` mount. Also share the configured IPAM data directory, Firecracker executable, kernel directory, state directory, and socket directory. Mount the configuration at the path passed to `fireactions server --config`.

The daemon and independent reaper must share these paths and namespaces. Keep the host Runner in the configured socket group. These mounts and privileges do not add jailer isolation.

## Optional host setup

The installer keeps host provisioning behind the explicit `--setup-host` option. Use it only when you intend to install or configure missing host dependencies. It can change host packages and services.

Run `install.sh` from the repository tree. It uses the service and timer files under `packaging/systemd/`. Optional setup downloads missing Firecracker `1.17.0`, CNI plugins, and the pinned guest kernel. It checks pinned SHA-256 hashes before extraction or installation. It does not build or import the guest image.

Storage setup can format and destroy data. Do not pass storage options unless you have selected the correct dedicated device and approved the destructive operation interactively. Storage setup requires both `--containerd-snapshotter-device` and `--format-device`. Never run those storage options on an existing host whose data must remain intact.

After explicit storage setup creates the new pool, the installer restarts Containerd to load its new configuration before host validation. Without storage options, `--setup-host` enables and starts Containerd but does not restart a running daemon.

## Verify the service

Check the main service and independent reaper timer:

```bash
sudo systemctl status fireactions.service
sudo systemctl status fireactions-reaper.timer
fireactions pools list
```

See [First build](first-build.md) to run a workflow after Forgejo Runner connects.
