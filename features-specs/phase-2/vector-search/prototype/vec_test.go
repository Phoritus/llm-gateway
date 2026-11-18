// Package vecproto measures the options ticket 01 compares, against a
// throwaway Redis 8 container: VECPROTO_REDIS (default 127.0.0.1:56379).
// Every test deletes what it creates; none of them flushes.
package vecproto

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const dim = 1024

func client(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("VECPROTO_REDIS")
	if addr == "" {
		addr = "127.0.0.1:56379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("no Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func randomUnit(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return normalize(v)
}

func normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	for i := range v {
		v[i] /= n
	}
	return v
}

func cosine(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// nearCopy returns a unit vector whose cosine with v is exactly target.
func nearCopy(r *rand.Rand, v []float32, target float64) []float32 {
	u := randomUnit(r, len(v))
	d := cosine(u, v)
	for i := range u {
		u[i] -= float32(d) * v[i]
	}
	u = normalize(u)
	s := math.Sqrt(1 - target*target)
	out := make([]float32, len(v))
	for i := range v {
		out[i] = float32(target)*v[i] + float32(s)*u[i]
	}
	return normalize(out)
}

func blob(v []float32) string {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return string(b)
}

func usedMemory(t *testing.T, rdb *redis.Client) int64 {
	t.Helper()
	info, err := rdb.Info(context.Background(), "memory").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\r\n") {
		if v, ok := strings.CutPrefix(line, "used_memory:"); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	t.Fatal("no used_memory in INFO")
	return 0
}

func vsim(t *testing.T, rdb *redis.Client, key string, v []float32, count int) map[string]float64 {
	t.Helper()
	res, err := rdb.Do(context.Background(), "VSIM", key, "FP32", blob(v), "WITHSCORES", "COUNT", count).Slice()
	if err != nil && err != redis.Nil {
		t.Fatalf("VSIM %s: %v", key, err)
	}
	out := map[string]float64{}
	for i := 0; i+1 < len(res); i += 2 {
		f, _ := strconv.ParseFloat(fmt.Sprint(res[i+1]), 64)
		out[fmt.Sprint(res[i])] = f
	}
	return out
}

func TestAnotherWorkspacesKeyNeverAnswers(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(1, 1))
	a, b := "vec:ws-a:bge-m3:1024", "vec:ws-b:bge-m3:1024"
	t.Cleanup(func() { rdb.Del(ctx, a, b) })

	v := randomUnit(r, dim)
	if err := rdb.Do(ctx, "VADD", a, "FP32", blob(v), "answer-a", "Q8").Err(); err != nil {
		t.Fatal(err)
	}
	if got := vsim(t, rdb, b, v, 10); len(got) != 0 {
		t.Fatalf("workspace b's key answered with %v", got)
	}
	if err := rdb.Do(ctx, "VADD", b, "FP32", blob(randomUnit(r, dim)), "answer-b", "Q8").Err(); err != nil {
		t.Fatal(err)
	}
	if got := vsim(t, rdb, b, v, 10); got["answer-a"] != 0 || len(got) != 1 {
		t.Fatalf("workspace b's search reached outside its key: %v", got)
	}
}

func TestASetPinsItsDimension(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(2, 2))
	key := "vec:ws-dim:fam:1024"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	if err := rdb.Do(ctx, "VADD", key, "FP32", blob(randomUnit(r, dim)), "e1", "Q8").Err(); err != nil {
		t.Fatal(err)
	}
	err := rdb.Do(ctx, "VADD", key, "FP32", blob(randomUnit(r, 768)), "e2", "Q8").Err()
	if err == nil {
		t.Fatal("a 768-dimension vector was accepted into a 1024-dimension set")
	}
	t.Logf("768 into 1024: %v", err)
	d, _ := rdb.Do(ctx, "VDIM", key).Int()
	t.Logf("VDIM = %d", d)
}

func TestScoreIsHalfOfOnePlusCosine(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(3, 3))
	key := "vec:ws-score:fam:1024"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	v := randomUnit(r, dim)
	for _, c := range []float64{1, 0.95, 0.5, 0, -1} {
		w := v
		if c != 1 {
			if c == -1 {
				w = slices.Clone(v)
				for i := range w {
					w[i] = -w[i]
				}
			} else {
				w = nearCopy(r, v, c)
			}
		}
		if err := rdb.Do(ctx, "VADD", key, "FP32", blob(w), fmt.Sprint(c), "NOQUANT").Err(); err != nil {
			t.Fatal(err)
		}
	}
	for name, score := range vsim(t, rdb, key, v, 10) {
		c, _ := strconv.ParseFloat(name, 64)
		if want := (1 + c) / 2; math.Abs(score-want) > 1e-3 {
			t.Errorf("cosine %v scored %v, want %v", c, score, want)
		}
		t.Logf("cosine %5v -> score %.4f", c, score)
	}
}

