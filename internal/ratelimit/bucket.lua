-- Token bucket, evaluated server-side so that the read, the refill and the
-- write are one atomic step. Doing this in Go would let two instances both
-- see the same remaining tokens and both allow the request.
--
-- KEYS[1] bucket key
-- ARGV[1] capacity        (tokens, the burst size)
-- ARGV[2] refill          (tokens per second)
-- ARGV[3] now             (unix milliseconds, supplied by the caller so every
--                          instance agrees on the clock)
-- ARGV[4] cost            (tokens this request reserves)
-- ARGV[5] ttl             (milliseconds of idle time before the key expires)
--
-- Returns {allowed, tokens_remaining_millis, retry_after_millis}

local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill   = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local cost     = tonumber(ARGV[4])
local ttl      = tonumber(ARGV[5])

local state  = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil or ts == nil then
  tokens = capacity
  ts = now
end

-- Clock skew between instances could otherwise hand out free tokens.
local elapsed = now - ts
if elapsed < 0 then
  elapsed = 0
end

tokens = math.min(capacity, tokens + (elapsed * refill / 1000.0))

local allowed = 0
local retry = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
else
  retry = math.ceil(((cost - tokens) / refill) * 1000.0)
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', key, ttl)

-- Redis returns integers from Lua, so tokens travel as thousandths.
return {allowed, math.floor(tokens * 1000), retry}
