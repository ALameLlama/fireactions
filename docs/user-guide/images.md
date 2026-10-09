# Guest images

A Fireactions guest image is a Linux root filesystem in a container image. Fireactions uses containerd to create a root filesystem for a configured Firecracker profile. The profile must name an image that is imported into the same containerd namespace configured for Fireactions.

## Guest image requirements

The guest image must include the Fireactions binary and a systemd unit for the guest agent. The supplied Ubuntu images create the `ci` user and `/workspace` directory. The unit starts the agent as root so that the agent can set up the job user and workspace. Job commands run as the configured default user, `ci`.

The supplied Ubuntu guest images let `ci` run all commands as root with passwordless `sudo`, including `sudo -n`.
This grants workflow code root access inside the guest.
Installing packages on the host does not add them to the guest image.
Rebuild and import the updated archive before starting a new job.

The unit enables delegated cgroup v2 control for the guest agent and its CI processes. The guest kernel command line must enable the unified cgroup hierarchy. See the supplied [`fireactions-agent.service`](https://github.com/ALameLlama/fireactions/blob/main/images/ubuntu-24.04/fireactions-agent.service) and [kernel guide](kernels.md).

Fireactions does not support SSH login or a default guest password. Do not add either as a way to debug a guest. Fireactions does not provide stdin, PTY, or signal RPC access.

## Nix-built guest image

On NixOS, the module builds and imports a matching Ubuntu 24.04 archive by default.
Run `nixos-rebuild switch` to apply configuration changes.
Run `nix flake update` before the rebuild to update the locked packages.
Nix builds the host and guest from the same Fireactions package.
The image uses a pinned Ubuntu base and Nixpkgs tools, including systemd, Bash, Git, Node.js, sudo, and CA certificates.
Docker is not required.

To build only the archive from this checkout, run:

```bash
nix build .#guest-image
```

The archive is `result`. It uses the existing `localhost/fireactions-guest:ubuntu-24.04` image reference.
Use the Docker procedure below for non-Nix hosts or custom Dockerfile images.

### Extend the image from NixOS

Configure the default guest image under `services.fireactions.guestImage`.
The module keeps the standard tools, user, and matching agent.
For example, add PHP and a file created by your own build script:

```nix
services.fireactions.guestImage = {
  packages = [ pkgs.php ];
  extraCommands = builtins.readFile ./guest-extra.sh;
  fakeRootCommands = ''
    chown -R 1000:1000 opt/ci
  '';
};
```

Use the `pkgs` argument in your host configuration module.
Create `guest-extra.sh` beside that configuration file:

```bash
mkdir -p opt/ci
printf '%s\n' 'configured-by-host' > opt/ci/image-label
```

`packages` adds executable tools to `/usr/bin` and includes their Nix store dependencies.
`extraCommands` runs after the default root filesystem setup.
`fakeRootCommands` runs after the default ownership and permissions are set.
Use the latter hook to set image ownership without root access on the build host.

Both hooks run in the build sandbox with the image root as their working directory.
Use relative image paths such as `etc/ci` or `opt/ci`.
These commands do not run inside a booted guest or a Docker container.
Do not use `apt-get` here. Add Nix packages or use the Dockerfile procedure instead.
Do not copy Runner credentials or other secrets into the image.

NixOS merges package lists and command strings contributed by separate modules.
Use `lib.mkBefore` or `lib.mkAfter` when one command depends on another module's command.
Apply changes with `nixos-rebuild switch`.
The existing lifecycle drains Runner, imports the archive, and starts fresh guests.
These options affect the default archive only. An explicit `imageArchive` override takes precedence.

### Extend the image from a flake

The exported `guest-image` package supports normal Nix `.override` arguments.
With a `fireactions` flake input and a `pkgs` package set, define a custom archive:

```nix
fireactions.packages.${pkgs.stdenv.hostPlatform.system}.guest-image.override {
  extraPackages = [ pkgs.php ];
  extraCommands = builtins.readFile ./guest-extra.sh;
  fakeRootCommands = ''
    chown -R 1000:1000 opt/ci
  '';
}
```

Use this expression for your flake's `packages.<system>.guest-image` output.
Build that output with `nix build .#guest-image`.
The builder appends both hooks rather than replacing the standard guest setup.
Use `.override`, not `.overrideAttrs`, to customize the inputs before Docker image layers are built.
The standard setup script lives in `nix/guest-image-setup.sh`.
Keep the guest agent and host binary built from the same source revision.

### Keep image recipes in your host configuration

Fireactions supplies the guest setup, not a catalog of operating system releases.
Your host or image repository owns the base-image pins, output name, tag, and project packages.
Override `fromImage`, `name`, and `tag` to build another image without changing Fireactions.
The base must use the host architecture and a merged `/usr` layout, with `/bin` and `/sbin` linked into `/usr`.
Follow the guest requirements above when adapting a different base.

From your host flake directory, generate a pinned base-image definition:

```bash
mkdir -p images
nix run --inputs-from . nixpkgs#nix-prefetch-docker -- \
  --image-name ubuntu --image-tag 24.04 --arch amd64 \
  --final-image-name ubuntu --final-image-tag 24.04 > images/base-image.nix
```

Select your own image name and release in this command.
For an ARM64 host, use `--arch arm64`.
Keep the generated definition in your host repository, not in Fireactions.

Create `images/project-guest.nix` in that repository:

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

The existing `extraCommands` and `fakeRootCommands` hooks also work in this recipe.
The shared builder still installs the agent, systemd, standard tools, the `ci` user, and passwordless sudo.

Add this inline module to your host flake's `modules` list.
The surrounding `outputs` function must include the Fireactions input as `fireactions`:

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

Keep any other labels from your existing configuration.
Jobs with `runs-on: project` select this pool.
The `fireactions` argument keeps its guest binary identical to the host package.
Run `sudo nixos-rebuild switch` to build and import the archive through the existing graceful update lifecycle.
Disabled pools do not add archives, and the importer imports each archive once.

For an externally built archive, set the pool's `imageArchive` to an absolute filename instead.
Rebuild that archive whenever the host binary changes.
After replacing it, restart `fireactions-image-import.service` to drain Runner and import the new archive.

## Build and export the supplied image

The guest image, kernel, and agent binary must match the host architecture. The examples below use amd64. For a native arm64 host and guest, use `GOARCH=arm64` and Docker's `--platform linux/arm64` instead.

From the repository root, build the static Linux amd64 `fireactions` binary. The guest image build copies this fork-built binary into the guest:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o fireactions ./cmd/fireactions
```

Build from the repository root. The Docker build uses the `fireactions` binary at the repository root.

```bash
docker build --platform linux/amd64 -f images/ubuntu-24.04/Dockerfile \
  -t localhost/fireactions-guest:ubuntu-24.04 .
docker save -o fireactions-guest.tar \
  localhost/fireactions-guest:ubuntu-24.04
```


Import the archive with the devmapper snapshotter into the same containerd namespace that appears in `containerd.namespace` in the Fireactions configuration. The example uses `fireactions`:

```bash
sudo ctr --namespace fireactions images import --local --snapshotter devmapper fireactions-guest.tar
sudo ctr --namespace fireactions images list
```

Use `--local` to unpack with devmapper through the client instead of the containerd transfer service.

Set the configured profile image to the imported reference, for example:

```yaml
image: localhost/fireactions-guest:ubuntu-24.04
image_pull_policy: Never
```

The `Never` policy means containerd must already have that image in the configured namespace. Image names are namespace-scoped. Importing the image into a different namespace does not make it available to Fireactions.

## Configure bootable profiles

A pool profile combines the image reference with a readable kernel, executable Firecracker binary, kernel arguments, and machine size. For example, the supplied configuration defines `ubuntu-24.04` with 2 CPUs and 4 GiB of memory, and `ubuntu-24.04-large` with 4 CPUs and 8 GiB.

Forgejo Runner selects a configured profile by its label, for example `firecracker:firecracker://ubuntu-24.04`. The label does not allow a workflow to boot an arbitrary Docker image. Configure each bootable image and kernel in the host configuration before jobs request it.

A used guest is disposable. Fireactions does not keep job changes for another run or return a claimed guest to idle capacity. To change an image, build and import the replacement and update the profile configuration.
