# Concepts

## Profiles and pools

A profile is a configured bootable guest image, kernel, and machine size. A pool is a named profile that Fireactions makes available to Forgejo Runner. A profile is not an arbitrary Docker image name. Fireactions must be configured with the image, kernel, and Firecracker settings needed to boot it.

The Forgejo Runner label can be `firecracker:firecracker://ubuntu-24.04`. The backend selects a profile from the request `image` field first, then `label_arg`, then the backend `profile` option. The selected value must name a configured profile. There is no implicit profile, and arbitrary image names are not accepted.

The supplied configuration keeps one `ubuntu-24.04` VM idle and sets `ubuntu-24.04-large` to zero idle replicas. The first profile uses two virtual CPUs and 4 GiB of memory. The large profile uses four virtual CPUs and 8 GiB. Change replica counts to match host capacity.

## Configure guest pools

The backend uses the same YAML configuration on NixOS, Ubuntu, and other Linux hosts.
Firecracker requires Linux with KVM.
On Ubuntu and other non-NixOS hosts, edit `/etc/fireactions/config.yaml`.
Start from the [complete YAML example](https://github.com/ALameLlama/fireactions/blob/main/examples/fireactions.yaml).
Keep each profile's image, user, Firecracker executable, and kernel aligned with your host.

| NixOS field under `services.fireactions.pools.<name>` | Backend YAML field in each `pools` entry |
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

Set `replicas: 0` to keep a profile available without prewarming guests.
Replica counts do not limit active jobs.
CPU and memory values must be positive. Memory values use MiB.
For eight standard idle guests and no large idle guests, set their `replicas` values to `8` and `0`.
Keep the example's CPU and memory values if you want 2 CPUs/4096 MiB and 4 CPUs/8192 MiB.
Budget memory for idle guests, active jobs, replacement guests, and the host.
Guest RAM is allocated on demand, but real workloads can consume the full configured amount.

Add a complete YAML profile to make another guest size available.
Remove its entry to disable it, and remove any corresponding Runner label.
On NixOS, add a named profile or set its `enable = false` instead.
Use the [configuration reference](../reference/configuration.md#image-profiles) for additional fields such as rootfs and network rate limiters.
On NixOS, set these fields under the profile's `settings`.
These values override matching generated fields. Global `settings.pools` replaces the entire list.

Before restarting the backend, stop Runner polling and let active jobs finish.
A backend restart destroys all existing guests. Jobs cannot resume afterward.
On a host installed with the shell installer, run:

```bash
sudo /usr/local/bin/fireactions validate /etc/fireactions/config.yaml
sudo /usr/local/bin/fireactions validate --host /etc/fireactions/config.yaml
sudo systemctl restart fireactions.service
sudo /usr/local/bin/fireactions pools list
```

Restart Runner polling after the backend is ready.
Keep `fireactions-reaper.timer` enabled.
On NixOS, change the Nix options and run `sudo nixos-rebuild switch --flake .` from the host flake directory.
Do not edit its generated `/etc/fireactions/config.yaml`.

## Disposable idle capacity

An idle VM is clean and has never run a job. When a job claims it, Fireactions removes it from idle capacity and provisions a clean replacement to restore the configured idle target. A claimed VM is not reused, even if a job fails.

Provisioning can take time. When no idle VM is available, Fireactions can create a VM for the request. Fireactions does not preserve job state between VMs.

## Hard leases and cleanup

A lease is the maximum time allowed for one VM job. The lease starts when the plugin receives the VM creation request, so provisioning time counts. A positive requested timeout is capped at `leases.max_lifetime`. If the request omits the timeout or sets it to zero, Fireactions uses the configured maximum.

At hard expiry, Fireactions cancels active work and stops the VM. `leases.cleanup_grace` bounds later cleanup attempts. It does not extend the job lease. The independent reaper timer scans for expired or abandoned VM resources, including after a host daemon crash.

At host daemon startup, Fireactions destroys old claimed and idle VMs before it accepts jobs. It does not resume an old job. Protect the journal and host state directory. If a journal record is corrupt, do not delete it without resolving the recorded resources.

The daemon and independent reaper share a lock for resource reclamation. They stop recorded VM processes before waiting for this lock. Cleanup waits remain bounded by the configured cleanup grace.
Do not remove journal lock files while a daemon or reaper can use the state directory.

## Host and guest roles

Forgejo Runner runs on the host and connects to `unix:///run/fireactions/plugin.sock`. Its registration token stays in a host-only file with mode `0600`. Do not copy the token into a guest image or plugin configuration.

The guest runs a systemd unit for the Fireactions agent. The guest image creates the `ci` user and workspace. The unit uses cgroup v2 delegation so CI processes can use their delegated process controls.
The host service needs KVM, containerd, CNI, network, and guest-rootfs privileges. Fireactions does not add Firecracker jailer isolation. Do not treat this setup as a security boundary against a malicious job that can exploit a host weakness.

Runner resource cap adjustments are advisory and ignored by Fireactions. Fireactions does not provide service containers, Docker-container actions, SSH login, guest passwords, stdin, PTY, or signal RPC.

## File transfers

Fireactions transfers files as tar archives within the guest workspace. Hard links in an upload must target a regular file created earlier in that upload. Pre-existing destination files cannot be hard-link targets. Hard-link chains within the same upload remain supported.

Archive exports keep directory descriptors bounded, including for deeply nested directory trees. Transfer byte and entry limits still apply.

## Retained names

Fireactions retains the `fireactions` binary name and its host state and socket names. The Go module is `github.com/ALameLlama/fireactions`. GitHub hosts the source repository, CI, and releases. Forgejo is the runner backend.
