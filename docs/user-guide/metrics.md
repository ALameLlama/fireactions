# Metrics

Fireactions exposes Prometheus metrics for its disposable VM lifecycle.
A profile is a named image and VM resource configuration.
A clean idle VM is ready and has never belonged to a job.
A claimed VM belongs to one environment and never returns to the idle pool.

Enable the HTTP listener in the host configuration:

```yaml
metrics:
  enabled: true
  address: 127.0.0.1:9090
```

The listener serves `/metrics` and accepts only a loopback address.
Use a local Prometheus collector or a protected proxy to collect these metrics.
Do not expose this endpoint directly to an untrusted network.

## Lifecycle measurements

| Metric | Type | Meaning | Labels |
| --- | --- | --- | --- |
| `fireactions_clean_idle_vms` | Gauge | Ready VMs that no environment has claimed | `profile` |
| `fireactions_claimed_vms` | Gauge | Published VMs claimed by environments | `profile` |
| `fireactions_active_environments` | Gauge | Environment entries retained until cleanup succeeds | `profile` |
| `fireactions_vm_acquisition_seconds` | Histogram | Time to claim an idle VM or provision a dedicated VM | `profile` |
| `fireactions_vm_boot_seconds` | Histogram | Time spent starting Firecracker | `profile` |
| `fireactions_guest_readiness_seconds` | Histogram | Time spent waiting for the guest agent | `profile` |
| `fireactions_operations_total` | Counter | Environment operations and their results | `profile`, `operation`, `outcome` |
| `fireactions_cleanup_failures_total` | Counter | Cleanup attempts that failed | `profile` |
| `fireactions_vm_ttl_expirations_total` | Counter | Owned VMs revoked after their hard lifetime expired | `profile` |
| `fireactions_stale_vm_reconciliations_total` | Counter | Stale owned VMs moved into recovery cleanup | `profile` |

Operation values are `create`, `start`, `copy_in`, `exec`, `copy_out`, and `remove`.
Outcome values are `success`, `failure`, and `cancelled`.
A nonzero command exit counts as `failure`, even when the Exec stream completes normally.
Cancellation and deadline expiration count as `cancelled`.

The idle target counts clean idle VMs, not claimed VMs.
Provisioning contributes to pending idle capacity but does not appear in the clean idle gauge.
A claim triggers replacement provisioning when the pool is active.
A paused pool can supply an existing idle VM but cannot create a replacement or a cold VM.
Scaling down removes only idle VMs and cancels excess idle provisioning.

Hard lifetime and stale-owner counters count the first durable cleanup transition, not each retry.
The independent reaper writes service logs. The daemon observes removed records and clears its environment and VM gauges.

Histograms and counters appear after their first event.
Labels contain configured profile names, never environment IDs, job arguments, or secrets.
Requests without a resolved configured profile do not create arbitrary profile labels.
Default Go and process metrics remain available.
