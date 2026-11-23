package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// maxSemanticEntries is how many answers one workspace keeps per family.
	// Past it the oldest-inserted go first.
	maxSemanticEntries = 1000
	// semanticIdleTTL is how long a workspace's vectors outlive its last
	// write, so one that stops asking, or a family no longer used, goes away
	// by itself.
	semanticIdleTTL = 7 * 24 * time.Hour
)

// putScript writes one entry to the three keys and trims them together, so a
// vector never outlives its answer. KEYS are the vector set, the age sorted
// set and the answer hash. ARGV is the element id, its attributes, the insert
// time in ms, the answer, the cap, the idle TTL in ms, the dimension, then the
// vector.
var putScript = redis.NewScript(`
local dims = tonumber(ARGV[7])
-- unpack in the middle of an argument list yields one value, so the call is built whole.
local vadd = {'VALUES', dims}
for i = 8, 7 + dims do
  vadd[#vadd + 1] = ARGV[i]
end
for _, a in ipairs({ARGV[1], 'Q8', 'SETATTR', ARGV[2]}) do
  vadd[#vadd + 1] = a
end
redis.call('VADD', KEYS[1], unpack(vadd))
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[1])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[4])
local over = redis.call('ZCARD', KEYS[2]) - tonumber(ARGV[5])
if over > 0 then
  local old = redis.call('ZPOPMIN', KEYS[2], over)
  for i = 1, #old, 2 do
    redis.call('VREM', KEYS[1], old[i])
    redis.call('HDEL', KEYS[3], old[i])
  end
end
for i = 1, 3 do
  redis.call('PEXPIRE', KEYS[i], ARGV[6])
end
return 1
`)

// Semantic is the semantic cache's store: per workspace and family, a Redis
// vector set of the questions asked, with the answers beside it. It lives in a
// Redis 8 of its own, not the one the rest of the gateway uses.
type Semantic struct {
	rdb *redis.Client
	// limit and now are fields so a test can fill a cache past its cap without
	// a thousand writes, and tell two writes apart in time.
	limit int
	now   func() time.Time
}

// NewSemantic returns a store over rdb, which must be a Redis with vector sets.
func NewSemantic(rdb *redis.Client) *Semantic {
	return &Semantic{rdb: rdb, limit: maxSemanticEntries, now: time.Now}
}

// stored is what the answer hash holds: the answer and the exact model that
// embedded its question.
type stored struct {
	Entry
	EmbedModel string `json:"embed_model"`
}

func semanticKeys(workspaceID string, v Vector) (vec, age, ans string) {
	suffix := workspaceID + ":" + v.Family + ":" + strconv.Itoa(v.Dims)
	return "vec:" + suffix, "vecage:" + suffix, "vecans:" + suffix
}

// Lookup returns the answer to the closest stored question asked in the same
// context (ctxHash) and scoring at least fam.Threshold, or nil. Any error,
// including an answer that is gone or cannot be read, is for the caller to
// treat as a miss.
func (s *Semantic) Lookup(ctx context.Context, workspaceID string, fam Family, ctxHash string, v Vector) (*Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	vec, _, ans := semanticKeys(workspaceID, v)
	// VSIM scores are (1 + cosine) / 2, and EPSILON is a distance in those
	// units, so a cosine threshold c is an EPSILON of (1 - c) / 2.
	epsilon := (1 - fam.Threshold) / 2
	args := []any{"VSIM", vec, "VALUES", len(v.Values)}
	for _, x := range v.Values {
		args = append(args, x)
	}
	// ctxHash is hex, so it cannot break out of the filter's string.
	args = append(args, "COUNT", 1, "EPSILON", strconv.FormatFloat(epsilon, 'f', -1, 64),
		"FILTER", `.ctx == "`+ctxHash+`"`)
	got, err := s.rdb.Do(ctx, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("cache: vsim: %w", err)
	}
	id, err := topElement(got)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}

	raw, err := s.rdb.HGet(ctx, ans, id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get answer: %w", err)
	}
	var st stored
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("cache: decode entry: %w", err)
	}
	if !ResponseCacheable(&st.Response) {
		return nil, errors.New("cache: stored entry is not a complete answer")
	}
	return &st.Entry, nil
}

// Put stores e as the answer to the question v embeds, asked in ctxHash. An
// answer past maxEntryBytes is left out, which is not an error. The element id
// is a hash of the context and the vector, so writing the same question twice
// replaces its entry instead of adding a second.
func (s *Semantic) Put(ctx context.Context, workspaceID string, fam Family, ctxHash string, v Vector, e Entry) error {
	raw, err := json.Marshal(stored{Entry: e, EmbedModel: v.Model})
	if err != nil {
		return fmt.Errorf("cache: encode entry: %w", err)
	}
	if len(raw) > maxEntryBytes {
		return nil
	}
	attrs, err := json.Marshal(map[string]string{"ctx": ctxHash})
	if err != nil {
		return fmt.Errorf("cache: encode attributes: %w", err)
	}

	keys := make([]string, 3)
	keys[0], keys[1], keys[2] = semanticKeys(workspaceID, v)
	args := []any{elementID(ctxHash, v), string(attrs), s.now().UnixMilli(), string(raw),
		s.limit, semanticIdleTTL.Milliseconds(), len(v.Values)}
	for _, x := range v.Values {
		args = append(args, x)
	}
	if err := putScript.Run(ctx, s.rdb, keys, args...).Err(); err != nil {
		return fmt.Errorf("cache: put: %w", err)
	}
	return nil
}

// topElement reads the closest element id out of a VSIM reply, or "" when
// nothing in the context was close enough.
func topElement(reply any) (string, error) {
	r, ok := reply.([]any)
	if !ok {
		return "", fmt.Errorf("cache: vsim returned %T, want a list of elements", reply)
	}
	if len(r) == 0 {
		return "", nil
	}
	id, ok := r[0].(string)
	if !ok {
		return "", fmt.Errorf("cache: vsim element is %T, want a string", r[0])
	}
	return id, nil
}

func elementID(ctxHash string, v Vector) string {
	h := sha256.New()
	h.Write([]byte(ctxHash))
	var b [4]byte
	for _, x := range v.Values {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(x))
		h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
