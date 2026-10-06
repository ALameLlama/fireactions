# Installation

This guide installs the Fireactions host service on Linux and connects a Forgejo Runner. Fireactions uses an existing KVM, containerd, devmapper snapshotter, CNI network, Firecracker binary, kernel, and CNI plugin executables.

## Requirements

Use a Linux host with root or `sudo` access. The host needs usable `/dev/kvm`, containerd with devmapper configured, a CNI network for Fireactions, the configured Firecracker binary and guest kernel, and the CNI plugins named by that network.

Configure a host resolver file with usable nameservers. The example uses `/run/systemd/resolve/resolv.conf`. Set `network.resolver_path` to the real resolver file on your host. The guest network must reach Forgejo and any package registries or services that workflows use.

The host service needs privileges to use KVM, containerd, CNI, host networking, and guest root filesystems. This is privileged host execution. Fireactions does not add a Firecracker jailer or a stronger host isolation boundary.

The supplied host profiles use Ubuntu 24.04. The `ubuntu-24.04` profile uses 2 CPUs and 4 GiB of memory. The `ubuntu-24.04-large` profile uses 4 CPUs and 8 GiB. Each profile needs a bootable guest image, a readable kernel, and an executable Firecracker binary.

## Prepare the configuration and image

Start from [`examples/fireactions.yaml`](../../examples/fireactions.yaml). Set the containerd socket, namespace, Firecracker path, kernel path, and host resolver path to values that exist on your host. Use an image name that you import into the same containerd namespace. The example namespace is `fireactions`.

Build and export the guest image as a Docker image archive. Import that archive with the devmapper snapshotter into the configured namespace. The example commands use the `fireactions` namespace:

```bash
sudo ctr --namespace fireactions images import --snapshotter devmapper fireactions-guest.tar
sudo ctr --namespace fireactions images list
```

The image name in the configuration must match the imported image reference. See [Images](images.md) for the guest image contract and build/export/import flow.

## Install Fireactions

Build the static Linux amd64 `fireactions` binary from the repository root as described in [Images](images.md). Then use that fork-built binary. The default installer preflights the host and installs Fireactions without replacing the existing containerd service or configuration, LVM, or CNI configuration.

```bash
sudo ./install.sh --binary ./fireactions --config examples/fireactions.yaml
```

The installer installs the Fireactions binary and configuration, creates the socket group and systemd units, writes the documented IP-forwarding sysctl setting, reloads systemd, and enables the main service and independent reaper timer. It does not download an upstream Fireactions binary.

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

Use `OWNER/REPO` for a repository-scoped Runner. The Forgejo command reads the existing 40-character token file and returns a registration UUID. Keep the UUID and token private. Do not print the token. Transfer the token securely to the host that runs Forgejo Runner, then store it in a file owned by the Runner OS user with mode `0600`.

Configure Runner with the Forgejo server URL, the registration UUID from Forgejo, and the host socket `unix:///run/fireactions/plugin.sock`. Set the token file path in a `file://` `token_url` field. Use the Fireactions label format from [`examples/forgejo-runner.yaml`](../../examples/forgejo-runner.yaml), for example `firecracker:firecracker://ubuntu-24.04`. The example shows the Runner configuration shape:

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

Add the Runner OS user to the `fireactions` socket group. For the example user, run:

```bash
sudo usermod -aG fireactions runner
```

Log out and back in to apply the new group membership, then restart the service that runs Forgejo Runner. Start Forgejo Runner with the completed host configuration:

```bash
forgejo-runner daemon --config examples/forgejo-runner.yaml
```

The host socket grants access to the Fireactions plugin. Do not expose it to guest jobs.

A label selects a configured Fireactions profile. It does not select an arbitrary registry image. Configure a separate label for each profile that Runner can request.

## Optional host setup

The installer keeps host provisioning behind the explicit `--setup-host` option. Use it only when you intend to install or configure missing host dependencies. It can change host packages and services.

Run `install.sh` from the repository tree. It uses the service and timer files under `packaging/systemd/`. Optional setup downloads missing Firecracker `1.17.0`, CNI plugins, and the pinned guest kernel. It does not build or import the guest image.

Storage setup can format and destroy data. Do not pass storage options unless you have selected the correct dedicated device and approved the destructive operation interactively. Storage setup requires both `--containerd-snapshotter-device` and `--format-device`. Never run those storage options on an existing host whose data must remain intact.

## Verify the service

Check the main service and independent reaper timer:

```bash
sudo systemctl status fireactions.service
sudo systemctl status fireactions-reaper.timer
fireactions pools list
```

See [First build](first-build.md) to run a workflow after Forgejo Runner connects.
