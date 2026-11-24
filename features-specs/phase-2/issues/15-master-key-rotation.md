# 15 Master key rotation job

Type: task
Status: resolved
Blocked by: -

`crypto.Keyring` already opens with older keys and every row stores its
`key_id` (R02's decisions), so rotation needs only the re-seal: a job, run as
a `make` target or at startup behind a flag, that re-seals every credential
whose `key_id` is not the current one, in small batches, each row in its own
transaction, safe to interrupt and run again. The old key can then be dropped
from config once a query shows no row still uses it.

Spec: [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

## Answer

- `MASTER_KEY_PREVIOUS`: older keys, comma-separated, that still open what
  they sealed. The service and the job both build the keyring from
  `MASTER_KEY` and these.
- `store.Credentials.Reseal`: lists credentials whose `key_id` is not the
  current one, 100 at a time, and re-seals each in a transaction of its own,
  re-checking the `key_id` under a row lock, so two runs never double up and
  an interrupted run is finished by the next. A row it cannot open stops it
  with the error. `KeyUsage` counts rows per key.
- `rotate-keys`, a subcommand of the gateway binary (`make rotate-keys`
  locally), runs both and logs the counts.
- Production: put the new key in `MASTER_KEY` and the old one in
  `MASTER_KEY_PREVIOUS`, `make deploy`, then on the VM
  `docker compose run --rm gateway-a rotate-keys`. When it reports no row
  under the old key, remove `MASTER_KEY_PREVIOUS` and deploy again.
- Proof: store tests (every row moved, a second run does nothing, the rows
  open under the new key alone, a missing old key is an error, a custom
  credential's base URL survives), and `make rotate-keys` on the dev database:
  81 credentials to a new key, 0 on a second run, 81 back.
