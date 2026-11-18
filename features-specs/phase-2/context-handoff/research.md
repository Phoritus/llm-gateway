# Context handoff: research

Research for ticket 06 ([issues/06-context-handoff-design.md](../issues/06-context-handoff-design.md)).
Sources were read on 2026-11-18. Source code is cited by commit, so the line numbers stay valid.
"Unverified" marks every claim I could not trace to a primary source.

## Summary

- No gateway does what the owner asks for in one step. **LiteLLM** fails over to a bigger model and never trims inside the router. **OpenRouter** trims the middle of the conversation and also prefers a bigger model.
- The good trimming code lives in clients: Cline, Continue, Roo Code and Aider. Each one trims or summarises before it sends a request, and each one needs the model's real context window to do it. Behind a gateway alias they don't know that window. Claude Code's docs say the same thing about gateway aliases.
- Server-side compaction from Anthropic and OpenAI is tied to one vendor. Anthropic's summary block is signed and OpenAI's compaction item is encrypted. Neither carries over to another vendor's model, and both need a model whose window still fits the whole conversation.
- Recommendation, per target and before the first byte:
  1. Prefer a target whose known window fits.
  2. Otherwise fit the conversation to the target with a deterministic, pair-safe trim: keep the system prompt, the first user turn and the newest turns, and drop or shorten from the middle.
  3. Optionally, summarise with a route target, off by default.
- Trigger handoff before the call, from an estimate against a known window, and again after an `ErrContextTooLong`. Learn windows from the error text, which states the limit. Always say so in an `X-Context-Handoff` header. Make it opt-in per route.
- Store no prompt text. The spec's `sess:{workspace}:{session}` key is not needed for any of this.

---

## 1. LiteLLM

