# 08 SSRF-guarded HTTP client

Type: task
Status: ready-for-agent
Blocked by: -

Custom endpoints (09) let a user name a URL the gateway will call, which is
the server-side request forgery surface ticket R03 kept out of Phase 1 by
fixing the vendor set. Build the guard first, as a package of its own:

- An `http.Client` whose dialer checks every resolved address at connect
  time (`net.Dialer.Control`), so DNS rebinding cannot slip past a check made
  earlier.
- Refuse loopback, private, link-local (the 169.254.169.254 metadata address
  included), CGNAT, multicast and unspecified addresses, in IPv4 and IPv6,
  IPv4-mapped IPv6 forms included.
- Redirects re-checked hop by hop, or not followed at all, which is what the
  Phase 1 adapters already do.
- https only, and no proxy taken from the environment.

Table tests for each address class, and a test that a hostname resolving to
127.0.0.1 is refused at dial time.

Spec: [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
