-- The level each bucket would have now, without touching it: no key is
-- written, created or given a new expiry.
--
-- KEYS[i]  bucket keys
-- ARGV[1]  now  (unix milliseconds, supplied by the caller)
-- then two values per bucket, in KEYS order: capacity, refill (tokens per second)
--
-- Returns {tokens_1_millis, tokens_2_millis, ...}.

local now = tonumber(ARGV[1])
local out = {}

for i, key in ipairs(KEYS) do
  local capacity = tonumber(ARGV[2 * i])
  local refill   = tonumber(ARGV[2 * i + 1])

  local state = redis.call('HMGET', key, 'tokens', 'ts')
  local have  = tonumber(state[1])
  local ts    = tonumber(state[2])
  if have == nil or ts == nil then
    have = capacity
    ts = now
  end

  local elapsed = math.max(0, now - ts)
  have = math.min(capacity, have + (elapsed * refill / 1000.0))
  -- Redis returns integers from Lua, so tokens travel as thousandths.
  out[i] = math.floor(have * 1000)
end

return out
