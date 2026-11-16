# 22 Free-tier limits that are counted per account

Status: resolved
Blocked by: 13

`free_quota_catalog` is keyed by `(vendor, model)` and `quota` keeps one set of
buckets per credential and model. Some vendors count a free limit across the
whole account instead: OpenRouter's 50 free requests a day
(`free_model_daily_requests` on `GET /api/v1/key`) cover every `:free` model.
With two such rows on one key, free-first sees twice the free room there is,
and keeps ordering an exhausted OpenRouter target first until the upstream's
429 places a skip.

Let a catalog row say its limit is per account, and count those buckets per
credential rather than per credential and model.

## Comments

- Resolved: `free_quota_catalog.per_account` (migration 00009), set on the
  OpenRouter `:free` rows. A per-account row's buckets are keyed by the
  credential alone, so every per-account model on one key spends the same
  `free:*` buckets. Rows sharing an account are expected to carry the same
  limits; if they differ, each spend applies its own row's rule to the shared
  bucket.
