"""Probe the embedding endpoints of the vendors a credential can name.

Run from the repo root: uv run features-specs/phase-2/embeddings/prototype/probe.py

Keys are read from .env.dev (ENV_FILE overrides) and sent only to their own
vendor. Nothing here prints, logs or writes a key: every printed body passes
through redact() first.
"""

import json
import math
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

ENV_FILE = os.environ.get("ENV_FILE", ".env.dev")
N = int(os.environ.get("PROBE_N", "10"))

SENTENCE = "How do I reset my password?"
PARAPHRASE = "What are the steps to change a forgotten password?"
UNRELATED = "The recipe needs two cups of flour and an egg."


def load_env(path):
    env = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    return env


ENV = load_env(ENV_FILE)
SECRETS = [v for k, v in ENV.items() if k.endswith("_API_KEY") and v]


def redact(text):
    for s in SECRETS:
        text = text.replace(s, "<redacted>")
    return text


def call(method, url, headers, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json", "User-Agent": "embedding-probe/1", **headers})
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, raw, (time.perf_counter() - t0) * 1000
    except urllib.error.HTTPError as e:
        return e.code, e.read(), (time.perf_counter() - t0) * 1000
    except (TimeoutError, urllib.error.URLError) as e:
        return 0, f"{type(e).__name__}".encode(), (time.perf_counter() - t0) * 1000


def short(raw, n=300):
    return redact(raw.decode("utf-8", "replace"))[:n].replace("\n", " ")


def norm(v):
    return math.sqrt(sum(x * x for x in v))


def cos(a, b):
    return sum(x * y for x, y in zip(a, b)) / (norm(a) * norm(b))


def openai_embed(base, headers, model, texts, dims=None):
    body = {"model": model, "input": texts}
    if dims:
        body["dimensions"] = dims
    st, raw, ms = call("POST", base + "/embeddings", headers, body)
    if st != 200:
        return st, None, ms, short(raw)
    return st, [d["embedding"] for d in json.loads(raw)["data"]], ms, ""


def report_vectors(label, vecs):
    a, p, u = vecs
    print(f"  {label}: dim={len(a)} norm={norm(a):.4f} cos(paraphrase)={cos(a, p):.4f} cos(unrelated)={cos(a, u):.4f}")


def latency(label, fn):
    times = []
    for _ in range(N):
        st, _, ms, err = fn()
        if st != 200:
            print(f"  {label}: HTTP {st} {err}")
            return
        times.append(ms)
    print(f"  {label}: n={N} p50={statistics.median(times):.0f}ms max={max(times):.0f}ms min={min(times):.0f}ms")


def list_models(label, url, headers, pick):
    st, raw, _ = call("GET", url, headers)
    if st != 200:
        print(f"  {label} models: HTTP {st} {short(raw)}")
        return []
    ids = pick(json.loads(raw))
    print(f"  {label} models matching embed: {ids}")
    return ids


def gemini():
    key = ENV.get("GEMINI_API_KEY")
    if not key:
        return print("gemini: no key")
    print("gemini")
    h = {"x-goog-api-key": key}
    native = "https://generativelanguage.googleapis.com/v1beta"
    list_models("native", native + "/models?pageSize=1000", h,
                lambda j: [m["name"] for m in j.get("models", []) if "embedContent" in m.get("supportedGenerationMethods", [])])
    compat = native + "/openai"
    oh = {"Authorization": "Bearer " + key}
    model = "gemini-embedding-001"
    for dims in (None, 1024, 768):
        st, vecs, ms, err = openai_embed(compat, oh, model, [SENTENCE, PARAPHRASE, UNRELATED], dims)
        if st != 200:
            print(f"  compat {model} dims={dims}: HTTP {st} {err}")
            continue
        report_vectors(f"compat {model} dims={dims or 'default'} ({ms:.0f}ms)", vecs)
    # Native embedContent with outputDimensionality and a task type.
    body = {"model": "models/" + model, "content": {"parts": [{"text": SENTENCE}]}, "outputDimensionality": 1024}
    st, raw, ms = call("POST", f"{native}/models/{model}:embedContent", h, body)
    if st == 200:
        v = json.loads(raw)["embedding"]["values"]
        print(f"  native embedContent dims=1024: dim={len(v)} norm={norm(v):.4f} ({ms:.0f}ms)")
    else:
        print(f"  native embedContent: HTTP {st} {short(raw)}")
    latency(f"compat {model} 1024 one sentence", lambda: openai_embed(compat, oh, model, [SENTENCE], 1024))
    # The native API takes a task type, which the compatible endpoint cannot.
    for task in os.environ.get("GEMINI_TASKS", "SEMANTIC_SIMILARITY").split(","):
        if not task:
            continue
        body = {"requests": [{"model": "models/" + model, "content": {"parts": [{"text": t}]},
                              "outputDimensionality": 1024, "taskType": task} for t in (SENTENCE, PARAPHRASE, UNRELATED)]}
        st, raw, ms = call("POST", f"{native}/models/{model}:batchEmbedContents", h, body)
        if st == 200:
            report_vectors(f"native {task} 1024 ({ms:.0f}ms)", [e["values"] for e in json.loads(raw)["embeddings"]])
        else:
            print(f"  native {task}: HTTP {st} {short(raw)}")
    # Any newer embedding model the list shows can be probed by name.
    for extra in os.environ.get("GEMINI_EXTRA", "").split(","):
        if not extra:
            continue
        for dims in (None, 1024):
            st, vecs, ms, err = openai_embed(compat, oh, extra, [SENTENCE, PARAPHRASE, UNRELATED], dims)
            if st != 200:
                print(f"  compat {extra} dims={dims}: HTTP {st} {err}")
                continue
            report_vectors(f"compat {extra} dims={dims or 'default'} ({ms:.0f}ms)", vecs)
        latency(f"compat {extra} 1024 one sentence", lambda: openai_embed(compat, oh, extra, [SENTENCE], 1024))


def plain_openai(label, envname, base, guess_models, models_path="/models"):
    key = ENV.get(envname)
    if not key:
        return print(f"{label}: no key")
    print(label)
    h = {"Authorization": "Bearer " + key}
    found = list_models(label, base + models_path, h,
                        lambda j: [m["id"] for m in j.get("data", []) if "embed" in m["id"].lower()
                                   or "embeddings" in json.dumps(m.get("architecture", {})).lower()])
    for model in (found[:2] or guess_models):
        st, vecs, ms, err = openai_embed(base, h, model, [SENTENCE, PARAPHRASE, UNRELATED])
        if st != 200:
            print(f"  POST /embeddings {model}: HTTP {st} {err}")
            continue
        report_vectors(f"{model} ({ms:.0f}ms)", vecs)
        st, vecs, ms, err = openai_embed(base, h, model, [SENTENCE, PARAPHRASE, UNRELATED], 512)
        print(f"  {model} dims=512: HTTP {st} {err}" if st != 200 else "", end="")
        if st == 200:
            report_vectors(f"{model} dims=512", vecs)
        latency(f"{model} one sentence", lambda: openai_embed(base, h, model, [SENTENCE]))


def openrouter():
    key = ENV.get("OPENROUTER_API_KEY")
    if not key:
        return print("openrouter: no key")
    print("openrouter")
    h = {"Authorization": "Bearer " + key}
    base = "https://openrouter.ai/api/v1"
    st, raw, _ = call("GET", base + "/embeddings/models", h)
    ids = []
    if st == 200:
        data = json.loads(raw).get("data", [])
        free = [m["id"] for m in data if str(m.get("pricing", {}).get("prompt", "1")) in ("0", "0.0")]
        ids = [m["id"] for m in data]
        print(f"  /embeddings/models: {len(ids)} models, free: {free}")
        print(f"  sample: {ids[:40]}")
    else:
        print(f"  /embeddings/models: HTTP {st} {short(raw)}")
    for model in os.environ.get("OPENROUTER_EMBED", "").split(","):
        if not model:
            continue
        for dims in (None, 1024):
            st, vecs, ms, err = openai_embed(base, h, model, [SENTENCE, PARAPHRASE, UNRELATED], dims)
            if st != 200:
                print(f"  {model} dims={dims}: HTTP {st} {err}")
                continue
            report_vectors(f"{model} dims={dims or 'default'} ({ms:.0f}ms)", vecs)
        latency(f"{model} one sentence", lambda: openai_embed(base, h, model, [SENTENCE]))


def crosscheck():
    """Embed the same texts on two endpoints of one family and compare.

    Same family means the vector for one text is (nearly) the same wherever
    it was made, so cos(endpoint A, endpoint B) for one text is close to 1.
    """
    print("crosscheck")
    texts = [SENTENCE, PARAPHRASE, UNRELATED]
    orh = {"Authorization": "Bearer " + ENV["OPENROUTER_API_KEY"]}
    orb = "https://openrouter.ai/api/v1"
    gh = {"Authorization": "Bearer " + ENV["GEMINI_API_KEY"]}
    gb = "https://generativelanguage.googleapis.com/v1beta/openai"
    pairs = [("gemini-embedding-2 gemini vs openrouter",
              (gb, gh, "gemini-embedding-2"), (orb, orh, "google/gemini-embedding-2"), 1024)]
    local = os.environ.get("LOCAL_EMBED_URL")  # e.g. a TEI container's http://127.0.0.1:18090/v1
    if local:
        pairs.append(("bge-m3 openrouter vs local", (orb, orh, "baai/bge-m3"), (local, {}, "BAAI/bge-m3"), None))
        pairs.append(("bge-m3 local vs local", (local, {}, "BAAI/bge-m3"), (local, {}, "BAAI/bge-m3"), None))
    for label, (b1, h1, m1), (b2, h2, m2), dims in pairs:
        s1, v1, _, e1 = openai_embed(b1, h1, m1, texts, dims)
        s2, v2, _, e2 = openai_embed(b2, h2, m2, texts, dims)
        if s1 != 200 or s2 != 200:
            print(f"  {label}: HTTP {s1} {e1} / {s2} {e2}")
            continue
        print(f"  {label}: same-text cos = {', '.join(f'{cos(a, b):.5f}' for a, b in zip(v1, v2))}")
    if local:
        latency("local bge-m3 one sentence", lambda: openai_embed(local, {}, "BAAI/bge-m3", [SENTENCE]))


if __name__ == "__main__":
    only = set(sys.argv[1:])
    if "crosscheck" in only:
        crosscheck()
        sys.exit()
    if not only or "gemini" in only:
        gemini()
    if not only or "groq" in only:
        plain_openai("groq", "GROQ_API_KEY", "https://api.groq.com/openai/v1", ["nomic-embed-text-v1_5"])
    if not only or "cerebras" in only:
        # The repo's .env.dev names the Cerebras key CEREBRAL_API_KEY.
        plain_openai("cerebras", "CEREBRAL_API_KEY", "https://api.cerebras.ai/v1", ["text-embedding-3-small"])
    if not only or "openrouter" in only:
        openrouter()
