# Configuration

Fireactions reads a YAML configuration file. The default path is `/etc/fireactions/config.yaml`. The host service uses this file. Run `fireactions validate FILE` to check its schema, or run `fireactions validate --host FILE` to check the existing host prerequisites too.

The example below shows the supported configuration fields. Fireactions applies defaults to optional values when it loads the file. See the notes after the example for those defaults and limits.

```yaml
socket_path: /run/fireactions/plugin.sock
socket_group: fireactions
state_dir: /var/lib/fireactions

containerd:
  address: /run/containerd/containerd.sock
  namespace: fireactions

metrics:
  enabled: true
  address: 127.0.0.1:8081

network:
  resolver_path: /etc/resolv.conf

guest:
  startup_timeout: 2m
  max_transfer_bytes: 10737418240
  max_archive_entries: 100000

leases:
  max_lifetime: 3h2m
  cleanup_grace: 2m
  reap_interval: 10s

pools:
  - name: ubuntu-24.04
    replicas: 1
    image: localhost/fireactions-guest:ubuntu-24.04
    image_pull_policy: Never
    default_user: ci
    firecracker:
      binary_path: /usr/local/bin/firecracker
      kernel_image_path: /var/lib/fireactions/kernels/6.1/vmlinux
      kernel_args: "console=ttyS0 reboot=k panic=1 pci=off nomodules rw init=/sbin/init systemd.unified_cgroup_hierarchy=1"
      machine_config:
        vcpu_count: 2
        mem_size_mib: 4096
```

## Host settings

`socket_path` selects the local Unix socket for the Forgejo runner plugin. `socket_group` names the Unix group that can access this socket. `state_dir` stores private VM ownership records and runtime state. Use absolute, clean paths. Do not share this state directory between Fireactions servers.

Use a root-owned state directory below trusted, root-owned ancestors. Fireactions rejects symlinks, foreign-owned ancestors, and writable ancestors without the sticky bit. It creates missing state directories with mode `0700`. Do not use `/` as the state directory.

Use a dedicated, real directory for `socket_path`, such as `/run/fireactions`. Fireactions rejects sockets directly in `/`, `/run`, `/var/run`, `/tmp`, or `/var/tmp`. The socket itself uses mode `0660`.

Every ancestor directory must be owned by `root`. An ancestor must not allow group or other writes unless it has the sticky bit. The sticky bit prevents users from replacing entries that belong to other users. Root-owned sticky directories, such as `/tmp`, can contain root-owned socket directories. User-owned ancestors are not accepted, even with the sticky bit.

Ancestor symlinks must be owned by `root`. Directories that contain these symlinks and directories in their resolved targets must satisfy the same ancestor policy. Fireactions accepts trusted aliases, such as `/var/run` pointing to `/run`. The socket directory itself cannot be a symlink.

For every new directory in the socket path, Fireactions sets the owner to `root`, the group to `socket_group`, and the mode to `0750`. This lets members of `socket_group` traverse newly created nested directories. An existing socket directory must already have this owner, group, and mode, without special mode bits. Fireactions does not change the ownership or permissions of existing directories. Existing ancestors must also allow members of `socket_group` to traverse the path for clients to connect.

`containerd.address` selects the containerd socket. `containerd.namespace` selects the namespace that stores guest images and snapshots. Import each guest image into this namespace. Containerd namespaces separate image names and snapshots.

`network.resolver_path` selects the host resolver file used to set nameservers for host-local IPAM and the Firecracker guest network. Fireactions does not copy the whole resolver file into the guest. The path must be absolute and clean, and the file must provide usable IPv4 nameservers.

Set `log_level` to `debug`, `info`, `warn`, `error`, `fatal`, `panic`, or `trace`. The default is `debug`.

## Metrics settings

Set `metrics.enabled` to `true` to serve Prometheus metrics. The default is enabled at `127.0.0.1:8081`. `metrics.address` must be a loopback host and port. The endpoint is `/metrics`. Do not expose it directly to an untrusted network.

## Guest and lease limits

`guest.startup_timeout` limits guest startup and readiness time. `guest.max_transfer_bytes` limits the total bytes in a file transfer and must be positive. `guest.max_archive_entries` limits entries in one archive and must be between `1` and `2147483647`, inclusive, to fit the guest protocol's int32 field. Defaults are `2m`, `10737418240`, and `100000`.

`leases.max_lifetime` limits each environment lifetime, including VM provisioning. A missing or zero requested lifetime uses this limit. The default is `3h2m`. At expiry, Fireactions cancels work and stops the VM. `leases.cleanup_grace` limits cleanup attempts after execution ends. It does not extend the job lifetime. `leases.reap_interval` sets how often the live daemon reconciles stale resources. Its default is `10s`. The independent systemd timer runs the one-shot reaper every `10s`; this timer interval is not set by `reap_interval`. `cleanup_grace` defaults to `2m`. All three durations must be positive. The reap interval cannot exceed cleanup grace, and startup timeout cannot exceed maximum lifetime.

## Image profiles

Each item in `pools` defines one named, bootable image profile. A profile name is not a request to pull or run an arbitrary Docker image. `image` names an OCI image that Fireactions uses as a root filesystem. Prepare a compatible Linux guest image and import it into the configured containerd namespace. The guest must contain the Fireactions agent and meet its boot contract.

`replicas` sets the target number of ready, never-claimed idle VMs. The value can be zero. A claimed VM is disposable and never returns to the idle pool. With an active profile at zero replicas, Fireactions creates a cold VM for an acquisition.

`image_pull_policy` is required. Set it to `Always`, `IfNotPresent`, or `Never`. `default_user` selects an existing guest account. The default is `ci`. The example uses this non-root account. Fireactions uses the configured account for workflow commands and transferred file ownership.

`firecracker.binary_path` and `firecracker.kernel_image_path` select the host Firecracker executable and Linux kernel. `machine_config.vcpu_count` and `machine_config.mem_size_mib` set positive CPU and memory values. Optional `kernel_args` passes kernel arguments. Optional `network_interface` and `rootfs` fields can set Firecracker rate limiters. Their `bandwidth` and `ops` token buckets each require positive `size` and `refill_time` values. `one_time_burst` is optional and cannot be negative. Omitted limits are unlimited.

Profile names must be unique. The configuration must contain at least one profile. Unknown YAML fields fail validation. The plugin selects a profile by request image first, then `label_arg`, then the `profile` backend option. Fireactions has no implicit profile.
