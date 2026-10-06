# Metrics

Fireactions exposes Prometheus metrics for its disposable VM lifecycle. A profile is a configured bootable guest image and its VM settings. A clean idle VM is ready and has never belonged to a job. A claimed VM belongs to one environment and never returns to the idle pool.

Enable the listener in the host configuration:

```yaml
metrics:
  enabled: true
  address: 127.0.0.1:8081
```

The listener serves `/metrics` on a loopback address. Collect metrics with a local Prometheus server or a protected proxy. Do not expose this endpoint directly to an untrusted network.

## Lifecycle metrics

| Metric | Type | Meaning | Labels |
| --- | --- | --- | --- |
| `fireactions_clean_idle_vms` | Gauge | Ready VMs that no environment has claimed | `profile` |
| `fireactions_claimed_vms` | Gauge | Published VMs claimed by environments | `profile` |
| `fireactions_active_environments` | Gauge | Environment entries retained until cleanup succeeds | `profile` |
| `fireactions_vm_acquisition_seconds` | Histogram | Time to claim an idle VM or provision a dedicated VM | `profile` |
| `fireactions_vm_boot_seconds` | Histogram | Time spent starting Firecracker | `profile` |
| `fireactions_guest_readiness_seconds` | Histogram | Time spent waiting for the guest agent | `profile` |
| `fireactions_operations_total` | Counter | Environment operations by result | `profile`, `operation`, `outcome` |
| `fireactions_cleanup_failures_total` | Counter | Environment cleanup attempts that failed | `profile` |
| `fireactions_vm_ttl_expirations_total` | Counter | Owned VMs revoked after hard lifetime expiry | `profile` |
| `fireactions_stale_vm_reconciliations_total` | Counter | Stale owned VMs moved into recovery cleanup | `profile` |

Operation labels are `create`, `start`, `copy_in`, `exec`, `copy_out`, and `remove`. Outcome labels are `success`, `failure`, and `cancelled`. A nonzero command exit is a failure even when the Exec stream completes normally. Cancellation and deadline expiration use the `cancelled` outcome.

The clean idle gauge does not include VMs that are still provisioning. Provisioning contributes to pending idle capacity. When an active profile loses an idle VM to a claim, it starts replacement provisioning. A paused profile can supply an existing idle VM, but it does not create a replacement or a cold VM. Scaling down removes idle VMs and cancels excess idle provisioning.

Lifetime expiration and stale-owner reconciliation counters count the first durable transition to cleanup, not each retry. The independent reaper writes service logs. The daemon observes removed records and clears its environment and VM gauges.

Histograms and counters appear after their first event. Labels use configured profile names. They do not use environment IDs, job arguments, or secrets. A request without a resolved configured profile does not create an arbitrary profile label. Default Go and process metrics are also available.
