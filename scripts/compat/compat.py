# /// script
# requires-python = ">=3.11"
# dependencies = ["openai==3.51.0", "httpx==0.28.1", "psycopg[binary]==3.3.9"]
# ///
"""Point the official OpenAI Python SDK at a running gateway and check that it
behaves: a plain answer, a stream, and each error a client has to handle.

It works against the mock provider, so it spends no provider quota and runs
only in development. It creates a throwaway account, confirms its address
directly in the development database (the dev mailer only logs the link), and
gives the account a mock credential, a route per mock script and a gateway
key. The key is written to COMPAT_KEY_FILE when that is set, so the IDE client
check and the load test can reuse the same workspace. --setup-only stops there.
"""

import os
import secrets
import sys

import httpx
import openai
import psycopg

BASE = os.environ.get("COMPAT_BASE_URL", "http://127.0.0.1:%s" % os.environ.get("PORT", "8080"))

MOCK_SCRIPT_BY_ALIAS = {
    "compat-echo": "mock/echo",
    "compat-fail": "mock/fail",
    "compat-rate-limited": "mock/rate-limited",
    "compat-context-too-long": "mock/context-too-long",
    "compat-break": "mock/break",
}

failures: list[str] = []


def check(name: str, fn) -> None:
    try:
        fn()
    except Exception as exc:
        failures.append(name)
        print(f"FAIL  {name}: {type(exc).__name__}: {exc}")
    else:
        print(f"ok    {name}")


def expect(cond: bool, what: str) -> None:
    if not cond:
        raise AssertionError(what)


def bootstrap() -> str:
    email = f"compat-{secrets.token_hex(6)}@example.test"
    password = secrets.token_urlsafe(18)
    api = httpx.Client(base_url=BASE + "/api", timeout=10)

    r = api.post("/auth/signup", json={"email": email, "password": password, "workspace_name": "compat"})
    expect(r.status_code in (200, 201, 202), f"signup answered {r.status_code}: {r.text}")
    with psycopg.connect(os.environ["DATABASE_URL_LOCAL"]) as db:
        db.execute("UPDATE users SET email_verified_at = now() WHERE email = %s", (email,))

    r = api.post("/auth/login", json={"email": email, "password": password})
    expect(r.status_code == 200, f"login answered {r.status_code}: {r.text}")
    api.headers["X-CSRF-Token"] = api.cookies["gw_csrf"]

    def isolated_credential(alias: str) -> str:
        r = api.post("/credentials", json={"provider": "mock", "name": alias, "secret": "mock-" + secrets.token_hex(8)})
        expect(r.status_code == 201, f"credential answered {r.status_code}: {r.text}")
        return r.json()["id"]

    for alias, model in MOCK_SCRIPT_BY_ALIAS.items():
        r = api.post("/routes", json={
            "alias": alias, "cache": "off",
            "targets": [{"credential_id": isolated_credential(alias), "model": model}],
        })
        expect(r.status_code == 201, f"route {alias} answered {r.status_code}: {r.text}")

    r = api.post("/gateway-keys", json={"name": "compat"})
    expect(r.status_code == 201, f"gateway key answered {r.status_code}: {r.text}")
    return r.json()["key"]