**Context-window fallbacks are routing only.**
- A `context_window_fallbacks: [{"small-model": ["large-model", ...]}]` rule sends a request that hit `ContextWindowExceededError` to another model group. A matching chain takes priority over generic `fallbacks`. If the chain is exhausted, the request "returns an error rather than continuing" ([docs: reliability](https://docs.litellm.ai/docs/proxy/reliability)).
- The prompt is not modified. The docs page mentions no trimming.
- With `enable_pre_call_checks: true`, the router checks the input against each deployment's declared input limit before it calls the provider, and filters out deployments that are too small. Without that setting, "requests are sent to the provider regardless of input token count" (same page).
- Limits come from LiteLLM's model list or from a per-deployment `model_info.max_input_tokens` override (same page).

**How it knows a request was too long.**
`is_error_str_context_window_exceeded` matches substrings in the provider's error message ([exception_mapping_utils.py L88-127 @4916d56](https://github.com/BerriAI/litellm/blob/4916d5636a18f3494e83866545d21a349b0556d4/litellm/litellm_core_utils/exception_mapping_utils.py#L88-L127)). The substrings:
- `"this model's maximum context length is"` (OpenAI)
- `"exceeds the maximum number of tokens allowed"` (Gemini)
- `"prompt is too long"` (Anthropic)
- `'"code": "context_length_exceeded"'`
- Cerebras: `"current length is … while limit is"`
- a few others

This is the same approach our `provider/openai` `aboutContextLength` takes.

**`trim_messages` is a separate utility. The router does not call it.**
Source: [utils.py L8976-9200 @4916d56](https://github.com/BerriAI/litellm/blob/4916d5636a18f3494e83866545d21a349b0556d4/litellm/utils.py#L8976-L9200). The code credits Killian Lucas's `tokentrim`.

What it does:
- It targets `trim_ratio × max_input_tokens`. The ratio is `DEFAULT_TRIM_RATIO = 0.75` ([constants.py L542](https://github.com/BerriAI/litellm/blob/4916d5636a18f3494e83866545d21a349b0556d4/litellm/constants.py#L542)).
- It merges every system message into one. Trailing `tool` messages are set aside and appended back unchanged.
- It walks the rest from newest to oldest and adds each message while the total fits.
- A message that does not fit is shortened by cutting characters out of its middle (`left + ".." + right`), for up to `MAX_TOKEN_TRIMMING_ATTEMPTS = 10` rounds. This is `shorten_message_to_fit_limit` at L9046.
- If the model is unknown, it returns the messages unchanged. On any exception, it returns the original messages.

Hazards I found by reading the code (not by running it):
- The walk does not stop at the first message that doesn't fit. It keeps adding older, smaller messages, so the result can have gaps in the middle.
- Tool pairs are only protected at the tail. A `tool` message in the middle can survive while the assistant `tool_calls` message it answers is dropped.
- The shortening check looks for the legacy `function_call` key, not `tool_calls`.
- `process_system_message` computes a shortened system message, then returns the original `system_message_event`, so the shortened version is thrown away (L8976-8987).
- Content arrays are sliced as if they were strings, so multimodal content is mishandled.

**Licence:** MIT, except `litellm/enterprise/` ([LICENSE](https://github.com/BerriAI/litellm/blob/4916d5636a18f3494e83866545d21a349b0556d4/LICENSE)).

## 2. OpenRouter: middle-out ("context compression")

The feature is now a plugin, `plugins: [{ id: "context-compression" }]`. The plugins table describes it as "middle-out truncation" ([message-transforms](https://openrouter.ai/docs/guides/features/message-transforms.md), [plugins](https://openrouter.ai/docs/guides/features/plugins.md)). The older `transforms: ["middle-out"]` form appears in search results, and I could not confirm it is still accepted (unverified).

What it does, per the docs:
- It removes or truncates messages "from the middle of the prompt, until the prompt fits". The reason given is that LLMs attend less to the middle of a sequence ([Liu et al., "Lost in the Middle"](https://arxiv.org/abs/2307.03172)).
- When the problem is the number of messages, it keeps half the messages from the start and half from the end. The docs give Anthropic's limit as 100,000 messages.
- It first looks for models whose context length is at least half of the total required tokens (input plus completion). If none qualify, it uses the model with the largest context length. So it combines picking a bigger model with trimming.
- It is **on by default for every endpoint with 8,192 tokens of context or less**. To turn it off, send `plugins: [{"id": "context-compression", "enabled": false}]`.
- It is skipped for image-only output models because "Compression would otherwise truncate multipart message content and drop input `image_url` parts". So it can drop images on every other model.

The page does not say how tool-call pairs are protected (unverified). `GET /api/v1/models` returns `context_length` and `top_provider.context_length` for each model. A live call on 2026-11-18 returned 478 models.

## 3. Anthropic: Claude Code, context editing, server-side compaction

**Claude Code** (client side)
- It compacts automatically as the limit approaches. `/compact <focus>` steers the summary.
- After compaction it re-injects CLAUDE.md, memory and up to five recently read files from disk ([context-window](https://code.claude.com/docs/en/context-window)).
- If one huge tool output refills the context after every summary, it "stops auto-compacting after a few attempts" ([how-claude-code-works](https://code.claude.com/docs/en/how-claude-code-works.md)).
- What matters for us: an ID it does not recognise, "such as an LLM gateway alias", gets an assumed window. Behind a gateway, Claude Code "can't detect a lower limit that the gateway or the server behind it enforces". The user has to set `CLAUDE_CODE_AUTO_COMPACT_WINDOW` or `CLAUDE_CODE_MAX_CONTEXT_TOKENS` ([model-config](https://code.claude.com/docs/en/model-config.md)).
- Claude Code speaks the Anthropic Messages API, so it is not a client of this gateway's `/v1` today. It shows the general problem: a client cannot fit to a window it does not know.

**Context editing** (beta `context-management-2025-06-27`)
Source: [context-editing](https://platform.claude.com/docs/en/build-with-claude/context-editing).
- `clear_tool_uses_20250919` replaces the oldest tool results with placeholder text. Defaults: `trigger` at 100,000 input tokens, `keep` 3 tool uses. Options: `clear_at_least`, `exclude_tools`, and `clear_tool_inputs` (default false).
- `clear_thinking_20251015` clears thinking blocks.
- It is "applied server-side before the prompt reaches Claude. Your client application maintains the full, unmodified conversation history". In other words, it is stateless.
- The response reports `context_management.applied_edits` with `cleared_input_tokens`. Clearing invalidates the prompt cache from the cleared point, which is why `clear_at_least` exists.
- The useful idea for us: **clearing old tool results with a placeholder is a strong, cheap, deterministic first cut**, because tool output dominates agent transcripts.

**Server-side compaction**
- At a threshold: beta `compact-2026-01-12`, edit `compact_20260112`. The default trigger is 150,000 input tokens and the minimum is 50,000. `instructions` replaces the default prompt entirely. The response carries a `compaction` block with readable text, and "all content blocks before it are ignored" on later requests ([compaction-threshold](https://platform.claude.com/docs/en/build-with-claude/compaction-threshold)).
- On demand: beta `compact-2026-09-04`, `"compaction": {"type": "summarize"}`. It returns a block with readable text and a `signature`. Changing the block fails with `compaction_signature_invalid` or `compaction_content_mismatch`. Summarisation is billed like any request ([compaction-on-demand](https://platform.claude.com/docs/en/build-with-claude/compaction-on-demand)).
- Both work only on Anthropic models, and the client has to carry the block forward. An OpenAI-format client talking to this gateway cannot do that, so neither can serve cross-vendor handoff. The default summary prompt is published on the threshold page and is a good template if we ever summarise.

## 4. OpenAI: Responses API

- **`truncation`**: `"auto"` or `"disabled"`, default `"disabled"`. `auto` drops "items from the beginning of the conversation". `disabled` fails with a 400. On some models, trimming starts earlier because the API subtracts the model's maximum output allowance ([Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create.md)). It is plain head-drop, with no protection for the system prompt or the first turn documented there.
- **Server-side compaction**: `context_management: [{type: "compaction", compact_threshold: N}]` on `POST /responses`. It emits a compaction item that is "opaque and not intended to be human-interpretable" (encrypted). It is ZDR-friendly with `store=false` ([compaction guide](https://developers.openai.com/api/docs/guides/compaction.md)).
- **Standalone `/responses/compact`**: "fully stateless". You send the full window and get back a compacted window to use as-is. "The window you send to `/responses/compact` must still fit within your model's context window" (same guide).
- None of this exists on Chat Completions, which is the surface this gateway serves and translates. The items cannot be read by another vendor.
- **Error text on Chat Completions**: `"This model's maximum context length is 4096 tokens. However, your messages resulted in 10007 tokens…"`, with code `context_length_exceeded`. I found this in LiteLLM's test fixture ([test_proxy_server_exception_mapping.py L26](https://github.com/BerriAI/litellm/blob/main/tests/unit/proxy/test_proxy_server_exception_mapping.py#L26)), not in an OpenAI document (unverified primary). The message states the limit and the actual size.
- **`GET /v1/models`** returns only `id`, `created`, `object`, `owned_by` and `shutdown_date`, with no window ([models list](https://developers.openai.com/api/reference/resources/models/methods/list.md)).

## 5. IDE clients

### Cline
Source: `sdk/packages/core/src/extensions/context/` [@eff43e1](https://github.com/cline/cline/tree/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/core/src/extensions/context). Licence: Apache-2.0.

- **Two strategies.** `agentic-compaction.ts` summarises with a model. `basic-compaction.ts` is deterministic and uses no summariser model.
- **Recovery uses the deterministic one.** On a provider-rejected overflow, `overflowRecovery` forces compaction with the basic strategy: "recovery must not depend on another successful LLM request" ([compaction.ts L46-55](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/core/src/extensions/context/compaction.ts#L46-L55), [L305](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/core/src/extensions/context/compaction.ts#L305)).
- **What `runBasicCompaction` keeps** ([basic-compaction.ts L443-452](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/core/src/extensions/context/basic-compaction.ts#L443-L452)):
  - "Typed user prompts always survive."
  - The latest turn keeps its newest messages within the target. The kept suffix "starts at an assistant message so no tool pair is split".
  - Older turns keep only their final assistant answer.
  - Dropped tool work becomes a `<SYSTEM_NOTICE>Earlier context was compacted…` block listing files read and edited and commands run.
  - Messages kept by an earlier compaction are "frozen", so repeated passes do not re-fold them.
- **Budgets** ([compaction-shared.ts L15-37](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/core/src/extensions/context/compaction-shared.ts#L15-L37)):
  - Usable input is 0.9 of the window. Compaction triggers at 0.9 of that and targets 0.7.
  - It preserves the most recent 20,000 tokens and caps a tool result at 2,000 characters.
  - The estimate is 3 characters per token and 1,600 tokens per image ([tokens.ts](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/shared/src/llms/tokens.ts)).
  - The comments note that dense content tokenises 2-3× denser than the estimate, so the provider's reported input count is used as a floor.
- **Recognising a context error from a gateway** ([error-classification.ts L8-36](https://github.com/cline/cline/blob/eff43e19b267e171f597ab03733bfed25090f72d/sdk/packages/llms/src/providers/error-classification.ts#L8-L36)): the code `context_length_exceeded` at any status, or a message pattern at status 400, 413 or 422. Our gateway's 413 with `code: "context_length_exceeded"` therefore triggers Cline's own recovery.
- **Not portable to a gateway as-is.** The tool-activity summary depends on Cline's own tool names (`read_files`, `editor`, `run_commands`). A gateway can only describe dropped tool calls generically.

### Roo Code
Source [@b867ec9](https://github.com/RooCodeInc/Roo-Code/tree/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16). Licence: Apache-2.0.

- **Sliding window** ([src/core/context-management/index.ts L65-129](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/context-management/index.ts#L65-L129)):
  - It keeps the first message and hides a fraction of the rest. The count is rounded down to an even number so user/assistant pairs stay aligned.
  - It inserts a marker, `[Sliding window truncation: N messages hidden to reduce context]`.
  - It is non-destructive in client storage.
  - `manageContext` tries condensing first when it is on, and falls back to dropping 50% ([L243-371](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/context-management/index.ts#L243-L371)). The budget is `window × 0.9 − max output`.
- **Condense** ([src/core/condense/index.ts](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/condense/index.ts)):
  - It summarises everything into one user message (a "fresh start").
  - It first injects synthetic `tool_result`s for orphan `tool_use`s, because some APIs reject them (L124-170).
  - It converts tool blocks to text so the summariser needs no `tools` parameter (L94-105, L299-310).
  - It strips images for models without vision.
- **On a context error** it forces truncation to 75%, up to 3 retries ([Task.ts L134-135](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/task/Task.ts#L134-L135)).
- **Error detection** ([context-error-handling.ts](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/context/context-management/context-error-handling.ts)): the OpenRouter-style check requires `status == 400`. The OpenAI check compares `error.code` to `"400"`. From reading the code, I believe **our 413 would not be recognised**, so Roo would not self-truncate (not tested).
- **Learning windows from a gateway.** Roo's LiteLLM provider reads `GET /v1/model/info` and uses `model_info.max_input_tokens` as the window ([fetchers/litellm.ts L28-45](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/api/providers/fetchers/litellm.ts#L28-L45)).

### Continue
Source [@5522c6f](https://github.com/continuedev/continue/tree/5522c6f44ca0ac3528b37244818fbfa39b5af470). Licence: Apache-2.0.

`compileChatMessages` ([core/llm/countTokens.ts L392-550](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/llm/countTokens.ts#L392-L550)) is the closest match to what a gateway needs, and it is short:
- It always keeps the system message, the tool definitions, and the final sequence. That sequence is the last user message, or the assistant `tool_calls` message plus every tool response, with IDs validated in `extractToolSequence` (L228-279).
- It drops the oldest messages first. After each drop it also drops any leading `tool` messages, so "never allow orphaned tool responses".
- It reserves `min(1000, max_tokens)` for output, plus a safety buffer of `min(1000, 2% of window)`.
- It throws when the parts it must keep don't fit.
- It counts images at 1,024 tokens.
- When the window is unknown, it assumes `DEFAULT_PRUNING_LENGTH = 128000` ([constants.ts](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/llm/constants.ts)). That is wrong for a gateway alias in front of an 8k free model.

There is also a user-triggered summary, `compactConversation` ([core/util/conversationCompaction.ts](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/util/conversationCompaction.ts)).

### Aider
Source: `ChatSummary` ([aider/history.py @5dc9490](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/history.py)). Licence: Apache-2.0.

- It splits the history so the tail is about half the budget, and moves the split back so the head ends on an assistant message.
- It summarises the head into one user message. The summariser is the first model in its list, which Aider sets to its "weak" model (unverified for current versions).
- It recurses up to depth 3.
- Only `user` and `assistant` text goes into the summary.
- The prompt ([prompts.py `summarize`](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/prompts.py)) asks the model to keep function, library and file names and to write in the first person as the user.

## 6. LangChain / LangGraph and LLMLingua

**LangChain `trim_messages`** ([langchain_core/messages/utils.py L1146 @6bd561c](https://github.com/langchain-ai/langchain/blob/6bd561c897b3c912ba3096b38b8201d265293f94/libs/core/langchain_core/messages/utils.py#L1146)). Licence: MIT.
- Options: `strategy="last"`, `include_system`, `start_on="human"`, `end_on`, and `allow_partial`.
- The docstring states the rules: start with a human message (after an optional system message), and a `ToolMessage` may only follow an `AIMessage` with a tool call.
- `count_tokens_approximately` ([L2262](https://github.com/langchain-ai/langchain/blob/6bd561c897b3c912ba3096b38b8201d265293f94/libs/core/langchain_core/messages/utils.py#L2262)) uses 4 characters per token plus 3 tokens per message.

**`SummarizationMiddleware`** ([langchain/agents/middleware/summarization.py](https://github.com/langchain-ai/langchain/blob/6bd561c897b3c912ba3096b38b8201d265293f94/libs/langchain_v1/langchain/agents/middleware/summarization.py)).
- `trigger` takes tokens, messages, or a fraction of the model profile's `max_input_tokens`.
- `keep` defaults to 20 messages, and it summarises at most 4,000 tokens (L113-114).
- `_find_safe_cutoff_point` moves the cut so that an AI message and its tool messages are never split (L862-905).

**LLMLingua / LongLLMLingua / LLMLingua-2** ([microsoft/LLMLingua](https://github.com/microsoft/LLMLingua)). Licence: MIT.
- It removes "non-essential tokens" using a small language model, "up to 20x compression".
- LLMLingua-2 is a BERT-level token classifier, for example `microsoft/llmlingua-2-xlm-roberta-large-meetingbank`.
- It is Python-only and needs `torch` and `transformers` ([setup.py](https://github.com/microsoft/LLMLingua/blob/main/setup.py)).
- **Feasibility from Go:** possible only as a Python sidecar with a model on CPU or GPU. That does not fit Phase 1's free tier, and for Phase 2 it adds a service.
- It rewrites text below the message level, so it can break code and JSON tool arguments. Its examples guard this with `force_tokens`.
- Not recommended for an IDE gateway (my judgement, not tested).

## 7. Token counting without an exact Go tokenizer

**Go tokenizers**

| Option | Licence and maintenance | Notes |
| --- | --- | --- |
| [pkoukk/tiktoken-go](https://github.com/pkoukk/tiktoken-go) | MIT, last push 2026-05-12 | Downloads the BPE files at runtime unless you set `TIKTOKEN_CACHE_DIR` or use the separate offline loader |
| [tiktoken-go/tokenizer](https://github.com/tiktoken-go/tokenizer) | MIT, last push 2026-10-21 | Pure Go. Embeds `cl100k`/`o200k` vocabularies as Go source (`codec/o200k_base_vocab.go` is about 7.9 MB), so the binary grows. Special tokens are not handled (its README TODO) |
| [daulet/tokenizers](https://github.com/daulet/tokenizers) | MIT | cgo bindings to HuggingFace tokenizers, a Rust static library. Needs a `tokenizer.json` per model family |

- Only OpenAI models use tiktoken. Llama, Qwen, Gemini, Mistral and Claude each tokenise differently.
- Our `bytes/4` estimate (`internal/gateway/limit.go` `estimateTokens`) is as good as any single tokenizer across this vendor mix, once a margin is added.

**Vendor count endpoints**
- Anthropic `POST /v1/messages/count_tokens` is free, with its own RPM limit, and returns an estimate ([token-counting](https://platform.claude.com/docs/en/build-with-claude/token-counting.md)).
- Gemini `models/{m}:countTokens` exists, and `models.get` returns `inputTokenLimit` and `outputTokenLimit` ([tokens](https://ai.google.dev/gemini-api/docs/tokens.md), [models API](https://ai.google.dev/api/models.md)). Gemini's pricing and quota for counting are unverified.
- Each count is a round trip before the first byte, so use one only near the boundary.

**Limits stated in errors.** These let the gateway learn a window at runtime:

| Vendor | Error text | Source |
| --- | --- | --- |
| OpenAI | "maximum context length is **N** tokens. However, your messages resulted in **M**" | §4, fixture |
| Anthropic | "prompt is too long: **M** tokens > **N** maximum" | our `anthropic_test.go` L306 |
| Gemini | "The input token count (**M**) exceeds the maximum number of tokens allowed (**N**)" | our `openai_test.go` L173 |
| Cerebras | "Current length is **M** while limit is **N**" | LiteLLM's matcher |

The pair (M, N) gives both the window and how wrong our estimate was for this request: M against `bytes/4`.

## 8. Where context-window sizes come from

| Source | Field | Caveat |
| --- | --- | --- |
| OpenRouter `GET /api/v1/models` | `context_length`, `top_provider.context_length` | Live and public, no key. Covers OpenRouter's catalogue |
| LiteLLM [model_prices_and_context_window.json](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) | `max_input_tokens`, `max_output_tokens` | MIT, 6,221 entries on 2026-11-18. **Has gaps:** no `groq/llama-3.3-70b-versatile` entry. **Ignores tiers:** `cerebras/llama3.1-8b` says 128,000 |
| Groq `GET /openai/v1/models` | `context_window` | Per model ([API reference](https://console.groq.com/docs/api-reference.md)) |
| Mistral `GET /v1/models` | `max_context_length` | Per model ([models endpoint](https://docs.mistral.ai/api/endpoint/models)) |
| Gemini `models.get` | `inputTokenLimit` | Native API. Whether the OpenAI-compatible `/models` returns it is unverified |
| Anthropic `GET /v1/models` | `max_input_tokens`, `max_tokens` | ([models list](https://platform.claude.com/docs/en/api/models/list.md)) |
| OpenAI `GET /v1/models` | none | Needs a catalogue |

**The window depends on the account's tier, not only on the model.** Cerebras documents `gpt-oss-120b` at "65k tokens (free tier), 131k tokens (paid)" ([openai-oss](https://inference-docs.cerebras.ai/models/openai-oss.md)). Third-party pages claim an 8k free-tier limit on other Cerebras models (unverified). So a catalogue value is an upper bound, and the window the gateway should trust belongs to the **credential and model** pair. This gateway already keys quota state at that granularity.

## 9. Correctness hazards when trimming

- **Tool pairs.**
  - Anthropic: "Tool result blocks must immediately follow their corresponding tool use blocks", and the `tool_result` blocks must come first in the user message ([handle-tool-calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls.md)).
  - OpenAI-format: a `tool` message must follow an assistant message whose `tool_calls` includes its `tool_call_id` (LangChain docstring; Continue's `extractToolSequence`). I did not find OpenAI's own wording (unverified primary).
  - Our `provider/anthropic` adapter already turns tool messages into `tool_result` blocks, so a broken pair fails at the vendor with a 400.
  - **The unit of trimming must be the assistant message plus all its tool responses**, never one message.
- **System and developer prompts** must be kept: LiteLLM, Continue, LangChain `include_system`. If they alone don't fit, refuse rather than trim them.
- **The last user turn** must be kept, including the current tool sequence (Continue). If it doesn't fit, return an honest error.
- **The first user turn** often holds the task. Roo keeps the first message, OpenRouter keeps the head, Cline keeps all typed prompts. OpenAI's `truncation: auto` drops it, which is the weakest choice.
- **Images**: OpenRouter's compression truncates multipart content and can drop `image_url` parts. Roo and Continue strip images for models without vision. Count an image at a fixed estimate (Continue 1,024, Cline 1,600) and never cut inside a content array.
- **Role alternation.** Dropping from the middle can leave two user messages in a row. Our Anthropic adapter merges same-role turns. OpenAI-format vendors usually accept them (unverified per vendor). A marker message keeps the transcript readable.
- **Placeholders, not silent gaps.** Roo inserts a truncation marker, Anthropic's context editing uses placeholder text, Cline uses a `SYSTEM_NOTICE`. The model should know something was removed.
- **Prompt-cache stability.** Any edit invalidates the vendor's prefix cache from that point (Anthropic context-editing docs). Cut points that move every turn defeat caching. Cline "freezes" earlier compaction output for this reason.
- **Estimates are wrong for dense content.** Cline caps the underestimate at 4× and floors with the reported input. A gateway trim needs a margin and one retry with a tighter budget.

---

## Comparison

| System | Where it runs | Trigger | Strategy | Keeps tool pairs | Stateless | Cross-vendor |
| --- | --- | --- | --- | --- | --- | --- |
| LiteLLM `context_window_fallbacks` | Gateway | Error, or pre-call check | Route to another model; no trimming | n/a | Yes | Yes |
| LiteLLM `trim_messages` | Library | Caller | Newest-first fill, middle-cut long messages | Tail only (buggy) | Yes | Yes |
| OpenRouter context compression | Gateway | Opt-in; default when window ≤ 8k | Bigger model, then drop or truncate the middle | Undocumented | Yes | Yes |
| Anthropic context editing | Vendor | Token threshold | Replace old tool results with placeholders | Yes | Yes | Anthropic only |
| Anthropic compaction | Vendor | Threshold or on demand | Model summary (signed block) | Yes | No, client carries the block | Anthropic only |
| OpenAI `truncation: auto` | Vendor | Overflow | Drop from the start | Undocumented | Yes | OpenAI only |
| OpenAI compaction | Vendor | Threshold or `/responses/compact` | Encrypted compaction item | Yes | No, client carries the item | OpenAI only |
| Cline basic / agentic | Client | 0.9 of budget, or overflow | Deterministic fold / model summary | Yes | No, client state | n/a |
| Roo Code | Client | Threshold, or error (75%) | Summary, else drop 50% after the first message | Synthetic results | No | n/a |
| Continue | Client | Every request | Drop oldest; keep system, tools, last sequence | Yes | Yes | n/a |
| Aider | Client | History budget | Recursive head summary with weak model | Text only | No | n/a |
| LangChain trim / summarise | Library | Caller / thresholds | Last-N with `start_on` / summary | Yes (safe cutoff) | Yes / No | n/a |
| LLMLingua | Python + model | Caller | Token-level deletion | No | Yes | n/a |

---

## Recommendation for this gateway

### What handoff is

Handoff is a per-target transform inside the router's existing pre-first-byte loop. Today `router.Run` calls `attempt(ctx, t)` with the same request for every target, and an `ErrContextTooLong` marks `tooLong[t.Model]`. Handoff makes the request that each target receives **fit that target**. Two facts make this legal and cheap:

- Failover is already allowed until the first chunk, so giving another target a modified request is a failover with a different body.
- Chat Completions is stateless: every request carries the whole conversation. The gateway can re-derive the fitted form every turn, so it needs no memory. This answers ticket 06's first question: what breaks today is the failover case, not clients that send only the newest message.

### Strategy order, per request

1. **Order fitting targets first.** If the conversation's estimate exceeds a target's known window, move that target behind those that fit. Use a stable reorder, as `quota.Prioritize` already does, rather than dropping it.
   - This is LiteLLM's pre-call check and the first half of OpenRouter's compression, and it loses nothing.
   - It applies to every route, even with handoff off. Skipping a target that is certain to fail saves a hop and the vendor's 400.
2. **Fit deterministically when the chosen target is too small.** This applies only on routes that opt in. Port Continue's `compileChatMessages`, with three of Cline's ideas added:
   - Keep every `system` and `developer` message, the tool definitions, the **first user message**, and the **final sequence**: the last user message, or the assistant `tool_calls` message with all its tool messages.
   - First, replace older tool results with a short placeholder, oldest first, keeping the newest 3. This is Anthropic's `clear_tool_uses` default. Tool output is usually most of the bulk, and the turn structure stays intact.
   - Then drop whole units (a user message, an assistant message, or an assistant `tool_calls` message plus its tool messages) from the middle, oldest first, until the estimate fits. Insert one marker message where the gap is, for example `[context handoff: 14 earlier messages omitted to fit <alias>'s window]`.
   - Never cut inside a content array, never split a pair, and never touch the system prompt or the last turn. If what must be kept does not fit, the honest answer is the existing context error.
   - Cut in coarse steps, for example quarters of the middle, so the cut point stays put across turns and the vendor's prefix cache survives longer.
3. **Summarise, optionally.** This is off by default and would be a second opt-in. Use a route target to summarise the dropped middle with a fixed prompt (Anthropic's default compaction prompt or Aider's). Do it only when step 2 would drop more than a set share.
   - It costs one extra upstream call before the first byte, inside the 45s budget, spends the user's quota, and can fail.
   - Cline's rule applies: recovery must not depend on another LLM call succeeding. So summarisation can improve step 2 but must never be required for it.
   - Stateless, it re-summarises every turn. Avoiding that means caching the summary, which stores derived prompt text.
   - I recommend leaving it out of ticket 07 and measuring step 2 first.

Do not use LLMLingua, vendor compaction blocks, or OpenAI `truncation` for this: they are Python-only, tied to one vendor, or only drop from the head.

### Triggers

- **Pre-flight**, when a window is known for the target's credential and model: compare `estimate × margin` against `window − reserved output`.
  - The estimate is today's `bytes/4` plus a fixed count per image.
  - Reserved output is the request's `max_tokens`, else `min(1000, …)` as Continue does.
  - Use the margin in both directions: a margin of 1.25 or more against the window, and the 0.9 usable share that Cline and Roo apply.
- **On `ErrContextTooLong`** from an attempt: parse M and N from the error text (§7). Record the learned window N. If handoff is on, retry that **same target once** with the request fitted to N, using the measured ratio M/estimate to scale. If that fails, move to the next target.
  - Each retry counts toward `MaxHops`. The trail shows it as a distinct outcome, for example `context_fitted`.

### Where windows come from

Use, in order of trust:
1. A window learned from this credential and model's own error.
2. A per-target override on the route, like `first_byte_timeout_seconds`.
3. A catalogue column next to `free_quota_catalog`.
4. Unknown. Then pre-flight is skipped and only the error trigger applies.

Learned windows go in Redis with a TTL, keyed like the bench (`ctxwin:{credential}:{model}`, for example a 7-day TTL). They hold numbers, not prompt text, so they fit the "shared state lives in Redis with a TTL" invariant and raise no privacy issue.

To seed the catalogue, prefer importing OpenRouter's `context_length` and LiteLLM's JSON (MIT) offline into a migration or reload job, over calling them at runtime. Treat their numbers as upper bounds, because of the tier gap in §8.

### What it must never do silently

- Set `X-Context-Handoff` on every response where a request was changed or its targets reordered by window. Example: `X-Context-Handoff: trimmed; omitted=14; cleared_tool_results=6; target=<vendor>`. Use only values from fixed sets, so it can also be a metric label (`gateway.context_handoff` by `action` = `reordered | trimmed | summarized | refused`) without exceeding the series budget.
- Record it in `X-Fallback-Trail` and on the usage event, for example `handoff` = the action and `omitted_messages`. No text.
- **Never cache an answer produced from a trimmed request** under the original request's key. That answer responds to a different prompt. `cache.ResponseCacheable` is the place to enforce this.
- Never change requests on a route that has not opted in.

### Opt-in and config

- A per-route setting next to `free_first` and `cache`, for example `context_handoff: off | reorder | trim`, default `off`. Reordering by window could reasonably be on for everyone.
- `CONTEXT_HANDOFF` stays the global gate the spec names.

### Privacy, and whether `sess:{workspace}:{session}` is needed

The recommendation is fully stateless, and the request itself is the session. The spec's key would hold "last model and recent messages", meaning stored prompt text, which the Data model forbids without an opt-in. Taking each possible use in turn:

- **Clients that send only the newest message.** None of the OpenAI-format clients studied do this. Chat Completions has no `previous_response_id`; that is the Responses API, which this gateway does not serve.
- **Summarising for a smaller model.** Stateless re-summarising works, and caching the summary is an optimisation. If it is ever wanted, use a content-addressed key, the hash of the summarised prefix, under explicit opt-in with a stated TTL. Not a session.
- **Sticky routing**, keeping a conversation on the model that answered it last for prompt-cache reuse and consistency. This is the only use that genuinely needs memory, and it needs no prompt text. A key such as `stick:{workspace}:{sha256(system + first user message)}` → `target id`, with a short TTL, would do. That is a separate feature from handoff.

So I recommend that ticket 07 **drop the `sess:` key**, and that the spec's Data model row be changed or removed. Whether to do that is the owner's call; see the questions below.

### The error a client sees when handoff cannot help

- Today the gateway returns 413 with `code: "context_length_exceeded"`. Cline recognises that code at any status and runs its own deterministic recovery.
- From reading its source, Roo Code requires status 400 and would not. Clients self-heal better when the message also states the limit, for example `"This model's maximum context length is N tokens. However, your messages resulted in M tokens."`, using the N the gateway learned.
- Changing 413 to 400 would contradict the spec's honest-exhaustion rule (413 when all failures were context errors). That is an owner decision.
- Separately, an optional `context_length` field on `GET /v1/models` entries (OpenRouter's name), set to the smallest known window among the route's targets, would let clients fit before sending. Which OpenAI-compatible clients read such a field is unverified. Roo's LiteLLM provider reads `/v1/model/info` `max_input_tokens`.

### Reuse versus write in Go

- **Write:** the fitter (about 200 lines: unit grouping, placeholder clearing, middle drop, marker). Follow Continue's `compileChatMessages` and Cline's basic compaction; the licences are Apache-2.0 and the code is reimplemented, not copied.
- **Write:** the error-text parsers for M and N, next to the existing classifiers in `provider/openai` and `provider/anthropic`.
- **Reuse:** the existing `bytes/4` estimate plus a margin. A Go tokenizer is not needed, because none covers this vendor mix. `tiktoken-go/tokenizer` could refine OpenAI targets later at about 8 MB of binary.
- **Reuse as data:** OpenRouter `/models` and LiteLLM's JSON, imported offline.
- **Do not reuse:** LiteLLM's `trim_messages` (the hazards in §1), LLMLingua, or vendor compaction.

### How to prove it

These all run against `provider/mock` and need no quota:
- **Window-aware mock.** Add a mock model such as `mock/window-<N>` that returns the OpenAI-style "maximum context length is N… resulted in M" error when `bytes/4 > N`.
- **Router tests:**
  - A long conversation with targets of 8k → 128k, where the 128k target is rate limited. Without handoff it ends in 413; with trimming it is answered by the 8k target, with the header set.
  - A second turn of the same conversation produces the same cut point.
- **Property test over generated conversations.** After fitting:
  - every `tool` message follows its `tool_calls` assistant message;
  - the system messages and the final sequence are unchanged byte for byte;
  - the estimate is at or below the budget;
  - content arrays are never split.
- **`make live`:** one Groq or Cerebras free target with a small window, and a deliberately long conversation.

## Proof: the fitter, prototyped

[prototype/](prototype/) is the recommended fitter (strategy step 2) in about
150 lines of Go, as a module of its own so the gateway's build does not
include it. `cd prototype && go test -v ./...` runs three tests:

- **TestFitInvariants**, a property test over 2,000 generated agent sessions
  (system prompt, task, user turns, assistant replies, tool calls with large
  tool outputs) at random budgets from 2k to 42k tokens. 1,986 fitted and 14
  were refused because the system prompt, task and final turn alone exceeded
  the budget. Every fitted one is within budget, has every tool result right
  after its call, keeps the system prompt, task and final turn byte for byte,
  and carries a marker wherever messages were dropped.
- **TestIDEScenario**, the case from the ticket: a 123-message, ~98k-token
  session handed to a model with an 8k window. The fitter sends 42 messages
  (~4.6k tokens: 58 old tool results cleared, 82 messages omitted), with
  every pair intact and the task kept. Dropping the oldest messages instead,
  as OpenAI's `truncation: auto` does, sends 7 messages, starts with an
  orphaned tool result (a vendor 400) and loses the task.
- **TestStableCutAcrossTurns**: the next turn of the same session keeps all
  95 fitted messages as an identical prefix, so a vendor's prompt cache
  still matches.

## Open questions for the owner

1. **Silent trimming versus refusing.** Trimming loses information the user sent. Is answering with an `X-Context-Handoff` header and a marker message acceptable, or should trimming require a client opt-in (a request header) on top of the route opt-in?
   *Owner: the header and the route opt-in are enough. (2026-11-18)*
2. **Defaults.** Should reorder-by-window be on for every route (it loses nothing), with trimming opt-in only?
   *Owner: yes, `reorder` by default on every route, `trim` opt-in. (2026-11-18)*
3. **Summarisation.** Is an extra upstream call that spends the user's quota and adds latency to time-to-first-token wanted at all? If so, which target summarises: the route's first, its cheapest, or a configured one?
   *Owner: not now. (2026-11-18)*
4. **The `sess:` key.** May the spec's Data model row be removed in favour of the stateless design? Is sticky routing (no prompt text) wanted as its own ticket?
   *Owner: remove it. (2026-11-18)*
5. **413 or 400 for context exhaustion.** Keep the spec's 413, or switch to OpenAI's 400 shape so more clients (for example Roo Code) self-recover? Whichever status is chosen, the message can include the learned limit.
   *Owner: 400, with the learned limit in the message. (2026-11-18)*
6. **Window catalogue.** Should the gateway ship a window catalogue seeded from OpenRouter and LiteLLM data, with the tier caveat, or rely only on learned windows and per-target overrides?
   *Owner: both, learned windows first. (2026-11-18)*
7. **First user message.** Is keeping the first user message (the task statement) right for the main use? In long IDE sessions it can be stale.
   *Owner: rejected; the first user message is not pinned. (2026-11-18)*
