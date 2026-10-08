# NixOS

The NixOS module installs Fireactions and its host dependencies, builds the guest kernel, and imports matching Ubuntu guest images.
Docker runs inside the Ubuntu guest, not on the NixOS host.
The default base comes from [hostinger/fireactions-images](https://github.com/hostinger/fireactions-images), with Docker, Buildx, Compose, and systemd already installed.
The module adds this fork's agent instead of using the upstream GitHub runner agent.

## Install the module

Use an installed NixOS host with working `/dev/kvm`.
Keep its hardware imports, bootloader, filesystems, and `system.stateVersion` unchanged.
A nested VM needs usable KVM access from its hypervisor.

Import `nixosModules.default` from `github:ALameLlama/fireactions`.
The same module is exported as `nixosModules.fireactions`.
Add this input and module to your existing host flake, or copy the [host flake example](https://github.com/ALameLlama/fireactions/blob/main/examples/nixos/flake.nix) beside `configuration.nix`:

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
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        fireactions.nixosModules.default
        {
          nix.settings.experimental-features = [ "nix-command" "flakes" ];
          services.fireactions = {
            enable = true;
            devmapper.poolName = "containerd-thinpool";
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

Use `aarch64-linux` for an ARM64 host and matching guests.
The module builds packages with your host's `pkgs`.
Use `nixos-unstable`, preferably the revision in this project's `flake.lock`.
The module needs `go_1_26` and Forgejo Runner 13.2 or newer when Runner is enabled.
Keep the generated `flake.lock` with your host configuration.

For local changes, use `url = "path:/absolute/path/to/fireactions";` instead of the GitHub input.
A GitHub input only includes tracked, pushed files.
Enable the `nix-command` and `flakes` experimental features before running flake commands.
For an individual command, pass `--extra-experimental-features 'nix-command flakes'` to `nix`.

The module configures these host resources:

- Firecracker, containerd with the devmapper snapshotter, and LVM thin-pool support.
- CNI plugins, including `tc-redirect-tap`, and a dedicated guest bridge.
- IP forwarding, host firewall forwarding rules, and `systemd-resolved` by default.
- The root daemon, guest archive importer, and an independent reaper timer every 10 seconds.
- An optional unprivileged Forgejo Runner with systemd credentials.

It generates `/etc/fireactions/config.yaml` and links CNI executables under `/opt/cni/bin`.
The daemon uses private state at `/var/lib/fireactions` with mode `0700`.
The socket directory uses mode `0750` and ownership `root:fireactions`.
Runner connects to `/run/fireactions/plugin.sock`.
This remains privileged host execution without a Firecracker jailer.

## Configure storage

Containerd needs an LVM thin pool, which stores writable guest snapshots.
Choose an existing pool or opt into creation on a known empty spare device.
The module does not create storage unless `devmapper.provision.enable` is true.

### Use an existing pool

Set the device-mapper name, not a disk path:

```nix
services.fireactions.devmapper.poolName = "containerd-thinpool";
```

A volume group named `containerd` with a thin pool named `thinpool` normally has this mapper name.
Make sure that `sudo dmsetup table containerd-thinpool` reports a `thin-pool` target.
The module enables LVM activation during boot and waits for the mapper device before starting containerd.
It does not format, resize, or delete an existing pool.
`devmapper.baseImageSize` defaults to `"30GB"`.

### Create a pool on an empty spare device

Do not select the root disk or any device that contains data.
Provisioning writes LVM metadata to the selected device.
An empty signature listing does not prove that a disk contains no data.
First inspect disks, mount points, and LVM volumes on the host:

```bash
findmnt /
lsblk -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINTS,MODEL,SERIAL
sudo pvs
sudo vgs
sudo lvs -a -o vg_name,lv_name,lv_size,segtype,devices
```

Only after you identify a known empty spare device, set its stable `/dev/disk/by-id/` path:

```nix
services.fireactions.devmapper.provision = {
  enable = true;
  device = "/dev/disk/by-id/REPLACE_WITH_VERIFIED_EMPTY_SPARE_DEVICE";
  volumeGroup = "containerd";
  logicalVolume = "thinpool";
  initialSize = 80;
  metadataSize = "1G";
};
```

The module derives `devmapper.poolName` from the volume names.
It refuses partitions, holders, mounted devices, existing signatures, and unrelated LVM storage.
It tags the volume group and thin pool to recognize its own interrupted provisioning.
Later starts activate the same pool without changing its size or deleting snapshots.
If creation stops before the volume group is recorded, the module refuses the orphan physical volume instead of overwriting it.
Inspect the device and recover it manually before retrying.

`initialSize` uses 80 percent of free volume-group extents at creation.
The remaining space allows LVM to extend the pool.
The module enables monitoring and extends the pool by 20 percent when usage reaches 80 percent, while free extents remain.
Monitor both data and metadata usage with `sudo lvs -a`.
Changing creation options does not resize an existing pool.
Disabling provisioning or removing the module does not delete storage.
To reuse that pool without provisioning, set `devmapper.poolName` explicitly.

## Register Runner and store its token

Runner is optional and disabled by default.
If you enable it, first complete [Forgejo registration](installation.md#register-forgejo-runner-on-the-host).
The module does not register Runner or create a token.
Securely transfer the registered token to a private upload location on the NixOS host.

Install that transferred file outside the Nix store and outside any repository:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/fireactions-secrets
sudo install -o root -g root -m 0600 /secure-upload/runner-token \
  /var/lib/fireactions-secrets/runner-token
```

Replace `/secure-upload/runner-token` with your transferred file's location, then remove the upload copy.
Use an absolute string for `runner.tokenFile`, not a Nix path such as `./runner-token`.
Nix paths copy files into the Nix store.
The module rejects credential paths inside that store.
Do not put secrets in `settings`, guest images, or job environment variables.

The root systemd manager reads the token through `LoadCredential`.
Runner reads `/run/credentials/fireactions-runner.service/token` through its generated `token_url`.
Runner uses the `fireactions-runner` user and private state at `/var/lib/fireactions-runner` with mode `0700`.
After replacing the source token, restart `fireactions-runner.service` to load it.

Runner capacity defaults to one concurrent job.
Its labels must select enabled pool names, such as `firecracker:firecracker://ubuntu-24.04`.

## Select guest images and Docker configuration

The default archive uses the pinned full Ubuntu 24.04 image from the upstream image repository.
Docker, Buildx, Compose, systemd, PAM, and sudo remain Ubuntu packages.
The image layer adds the same Fireactions binary as the host, Node.js 24, and the `ci` user with passwordless sudo.
It replaces the upstream agent, removes the GitHub runner, locks passwords, and disables SSH and login services.
No Docker daemon is needed to build the archive on the host.

Docker starts before the guest agent accepts commands.
The `ci` user belongs to the guest's native `docker` group.
This grants root access inside the disposable guest, not access to the host Docker socket.
The image creates `/workspace` and `/opt/hostedtoolcache` for `ci`.
At boot, systemd assigns existing tool-cache entries to `ci` without removing their contents or changing their permissions.

Select the default release or an independent image recipe for a pool:

```nix
services.fireactions = {
  guestImage.ubuntuVersion = "24.04";
  guestImage.docker.settings = {
    "registry-mirrors" = [ "https://registry.example.org" ];
  };
  pools."ubuntu-22.04".guestImage.ubuntuVersion = "22.04";
  pools."ubuntu-20.04".guestImage.ubuntuVersion = "20.04";
  runner.labels = [
    "ubuntu-latest:firecracker://ubuntu-24.04"
    "ubuntu-22:firecracker://ubuntu-22.04"
    "ubuntu-20:firecracker://ubuntu-20.04"
  ];
};
```

The standard pool names and default Runner label follow `guestImage.ubuntuVersion`.
Pools without their own recipe share the default image.
A pool recipe gets a distinct image name by default.
Use `guestImage.name` and `guestImage.tag` to change its reference.
An explicit pool `image` must match the imported archive.

`guestImage.docker.settings` writes nonempty Docker daemon configuration to `/etc/docker/daemon.json`.
These values enter the Nix store and must not contain secrets.
Set `guestImage.docker.enable = false` to disable Docker services and remove the agent's Docker startup dependency.
The base image still contains the installed Docker tools.
To select another Docker version, supply a custom full base through `fromImage`, not a Nix Docker package.
The guest kernel must support the firewall backend used by that base.

The flake exports these packages for `x86_64-linux` and `aarch64-linux`:

```bash
nix build .#guest-image
nix build '.#"guest-image-ubuntu20.04"'
nix build '.#"guest-image-ubuntu22.04"'
nix build '.#"guest-image-ubuntu24.04"'
```

`guest-image` selects Ubuntu 24.04.
The uncompressed archive is `result` and uses `localhost/fireactions-guest:ubuntu-24.04` by default.
The module imports each managed archive once into the configured containerd namespace with devmapper before starting the backend.
Default profiles use `image_pull_policy: Never`.
Disabled pools do not add archives.

## Extend images without changing Fireactions

Keep custom recipes, base pins, and build scripts in your host or image repository.
Add tools and image files through the existing recipe hooks:

```nix
services.fireactions.guestImage = {
  packages = [ pkgs.php ];
  extraCommands = builtins.readFile ./guest-extra.sh;
  fakeRootCommands = ''
    chown -R 1000:1000 opt/ci
  '';
};
```

For example, `guest-extra.sh` beside your host configuration can contain:

```bash
mkdir -p opt/ci
printf '%s\n' 'configured-by-host' > opt/ci/image-label
```

`packages` adds executable tools to `/usr/local/bin` and includes their Nix store dependencies.
`extraCommands` runs after the standard image layer setup.
`fakeRootCommands` runs after the standard ownership and permissions are set.
Both hooks run in the build sandbox, not inside a booted Ubuntu guest.
Use relative image paths such as `etc/ci` or `opt/ci`.
The layer does not expose the base filesystem, so plain `rm` does not remove a lower-layer file.
Use a custom base for changes to installed Ubuntu packages.
Do not run `apt-get` in these hooks or copy secrets into an image.

These options also work under `pools.<name>.guestImage`.
NixOS merges package lists and command strings from separate modules.
Use `lib.mkBefore` or `lib.mkAfter` when command order matters.
An explicit `imageArchive` overrides that recipe.

### Use an external base recipe

The base must match the guest architecture and contain native systemd, Bash, sudo, account files, CA certificates, and your required tools.
If Docker is enabled, the base must contain Docker, its `docker` group, and its enabled native units.
A bare `ubuntu` container does not contain these guest services.
The base must use a merged `/usr` layout, with `/bin` and `/sbin` linked into `/usr`.
Start your custom Dockerfile from the full upstream image, then install Ubuntu packages there.

For example, actions such as `shivammathur/setup-php` need writable Ubuntu PHP files under `/etc/php`.
Install PHP in the base instead of adding Nix PHP through `packages`.
Preinstall each matrix version, its `php-config` binary, extensions, and coverage driver.
If the image includes Composer, set the action's `tools: none` input to reuse it.
Otherwise, the action refreshes Composer on each run.

Generate a pinned base definition from your host flake directory:

```bash
mkdir -p images
nix run --inputs-from . nixpkgs#nix-prefetch-docker -- \
  --image-name ghcr.io/example/project-guest --image-tag ci --arch amd64 \
  --final-image-name ghcr.io/example/project-guest --final-image-tag ci > images/base-image.nix
```

Use `--arch arm64` for an ARM64 base.
Keep the generated definition in your own repository.
Create `images/project-guest.nix` there:

```nix
{ pkgs, guestImage, fireactions }:
guestImage.override {
  inherit fireactions;
  fromImage = pkgs.dockerTools.pullImage (import ./base-image.nix);
  name = "localhost/project-guest";
  tag = "ci";
  extraPackages = [ pkgs.php ];
}
```

The exported image packages accept `fromImage`, `name`, `tag`, `dockerEnabled`, `dockerSettings`, `extraPackages`, `extraCommands`, and `fakeRootCommands` through `.override`.
Use `.override`, not `.overrideAttrs`, to change inputs before the layers are built.
The standard layer setup lives in `nix/guest-image-setup.sh`.

Add the archive to an enabled pool in your host flake:

```nix
({ config, pkgs, ... }: {
  services.fireactions.pools.project = {
    image = "localhost/project-guest:ci";
    imageArchive = import ./images/project-guest.nix {
      inherit pkgs;
      guestImage = fireactions.packages.${pkgs.stdenv.hostPlatform.system}.guest-image;
      fireactions = config.services.fireactions.package;
    };
    prewarmCount = 0;
  };
  services.fireactions.runner.labels = [
    "firecracker:firecracker://ubuntu-24.04"
    "project:firecracker://project"
  ];
})
```

The surrounding `outputs` function must include the `fireactions` input.
Keep any other required Runner labels.
Jobs with `runs-on: project` select this pool.
Passing `config.services.fireactions.package` keeps the guest agent identical to the host.

For a local base archive, use `pkgs.requireFile` with its hash.
Build and export that base first, then import it with `nix-store --add-fixed sha256`.
Keep a garbage collection root for the stored archive.
Changing its Dockerfile does not rebuild a `requireFile` archive.

For a complete externally built guest archive, set `imageArchive` to an absolute runtime filename string.
Rebuild it whenever the host binary changes.
After replacing it, restart `fireactions-image-import.service` to drain Runner and import it.
Runtime files have no automatic watcher.
With global `imageArchive = null`, only archives from enabled pools are managed.
Import other configured images separately into the namespace in `settings.containerd.namespace`.

## Configure pools and backend settings

The standard pool keeps one idle guest with 2 CPUs and 4096 MiB of memory.
The large pool uses 4 CPUs and 8192 MiB with no idle guests.
Budget memory for idle guests, active jobs, replacements, and the host.
Guest memory is allocated on demand, but jobs can consume the full configured amount.

```nix
services.fireactions.pools = {
  "ubuntu-24.04" = { prewarmCount = 1; vcpuCount = 2; memoryMiB = 4096; };
  "ubuntu-24.04-large" = { prewarmCount = 0; vcpuCount = 4; memoryMiB = 8192; };
};
```

Per-pool overrides preserve other profiles and their defaults.
New pools default to zero idle guests, two CPUs, and 4096 MiB.
Set `prewarmCount = 0` to keep a pool available without prewarming.
Set `enable = false` to remove a pool from the generated configuration.
Replica counts do not limit active jobs.
Remove corresponding Runner labels when you disable a pool.

| Field under `services.fireactions.pools.<name>` | Backend YAML field |
| --- | --- |
| Attribute name | `name` |
| `prewarmCount` | `replicas` |
| `vcpuCount` | `firecracker.machine_config.vcpu_count` |
| `memoryMiB` | `firecracker.machine_config.mem_size_mib` |
| `image` | `image` |
| `imagePullPolicy` | `image_pull_policy` |
| `defaultUser` | `default_user` |
| `firecrackerBinary` | `firecracker.binary_path` |
| `kernelImage` | `firecracker.kernel_image_path` |
| `kernelArgs` | `firecracker.kernel_args` |

Use each pool's `settings` for additional backend fields, such as rootfs and network rate limiters.
These values merge recursively and override matching generated fields.
Global `services.fireactions.settings` overrides backend defaults.
An explicit `settings.pools` replaces the entire generated list, so include every complete profile that you need.
These values enter the Nix store and must not contain secrets.
The old top-level `prewarmCount` option is removed. Use the corresponding pool option instead.

The guest bridge defaults to `fireactions-br0` with subnet `192.168.128.0/24`.
Change these through `networking.bridge` and `networking.subnet` under `services.fireactions`.
Choose a subnet that does not overlap host, VPN, or other container networks.
The module supports iptables and nftables host firewalls.
If you disable `systemd-resolved`, set `settings.network.resolver_path` to a usable host resolver file.
Guests must reach Forgejo and the services used by workflows.
`tcRedirectTapPackage` accepts a custom plugin package.

Runner caching is disabled by default.
Set `runner.cache.enable = true` to open the cache proxy on TCP port `8088` only on the guest bridge.
Keep the host firewall enabled because Runner binds the proxy to all interfaces.
Reserve port `8088` for Runner.
Cache files persist in `/var/lib/fireactions-runner/.cache/actcache` across guest deletion and Runner restarts.
Jobs that started without caching need a new run to receive the cache endpoint.

## Build and configure the guest kernel

The module builds upstream Linux from the source and version of `guestKernelPackage`.
The default is `pkgs.linuxKernel.kernels.linux_6_12`, with the patch version selected by the locked Nixpkgs input.
It does not use Amazon patches or downloaded Firecracker CI configurations.
Changing the guest kernel does not change the host kernel.

```nix
services.fireactions = {
  guestKernelPackage = pkgs.linuxKernel.kernels.linux_6_18;
  guestKernelConfig = ''
    CONFIG_NF_TABLES=y
  '';
};
```

The builder starts with the native architecture configuration and enables built-in systemd, cgroup, virtio, filesystem, and Docker networking support.
It uses ACPI for x86_64 virtio discovery and 4 KiB pages on ARM64.
After `olddefconfig`, it rejects missing required built-in options, loadable modules, and Rust support before compilation.
The guest has no kernel modules, and the builder does not supply a Rust toolchain.
The output is an uncompressed `vmlinux` file on x86_64 or an `Image` file on ARM64.

Set `pools.<name>.kernelImage` to select another kernel for one pool.
An explicit `guestKernel` overrides `guestKernelPackage` and `guestKernelConfig`.
The exported `guest-kernel.override` accepts `sourceKernel`, `extraConfig`, `configfile`, and `kernelStdenv`:

```nix
fireactions.packages.${pkgs.stdenv.hostPlatform.system}.guest-kernel.override {
  sourceKernel = pkgs.linuxKernel.kernels.linux_6_12;
  configfile = ./guest-kernel.config;
}
```

A custom `configfile` replaces the generated configuration. `extraConfig` is not appended to it.
Use a complete configuration for the source and architecture.
Keep required drivers built in with `=y`, not `=m`.
Keep `CONFIG_MODULES` and `CONFIG_RUST` disabled, and `CONFIG_ARM64_4K_PAGES=y` on ARM64.
The same required-option checks apply to custom configurations.

Firecracker's [support policy](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/docs/kernel-policy.md) does not list 6.12 or 6.18 as validated guest series.
Test the selected kernel with your Firecracker release before deployment.
The agent also requires `cgroup.kill`, which needs Linux 5.14 or newer, or a suitable backport.
Passing configuration checks alone does not prove that the guest boots.

## Apply changes and upgrade

Before the first switch, prepare the existing pool or select the verified empty spare device.
If Runner is enabled, install its token and replace registration placeholders.
From the host flake directory, run:

```bash
sudo nixos-rebuild switch --flake .#fireactions-host
sudo fireactions validate --host /etc/fireactions/config.yaml
sudo systemctl status fireactions-storage.service containerd.service
sudo systemctl status fireactions.service fireactions-reaper.timer
sudo systemctl status fireactions-runner.service
```

Omit the Runner command if Runner is disabled.
If your configuration name matches the host name, omit `#fireactions-host`.
Change Nix options instead of editing the generated `/etc/fireactions/config.yaml`.
After fixing a startup failure, retry the affected service without deleting snapshots or storage.
See [image import recovery](../help/troubleshooting.md#an-image-import-reports-a-missing-parent-snapshot) for a failed archive import.

To update locked dependencies and rebuild the matching host and guest, run:

```bash
nix flake update
sudo nixos-rebuild switch --flake .#fireactions-host
```

To apply configuration changes without updating dependencies, omit `nix flake update`.
The module allows 30-minute jobs, 35 minutes for graceful Runner shutdown, and a 36-minute systemd stop limit.
Systemd drains Runner before stopping the backend, imports changed archives, then starts the backend and Runner.
A backend restart destroys its guests. Jobs cannot resume afterward.
A rebuild without relevant changes does not restart these services.
Rollback restores the previous system and matching Nix-built archives, but does not undo storage provisioning.

When migrating from the earlier hybrid guest, remove `guestImage.docker.package` overrides and Nix-specific Docker unit or socket hooks.
Use native Ubuntu Docker configuration through `guestImage.docker.settings`, or select its version in a custom full base.
Keep custom PHP bases and other project software, but make sure that each base supplies native guest services.
Remove an explicit `guestKernel` override before selecting `guestKernelPackage` if that override is no longer needed.
Runtime guest archives remain your responsibility and must match the host binary after every upgrade.
