# /// script
# requires-python = ">=3.11"
# dependencies = ["openai==3.51.0", "httpx==0.28.1", "psycopg[binary]==3.3.9"]
# ///
"""Check the real provider adapters end to end through a running development
gateway, with the provider keys in the env file.

For each vendor whose key is set it stores the key as a credential, runs the
credential test, and asks one short question plain and streamed. Then, on the
first vendor that answered: a bad key fails over to the good one, a repeated
temperature-0 question is served from the exact cache, and the usage API has
counted it all. Each run creates a throwaway account and spends a handful of
free-tier requests. Keys are read from the environment and never printed.
"""

import os
import secrets
import sys
import time

import httpx
import openai
import psycopg

BASE = os.environ.get("LIVE_BASE_URL", "http://127.0.0.1:%s" % os.environ.get("PORT", "8080"))

VENDORS = {
    "groq": (["GROQ_API_KEY"], "openai/gpt-oss-20b"),
    "gemini": (["GEMINI_API_KEY"], "gemini-3.5-flash-lite"),
    "cerebras": (["CEREBRAS_API_KEY", "CEREBRAL_API_KEY"], "gpt-oss-120b"),
    "openrouter": (["OPENROUTER_API_KEY"], "nvidia/nemotron-3.5-lightning:free"),
    "openai": (["OPENAI_API_KEY"], "gpt-5-nano"),
    "mistral": (["MISTRAL_API_KEY"], "mistral-small-latest"),
    "anthropic": (["ANTHROPIC_API_KEY"], "claude-haiku-5-5"),
}

QUESTION = [{"role": "user", "content": "Reply with exactly one word: pong"}]

failures: list[str] = []


def check(name: str, fn) -> bool:
    try:
        fn()
    except Exception as exc:
        failures.append(name)
        print(f"FAIL  {name}: {type(exc).__name__}: {exc}")
        return False
    print(f"ok    {name}")
    return True


def expect(cond: bool, what: str) -> None:
    if not cond:
        raise AssertionError(what)


def key_for(names: list[str]) -> str | None:
    for name in names:
        if value := os.environ.get(name, "").strip():
            return value
    return None


def control_plane() -> httpx.Client:
    email = f"live-{secrets.token_hex(6)}@example.test"
    password = secrets.token_urlsafe(18)
    api = httpx.Client(base_url=BASE + "/api", timeout=30)
    r = api.post("/auth/signup", json={"email": email, "password": password, "workspace_name": "live"})
    expect(r.status_code in (200, 201, 202), f"signup answered {r.status_code}: {r.text}")
    with psycopg.connect(os.environ["DATABASE_URL_LOCAL"]) as db:
        db.execute("UPDATE users SET email_verified_at = now() WHERE email = %s", (email,))
    r = api.post("/auth/login", json={"email": email, "password": password})
    expect(r.status_code == 200, f"login answered {r.status_code}: {r.text}")
    api.headers["X-CSRF-Token"] = api.cookies["gw_csrf"]
    return api


def credential(api: httpx.Client, vendor: str, name: str, secret: str) -> str:
    r = api.post("/credentials", json={"provider": vendor, "name": name, "secret": secret})
    expect(r.status_code == 201, f"credential answered {r.status_code}: {r.text}")
    return r.json()["id"]


def route(api: httpx.Client, alias: str, targets: list[tuple[str, str]], cache: str = "off") -> None:
    r = api.post("/routes", json={
        "alias": alias, "cache": cache,
        "targets": [{"credential_id": c, "model": m} for c, m in targets],
    })
    expect(r.status_code == 201, f"route {alias} answered {r.status_code}: {r.text}")