def main() -> int:
    try:
        httpx.get(BASE + "/healthz", timeout=3).raise_for_status()
    except httpx.HTTPError as exc:
        print(f"no gateway at {BASE} ({exc}); start one with `make run`", file=sys.stderr)
        return 2

    key = bootstrap()
    if path := os.environ.get("COMPAT_KEY_FILE"):
        with open(path, "w") as f:
            f.write(key)
    if "--setup-only" in sys.argv:
        return 0

    print(f"openai {openai.__version__} against {BASE}/v1")
    client = openai.OpenAI(base_url=BASE + "/v1", api_key=key, max_retries=0, timeout=30)
    hello = [{"role": "user", "content": "hello from the sdk"}]

    def non_streaming():
        resp = client.chat.completions.create(model="compat-echo", messages=hello)
        expect(resp.object == "chat.completion", f"object is {resp.object!r}")
        expect(resp.choices[0].finish_reason == "stop", f"finish_reason {resp.choices[0].finish_reason!r}")
        expect(bool(resp.choices[0].message.content), "empty content")
        expect(resp.usage is not None and resp.usage.total_tokens > 0, f"usage {resp.usage!r}")

    def models_list():
        ids = [m.id for m in client.models.list()]
        expect(sorted(ids) == sorted(MOCK_SCRIPT_BY_ALIAS), f"models are {ids!r}")
        expect(not any(m.startswith("mock/") for m in ids), "the list names an upstream model")

    def raw_headers():
        raw = client.chat.completions.with_raw_response.create(model="compat-echo", messages=hello)
        expect(raw.headers.get("x-fallback-attempts") == "1", f"X-Fallback-Attempts {raw.headers.get('x-fallback-attempts')!r}")
        expect(raw.headers.get("x-request-id") is not None, "no X-Request-Id")
        raw.parse()

    def streaming():
        text, finish, chunks = "", None, 0
        for chunk in client.chat.completions.create(model="compat-echo", messages=hello, stream=True):
            chunks += 1
            if chunk.choices:
                text += chunk.choices[0].delta.content or ""
                finish = chunk.choices[0].finish_reason or finish
        expect(chunks > 1, f"{chunks} chunk(s)")
        expect(bool(text), "empty streamed text")
        expect(finish == "stop", f"finish_reason {finish!r}")

    def stream_usage():
        usage = None
        for chunk in client.chat.completions.create(
            model="compat-echo", messages=hello, stream=True, stream_options={"include_usage": True}
        ):
            usage = chunk.usage or usage
        expect(usage is not None and usage.total_tokens > 0, f"usage {usage!r}")

    def stream_helper():
        with client.chat.completions.stream(model="compat-echo", messages=hello) as stream:
            final = stream.get_final_completion()
        expect(bool(final.choices[0].message.content), "empty final completion")

    def raises(exc_type, status, code, using=client, **kwargs):
        def run():
            try:
                using.chat.completions.create(messages=hello, **kwargs)
            except exc_type as exc:
                expect(exc.status_code == status, f"status {exc.status_code}, want {status}")
                expect(exc.code == code, f"code {exc.code!r}, want {code!r}")
                return
            raise AssertionError(f"no {exc_type.__name__}")
        return run

    def rate_limited_retry_after():
        try:
            client.chat.completions.create(model="compat-rate-limited", messages=hello)
        except openai.RateLimitError as exc:
            expect(exc.response.headers.get("retry-after") is not None, "no Retry-After")
            return
        raise AssertionError("no RateLimitError")

    bad_client = openai.OpenAI(base_url=BASE + "/v1", api_key="gwk_" + "A" * 43, max_retries=0)

    def error_after_first_chunk_raises():
        got = 0
        try:
            for chunk in client.chat.completions.create(model="compat-break", messages=hello, stream=True):
                got += 1
        except openai.APIError as exc:
            expect(got > 0, "error before any chunk")
            expect(exc.code == "upstream_error", f"code {exc.code!r}, want 'upstream_error'")
            return
        raise AssertionError(f"stream ended cleanly after {got} chunk(s)")

    def streaming_error_before_first_chunk():
        try:
            client.chat.completions.create(model="compat-fail", messages=hello, stream=True)
        except openai.APIStatusError as exc:
            expect(exc.status_code == 502, f"status {exc.status_code}")
            return
        raise AssertionError("no error status on a stream that failed before its first chunk")

    check("non-streaming completion", non_streaming)
    check("models.list names the route aliases", models_list)
    check("raw response headers", raw_headers)
    check("streaming completion", streaming)
    check("streaming with include_usage", stream_usage)
    check("stream helper (get_final_completion)", stream_helper)
    check("unknown model -> NotFoundError", raises(openai.NotFoundError, 404, "model_not_found", model="no-such-route"))
    check("upstream failure -> 502", raises(openai.InternalServerError, 502, "upstream_error", model="compat-fail"))
    check("rate limited -> RateLimitError", raises(openai.RateLimitError, 429, "upstream_rate_limited", model="compat-rate-limited"))
    check("rate limited carries Retry-After", rate_limited_retry_after)
    check("context too long -> 413", raises(openai.APIStatusError, 413, "context_length_exceeded", model="compat-context-too-long"))
    check("bad key -> AuthenticationError", raises(openai.AuthenticationError, 401, "invalid_api_key", using=bad_client, model="compat-echo"))
    check("failure before first chunk is a status, streaming", streaming_error_before_first_chunk)
    check("failure after first chunk raises APIError", error_after_first_chunk_raises)

    print(f"\n{len(failures)} failed" if failures else "\nall passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