func TestQ8KeepsANearDuplicateAboveThreshold(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(4, 4))
	keys := map[string]string{"Q8": "vec:ws-q8:fam:1024", "BIN": "vec:ws-bin:fam:1024", "NOQUANT": "vec:ws-f32:fam:1024"}
	t.Cleanup(func() {
		for _, k := range keys {
			rdb.Del(ctx, k)
		}
	})
	const n = 2000
	stored := make([][]float32, n)
	for i := range stored {
		stored[i] = randomUnit(r, dim)
	}
	for quant, key := range keys {
		pipe := rdb.Pipeline()
		for i, v := range stored {
			pipe.Do(ctx, "VADD", key, "FP32", blob(v), strconv.Itoa(i), quant)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var worst = map[string]float64{}
	for _, c := range []float64{0.99, 0.97, 0.95, 0.93} {
		for trial := range 20 {
			target := stored[trial*7]
			q := nearCopy(r, target, c)
			exact := (1 + cosine(q, target)) / 2
			for quant, key := range keys {
				got := vsim(t, rdb, key, q, 1)
				score, ok := got[strconv.Itoa(trial*7)]
				if !ok {
					t.Logf("%s: cosine %.2f, the stored entry was not the top hit: %v", quant, c, got)
					worst[quant] = math.Inf(1)
					continue
				}
				worst[quant] = math.Max(worst[quant], math.Abs(score-exact))
			}
		}
	}
	for quant, w := range worst {
		t.Logf("%-7s largest score error over 80 queries: %.5f (score units; cosine error is twice that)", quant, w)
	}
	if worst["Q8"] > 0.005 {
		t.Errorf("Q8 moved a score by %.4f, enough to cross a 0.95 cosine threshold", worst["Q8"])
	}
}

func TestMemoryPerEntry(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(5, 5))
	const n = 5000
	vectors := make([]string, n)
	for i := range vectors {
		vectors[i] = blob(randomUnit(r, dim))
	}

	measure := func(name string, load func(), drop func()) {
		before := usedMemory(t, rdb)
		load()
		after := usedMemory(t, rdb)
		drop()
		t.Logf("%-22s %6d bytes per entry over %d entries", name, (after-before)/n, n)
	}

	for _, quant := range []string{"NOQUANT", "Q8", "BIN"} {
		key := "vec:ws-mem:" + quant
		measure("vector set "+quant, func() {
			pipe := rdb.Pipeline()
			for i, v := range vectors {
				pipe.Do(ctx, "VADD", key, "FP32", v, strconv.Itoa(i), quant)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}, func() { rdb.Del(ctx, key) })
	}

	for _, algo := range []string{"HNSW", "FLAT"} {
		idx, prefix := "idx:mem:"+algo, "h:mem:"+algo+":"
		measure("query engine "+algo, func() {
			if err := rdb.Do(ctx, "FT.CREATE", idx, "ON", "HASH", "PREFIX", 1, prefix, "SCHEMA",
				"v", "VECTOR", algo, 6, "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE").Err(); err != nil {
				t.Fatal(err)
			}
			pipe := rdb.Pipeline()
			for i, v := range vectors {
				pipe.HSet(ctx, prefix+strconv.Itoa(i), "v", v)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			waitIndexed(t, rdb, idx, n)
		}, func() { rdb.Do(ctx, "FT.DROPINDEX", idx, "DD") })
	}
}

func waitIndexed(t *testing.T, rdb *redis.Client, idx string, n int) {
	t.Helper()
	for range 600 {
		info, err := rdb.Do(context.Background(), "FT.INFO", idx).Slice()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(info); i += 2 {
			if fmt.Sprint(info[i]) == "num_docs" && fmt.Sprint(info[i+1]) == strconv.Itoa(n) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never indexed %d documents", idx, n)
}

func TestCostOfAnIndexPerWorkspace(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(6, 6))
	const workspaces = 500

	before := usedMemory(t, rdb)
	for i := range workspaces {
		idx := fmt.Sprintf("idx:ws%d", i)
		if err := rdb.Do(ctx, "FT.CREATE", idx, "ON", "HASH", "PREFIX", 1, fmt.Sprintf("h:ws%d:", i), "SCHEMA",
			"v", "VECTOR", "HNSW", 6, "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE").Err(); err != nil {
			t.Fatal(err)
		}
	}
	after := usedMemory(t, rdb)
	t.Logf("empty query engine index: %d bytes each over %d", (after-before)/workspaces, workspaces)
	for i := range workspaces {
		rdb.Do(ctx, "FT.DROPINDEX", fmt.Sprintf("idx:ws%d", i))
	}

	before = usedMemory(t, rdb)
	for i := range workspaces {
		rdb.Do(ctx, "VADD", fmt.Sprintf("vec:ws%d", i), "FP32", blob(randomUnit(r, dim)), "e", "Q8")
	}
	after = usedMemory(t, rdb)
	t.Logf("vector set holding one Q8 entry: %d bytes each over %d", (after-before)/workspaces, workspaces)
	for i := range workspaces {
		rdb.Del(ctx, fmt.Sprintf("vec:ws%d", i))
	}
}

func TestSearchLatency(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(7, 7))
	const n, queries = 5000, 300
	key, idx, prefix := "vec:ws-lat", "idx:lat", "h:lat:"
	t.Cleanup(func() { rdb.Del(ctx, key); rdb.Do(ctx, "FT.DROPINDEX", idx, "DD") })

	rdb.Do(ctx, "FT.CREATE", idx, "ON", "HASH", "PREFIX", 1, prefix, "SCHEMA",
		"v", "VECTOR", "HNSW", 6, "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE")
	pipe := rdb.Pipeline()
	for i := range n {
		v := blob(randomUnit(r, dim))
		pipe.Do(ctx, "VADD", key, "FP32", v, strconv.Itoa(i), "Q8")
		pipe.HSet(ctx, prefix+strconv.Itoa(i), "v", v)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, rdb, idx, n)

	time1 := func(name string, run func(q string) error) {
		d := make([]time.Duration, queries)
		for i := range d {
			q := blob(randomUnit(r, dim))
			start := time.Now()
			if err := run(q); err != nil {
				t.Fatal(err)
			}
			d[i] = time.Since(start)
		}
		slices.Sort(d)
		t.Logf("%-22s p50 %v  p99 %v  (%d entries, round trip included)", name, d[queries/2], d[queries*99/100], n)
	}
	time1("PING (round trip)", func(string) error { return rdb.Ping(ctx).Err() })
	time1("VSIM COUNT 1", func(q string) error {
		return rdb.Do(ctx, "VSIM", key, "FP32", q, "WITHSCORES", "COUNT", 1).Err()
	})
	time1("VSIM COUNT 1 NOTHREAD", func(q string) error {
		return rdb.Do(ctx, "VSIM", key, "FP32", q, "WITHSCORES", "COUNT", 1, "NOTHREAD").Err()
	})
	time1("FT.SEARCH KNN 1", func(q string) error {
		res, err := rdb.Do(ctx, "FT.SEARCH", idx, "*=>[KNN 1 @v $q AS d]", "PARAMS", 2, "q", q,
			"RETURN", 1, "d", "DIALECT", 2).Slice()
		if err == nil && (len(res) != 3 || fmt.Sprint(res[0]) != "1") {
			return fmt.Errorf("FT.SEARCH returned %v, want one hit", res)
		}
		return err
	})
}

// store adds an entry and its answer, then evicts the oldest entries past the
// cap, in one script so a crash cannot leave a vector without its answer.
var store = redis.NewScript(`
redis.call('VADD', KEYS[1], 'FP32', ARGV[1], ARGV[2], 'Q8')
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[2], ARGV[5])
local n = redis.call('VCARD', KEYS[1])
local cap = tonumber(ARGV[4])
while n > cap do
  local oldest = redis.call('ZPOPMIN', KEYS[2])
  redis.call('VREM', KEYS[1], oldest[1])
  redis.call('HDEL', KEYS[3], oldest[1])
  n = n - 1
end
return n
`)

func TestCapEvictsTheOldestInOneScript(t *testing.T) {
	rdb, ctx := client(t), context.Background()
	r := rand.New(rand.NewPCG(8, 8))
	keys := []string{"vec:ws-cap", "vecage:ws-cap", "vecans:ws-cap"}
	t.Cleanup(func() { rdb.Del(ctx, keys...) })
	const cap, total = 100, 150

	for i := range total {
		if err := store.Run(ctx, rdb, keys, blob(randomUnit(r, dim)), "e"+strconv.Itoa(i), i, cap, "answer").Err(); err != nil {
			t.Fatal(err)
		}
	}
	card, _ := rdb.Do(ctx, "VCARD", keys[0]).Int()
	answers, _ := rdb.HLen(ctx, keys[2]).Result()
	ages, _ := rdb.ZCard(ctx, keys[1]).Result()
	if card != cap || answers != cap || ages != cap {
		t.Fatalf("after %d stores at cap %d: VCARD %d, answers %d, ages %d", total, cap, card, answers, ages)
	}
	for _, e := range []string{"e0", "e49"} {
		if in, _ := rdb.Do(ctx, "VISMEMBER", keys[0], e).Int(); in != 0 {
			t.Errorf("%s, among the oldest, is still in the set", e)
		}
	}
	if in, _ := rdb.Do(ctx, "VISMEMBER", keys[0], "e50").Int(); in != 1 {
		t.Error("e50, the oldest survivor, was evicted")
	}
}