def main() -> int:
    try:
        httpx.get(BASE + "/healthz", timeout=3).raise_for_status()
    except httpx.HTTPError as exc:
        print(f"no gateway at {BASE} ({exc}); start one with `make run`", file=sys.stderr)
        return 2

    present = {v: (k, os.environ.get(f"LIVE_MODEL_{v.upper()}", m))
               for v, (names, m) in VENDORS.items() if (k := key_for(names))}
    skipped = sorted(set(VENDORS) - set(present))
    if only := os.environ.get("LIVE_VENDORS"):
        present = {v: present[v] for v in only.split(",") if v in present}
    if not present:
        print("no provider key is set in the env file", file=sys.stderr)
        return 2
    print(f"vendors: {', '.join(present)}" + (f" (no key: {', '.join(skipped)})" if skipped else ""))

    api = control_plane()
    r = api.post("/gateway-keys", json={"name": "live"})
    expect(r.status_code == 201, f"gateway key answered {r.status_code}: {r.text}")
    client = openai.OpenAI(base_url=BASE + "/v1", api_key=r.json()["key"], max_retries=0, timeout=120)

    working: list[tuple[str, str, str]] = []
    for vendor, (secret, model) in present.items():
        cred = credential(api, vendor, vendor, secret)
        alias = f"live-{vendor}"
        route(api, alias, [(cred, model)])

        def key_test():
            r = api.post(f"/credentials/{cred}/test")
            expect(r.status_code == 200 and r.json().get("ok"), f"test answered {r.status_code}: {r.text}")

        def plain():
            resp = client.chat.completions.create(model=alias, messages=QUESTION, max_tokens=1024)
            expect(resp.choices[0].finish_reason in ("stop", "length"), f"finish_reason {resp.choices[0].finish_reason!r}")
            expect(bool(resp.choices[0].message.content), "empty content")
            expect(resp.usage is not None and resp.usage.total_tokens > 0, f"usage {resp.usage!r}")

        def streamed():
            text, finish, usage = "", None, None
            for chunk in client.chat.completions.create(
                    model=alias, messages=QUESTION, max_tokens=1024, stream=True,
                    stream_options={"include_usage": True}):
                if chunk.choices:
                    text += chunk.choices[0].delta.content or ""
                    finish = chunk.choices[0].finish_reason or finish
                usage = chunk.usage or usage
            expect(bool(text), "no content streamed")
            expect(finish is not None, "no finish_reason")
            expect(usage is not None and usage.total_tokens > 0, f"usage {usage!r}")

        ok = check(f"{vendor}: credential test", key_test)
        ok = check(f"{vendor}: answer ({model})", plain) and ok
        ok = check(f"{vendor}: stream ({model})", streamed) and ok
        if ok:
            working.append((vendor, cred, model))

    if not working:
        print("no vendor answered; skipping the routing checks")
    else:
        vendor, cred, model = working[0]
        bad = credential(api, vendor, f"{vendor}-bad", "invalid-" + secrets.token_hex(16))

        def bad_key_test():
            r = api.post(f"/credentials/{bad}/test")
            expect(r.status_code == 200 and not r.json().get("ok") and r.json().get("code") == "key_rejected",
                   f"test answered {r.status_code}: {r.text}")

        def failover():
            route(api, "live-failover", [(bad, model), (cred, model)])
            raw = client.chat.completions.with_raw_response.create(
                model="live-failover", messages=QUESTION, max_tokens=1024)
            raw.parse()
            expect(raw.headers.get("x-fallback-attempts") == "2",
                   f"X-Fallback-Attempts {raw.headers.get('x-fallback-attempts')!r}, trail {raw.headers.get('x-fallback-trail')!r}")

        def cache_hit():
            route(api, "live-cached", [(cred, model)], cache="always")
            ask = dict(model="live-cached", messages=QUESTION, max_tokens=1024, temperature=0)
            first = client.chat.completions.with_raw_response.create(**ask)
            first.parse()
            expect(first.headers.get("x-cache") == "MISS", f"first X-Cache {first.headers.get('x-cache')!r}")
            second = client.chat.completions.with_raw_response.create(**ask)
            second.parse()
            expect(second.headers.get("x-cache") == "HIT", f"second X-Cache {second.headers.get('x-cache')!r}")

        def usage_counted():
            for _ in range(20):
                r = api.get("/usage")
                expect(r.status_code == 200, f"usage answered {r.status_code}: {r.text}")
                body = r.json()
                if any(e.get("cached") for e in body.get("recent", [])):
                    return
                time.sleep(0.5)
            raise AssertionError(f"no cached event in recent usage: {r.text[:400]}")

        check(f"{vendor}: a bad key is reported as key_rejected", bad_key_test)
        check(f"{vendor}: a bad key fails over to the good one", failover)
        check(f"{vendor}: a repeated temperature-0 question is a cache hit", cache_hit)
        check("usage records the requests", usage_counted)

    if failures:
        print(f"{len(failures)} check(s) failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
