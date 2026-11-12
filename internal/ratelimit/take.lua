-- Several token buckets spent as one atomic step: either every bucket holds
-- its cost and all are spent, or none is touched. Checking them one script at
-- a time would spend the first bucket of a request the second then refuses.
--
-- KEYS[i]  bucket keys
-- ARGV[1]  now    (unix milliseconds, supplied by the caller)
-- ARGV[2]  force  ("1" applies every cost without the admission check: a
--                  negative cost refunds, a positive one may leave a debt)
-- then four values per bucket, in KEYS order:
--   capacity, refill (tokens per second), cost, ttl (milliseconds)
--
-- Returns {allowed, tokens_1_millis, retry_1_millis, tokens_2_millis, ...}.
-- A bucket's retry is zero unless that bucket refused.

local now   = tonumber(ARGV[1])
local force = ARGV[2] == '1'

local tokens  = {}
local out     = {1}
local refused = false

for i, key in ipairs(KEYS) do
  local a        = 4 * i - 1
  local capacity = tonumber(ARGV[a])
  local refill   = tonumber(ARGV[a + 1])
  local cost     = tonumber(ARGV[a + 2])

  local state = redis.call('HMGET', key, 'tokens', 'ts')
  local have  = tonumber(state[1])
  local ts    = tonumber(state[2])
  if have == nil or ts == nil then
    have = capacity
    ts = now
  end

  -- Clock skew between instances could otherwise hand out free tokens.
  local elapsed = math.max(0, now - ts)
  have = math.min(capacity, have + (elapsed * refill / 1000.0))

  local retry = 0
  if not force and have < cost then
    refused = true
    retry = math.ceil(((cost - have) / refill) * 1000.0)
  end

  tokens[i] = have
  -- Redis returns integers from Lua, so tokens travel as thousandths.
  out[2 * i] = math.floor(have * 1000)
  out[2 * i + 1] = retry
end

if refused then
  out[1] = 0
  return out
end

for i, key in ipairs(KEYS) do
  local a        = 4 * i - 1
  local capacity = tonumber(ARGV[a])
  local cost     = tonumber(ARGV[a + 2])

  -- A refund cannot overfill the bucket, and a debt is floored so that one
  -- huge answer cannot lock a bucket for more than about two refill periods.
  local left = math.max(-capacity, math.min(capacity, tokens[i] - cost))
  redis.call('HSET', key, 'tokens', left, 'ts', now)
  redis.call('PEXPIRE', key, ARGV[a + 3])
  out[2 * i] = math.floor(left * 1000)
end

return out
