# Create a custom guest image

Fireactions boots a Linux guest from a prepared root filesystem image. The guest image recipe is `images/ubuntu-24.04/Dockerfile`. It installs the guest services and copies in the Fireactions agent. Do not use the repository's top-level Dockerfile as a guest image recipe.

To add software, edit the guest image recipe. For example, add packages to its existing `apt-get install` list:

```dockerfile
RUN apt-get update \
    && apt-get install -y --no-install-recommends systemd systemd-sysv bash coreutils git ca-certificates curl make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
```

Build the guest image from the repository root. The recipe expects a Linux guest-agent binary named `fireactions` at the repository root. Build the agent for the guest architecture before you build the image.

```sh
docker build --platform linux/amd64 \
  --file images/ubuntu-24.04/Dockerfile \
  --tag localhost/fireactions-guest:custom .
docker save localhost/fireactions-guest:custom -o fireactions-guest.tar
sudo ctr --namespace fireactions images import --snapshotter devmapper fireactions-guest.tar
```

Import the archive into the namespace named by `containerd.namespace` in the host configuration. Set the profile's `image` to the imported image name and `image_pull_policy` to `Never` when the image is loaded locally. Keep the image name, namespace, architecture, guest kernel, and boot configuration aligned.

The guest must include a compatible Linux system, an init system, a `ci` user (or the configured `default_user`), and the enabled Fireactions agent service. A profile selects a prepared image that Fireactions can boot. It does not enable arbitrary Docker images as workflow containers. The Forgejo runner plugin protocol is pinned to Runner 13.2 alpha and supports Linux guests only.
