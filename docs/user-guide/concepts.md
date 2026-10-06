# Concepts

## Profiles and pools

A profile is a configured bootable guest image, kernel, and machine size. A pool is a named profile that Fireactions makes available to Forgejo Runner. A profile is not an arbitrary Docker image name. Fireactions must be configured with the image, kernel, and Firecracker settings needed to boot it.

The Forgejo Runner label can be `firecracker:firecracker://ubuntu-24.04`. The backend selects a profile from the request `image` field first, then `label_arg`, then the backend `profile` option. The selected value must name a configured profile. There is no implicit profile, and arbitrary image names are not accepted.

The supplied configuration keeps one `ubuntu-24.04` VM idle and sets `ubuntu-24.04-large` to zero idle replicas. The first profile uses two virtual CPUs and 4 GiB of memory. The large profile uses four virtual CPUs and 8 GiB. Change replica counts to match host capacity.

## Disposable idle capacity

An idle VM is clean and has never run a job. When a job claims it, Fireactions removes it from idle capacity and provisions a clean replacement to restore the configured idle target. A claimed VM is not reused, even if a job fails.

Provisioning can take time. When no idle VM is available, Fireactions can create a VM for the request. Fireactions does not preserve job state between VMs.

## Hard leases and cleanup

A lease is the maximum time allowed for one VM job. The lease starts when the plugin receives the VM creation request, so provisioning time counts. A positive requested timeout is capped at `leases.max_lifetime`. If the request omits the timeout or sets it to zero, Fireactions uses the configured maximum.

At hard expiry, Fireactions cancels active work and stops the VM. `leases.cleanup_grace` bounds later cleanup attempts. It does not extend the job lease. The independent reaper timer scans for expired or abandoned VM resources, including after a host daemon crash.

At host daemon startup, Fireactions destroys old claimed and idle VMs before it accepts jobs. It does not resume an old job. Protect the journal and host state directory. If a journal record is corrupt, do not delete it without resolving the recorded resources.

## Host and guest roles

Forgejo Runner runs on the host and connects to `unix:///run/fireactions/plugin.sock`. Its registration token stays in a host-only file with mode `0600`. Do not copy the token into a guest image or plugin configuration.

The guest runs a systemd unit for the Fireactions agent. The guest image creates the `ci` user and workspace. The unit uses cgroup v2 delegation so CI processes can use their delegated process controls.
The host service needs KVM, containerd, CNI, network, and guest-rootfs privileges. Fireactions does not add Firecracker jailer isolation. Do not treat this setup as a security boundary against a malicious job that can exploit a host weakness.

Runner resource cap adjustments are advisory and ignored by Fireactions. Fireactions does not provide service containers, Docker-container actions, SSH login, guest passwords, stdin, PTY, or signal RPC.

## Retained names

Fireactions retains some historical names, including `fireactions`, its module path, and the `fireactions` host state and socket names. These names do not mean that GitHub is the runner backend. GitHub remains the source, CI, and release host.
