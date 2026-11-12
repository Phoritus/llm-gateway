# 02 internal/crypto: AES-256-GCM seal and open

Status: resolved
Blocked by: 01

Build-order step 3, first piece. Fill `internal/crypto` (still a `doc.go`).

- Seal and open with AES-256-GCM. The key is `config.MasterKey` (32 bytes,
  already validated at startup). A fresh random nonce per value.
- Store a **key id** with each ciphertext so the master key can be rotated by a
  background re-encrypt job later (spec, Security: "Master key rotation").
- Pure functions plus a key type; nothing here touches the database.

Done when round-trip, tamper (flipped byte, wrong key, wrong key id) and
nonce-uniqueness tests pass. Spec: [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
