# Guest images

A Fireactions guest image is a Linux root filesystem in a container image. Fireactions uses containerd to create a root filesystem for a configured Firecracker profile. The profile must name an image that is imported into the same containerd namespace configured for Fireactions.

## Guest image requirements

The guest image must include the Fireactions binary and a systemd unit for the guest agent. The supplied Ubuntu 24.04 image creates the `ci` user and `/workspace` directory. Its unit starts the agent as root so the agent can set up the job user and workspace. Job commands run as the configured default user, `ci`.

The unit enables delegated cgroup v2 control for the guest agent and its CI processes. The guest kernel command line must enable the unified cgroup hierarchy. See the supplied [`fireactions-agent.service`](https://github.com/ALameLlama/fireactions/blob/main/images/ubuntu-24.04/fireactions-agent.service) and [kernel guide](kernels.md).

Fireactions does not support SSH login or a default guest password. Do not add either as a way to debug a guest. Fireactions does not provide stdin, PTY, or signal RPC access.

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
