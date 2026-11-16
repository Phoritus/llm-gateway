# 22 Free-tier limits that are counted per account

Status: ready-for-agent
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
