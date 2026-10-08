# Debug workflow failures

Fireactions does not provide SSH login, a default guest password, or a `tmate` session. It also does not provide stdin, PTY, or signal RPC support. Use the supported workflow logs and guest agent logs to find failures.

Use the Forgejo workflow run page to read each step's output and identify the first failing command. Add temporary diagnostic commands to print relevant non-secret state, such as the working directory, tool version, and selected input files. Remove diagnostic output that reveals secrets before sharing logs.

Use the host CLI to list VMs and read agent logs:

```sh
fireactions ps
fireactions logs VM_ID --follow
```

The host CLI must be able to access the Fireactions Unix socket. The service account or your account must have access through the configured socket group. The `logs` command reports guest agent logs. It does not provide an interactive shell.

For boot or provisioning failures, inspect the host Fireactions service journal and the containerd, CNI, and Firecracker logs for the configured profile. Use `fireactions validate --host /etc/fireactions/config.yaml` to check configured host prerequisites. Do not expose workflow secrets in logs or diagnostic output.
