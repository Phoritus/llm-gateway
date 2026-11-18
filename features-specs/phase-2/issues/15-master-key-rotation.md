# 15 Master key rotation job

Type: task
Status: ready-for-agent
Blocked by: -

`crypto.Keyring` already opens with older keys and every row stores its
`key_id` (R02's decisions), so rotation needs only the re-seal: a job, run as
a `make` target or at startup behind a flag, that re-seals every credential
whose `key_id` is not the current one, in small batches, each row in its own
transaction, safe to interrupt and run again. The old key can then be dropped
from config once a query shows no row still uses it.

Spec: [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
