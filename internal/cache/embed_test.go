package cache

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
)

type embedderFunc func(ctx context.Context, req *provider.EmbedRequest) (*provider.EmbedResponse, error)

func (f embedderFunc) Embed(ctx context.Context, req *provider.EmbedRequest, _ provider.Credential) (*provider.EmbedResponse, error) {
	return f(ctx, req)
}

func fixed(v ...float32) embedderFunc {
	return func(context.Context, *provider.EmbedRequest) (*provider.EmbedResponse, error) {
		return &provider.EmbedResponse{Vector: v}, nil
	}
}

func block(ctx context.Context, _ *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var twoVendors = Family{
	Name: "f", Dims: 2, Matryoshka: true,
	Models: map[provider.Vendor]string{provider.VendorGemini: "g", provider.VendorOpenRouter: "o/g"},
}

func targets(vs ...provider.Vendor) []EmbedTarget {
	var out []EmbedTarget
	for _, v := range vs {
		out = append(out, EmbedTarget{Vendor: v})
	}
	return out
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func TestFamiliesAreConsistent(t *testing.T) {
	for key, f := range Families {
		if f.Name != key || f.Dims <= 0 || len(f.Models) == 0 {
			t.Errorf("family %q = %+v", key, f)
		}
	}
}

func TestEmbedWithMock(t *testing.T) {
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorMock: &mock.Provider{}}, time.Second)
	fam := Families["mock"]
	a, err := e.Embed(t.Context(), fam, targets(provider.VendorMock), "reset my password")
	if err != nil {
		t.Fatal(err)
	}
	if a.Family != "mock" || a.Dims != 8 || len(a.Values) != 8 {
		t.Fatalf("vector = %+v", a)
	}
	b, _ := e.Embed(t.Context(), fam, targets(provider.VendorMock), "reset my password please")
	c, _ := e.Embed(t.Context(), fam, targets(provider.VendorMock), "tomato soup")
	if dot(a.Values, b.Values) <= dot(a.Values, c.Values) {
		t.Errorf("shared words %v, unrelated %v", dot(a.Values, b.Values), dot(a.Values, c.Values))
	}
}

func TestEmbedNormalises(t *testing.T) {
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: fixed(3, 4)}, time.Second)
	v, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorGemini), "x")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(dot(v.Values, v.Values)-1) > 1e-6 || math.Abs(float64(v.Values[0])-0.6) > 1e-6 {
		t.Errorf("values = %v", v.Values)
	}
}

func TestEmbedSendsDimensionsOnlyToMatryoshkaFamilies(t *testing.T) {
	var got provider.EmbedRequest
	spy := embedderFunc(func(_ context.Context, r *provider.EmbedRequest) (*provider.EmbedResponse, error) {
		got = *r
		return &provider.EmbedResponse{Vector: []float32{1, 0}}, nil
	})
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: spy}, time.Second)
	if _, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorGemini), "x"); err != nil {
		t.Fatal(err)
	}
	if got.Model != "g" || got.Input != "x" || got.Dimensions != 2 {
		t.Errorf("request = %+v", got)
	}
	native := twoVendors
	native.Matryoshka = false
	if _, err := e.Embed(t.Context(), native, targets(provider.VendorGemini), "x"); err != nil {
		t.Fatal(err)
	}
	if got.Dimensions != 0 {
		t.Errorf("dimensions = %d, want none", got.Dimensions)
	}
}

func TestEmbedFailsOver(t *testing.T) {
	fam := Family{Name: "f", Dims: 2, Models: map[provider.Vendor]string{provider.VendorMock: "mock/fail", provider.VendorGemini: "g"}}
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{
		provider.VendorMock:   &mock.Provider{},
		provider.VendorGemini: fixed(1, 0),
	}, time.Second)
	v, err := e.Embed(t.Context(), fam, targets(provider.VendorMock, provider.VendorGemini), "x")
	if err != nil || v.Values[0] != 1 {
		t.Fatalf("v = %+v, err = %v", v, err)
	}
}

func TestEmbedSkipsVendorsOutsideTheFamily(t *testing.T) {
	called := false
	other := embedderFunc(func(context.Context, *provider.EmbedRequest) (*provider.EmbedResponse, error) {
		called = true
		return &provider.EmbedResponse{Vector: []float32{1, 0}}, nil
	})
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorOpenAI: other, provider.VendorGemini: fixed(0, 1)}, time.Second)
	v, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorOpenAI, provider.VendorAnthropic, provider.VendorGemini), "x")
	if err != nil || called || v.Values[1] != 1 {
		t.Errorf("v = %+v, err = %v, outside vendor called = %v", v, err, called)
	}
}

func TestEmbedBadAnswersFailOver(t *testing.T) {
	for name, bad := range map[string]embedderFunc{
		"wrong length": fixed(1, 0, 0),
		"NaN":          fixed(float32(math.NaN()), 1),
		"Inf":          fixed(float32(math.Inf(1)), 1),
		"zero":         fixed(0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: bad, provider.VendorOpenRouter: fixed(0, 2)}, time.Second)
			v, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorGemini, provider.VendorOpenRouter), "x")
			if err != nil || v.Values[1] != 1 {
				t.Errorf("v = %+v, err = %v", v, err)
			}
			e = NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: bad}, time.Second)
			if _, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorGemini), "x"); !errors.Is(err, provider.ErrUpstream) {
				t.Errorf("err = %v, want ErrUpstream", err)
			}
		})
	}
}

func TestEmbedTimeoutBoundsTheWholeCall(t *testing.T) {
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: embedderFunc(block), provider.VendorOpenRouter: embedderFunc(block)}, 100*time.Millisecond)
	start := time.Now()
	_, err := e.Embed(t.Context(), twoVendors, targets(provider.VendorGemini, provider.VendorOpenRouter), "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 180*time.Millisecond {
		t.Errorf("took %v for a 100ms budget over two targets", d)
	}
}

func TestEmbedCallerCancelStopsAtOnce(t *testing.T) {
	second := false
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{
		provider.VendorGemini: embedderFunc(block),
		provider.VendorOpenRouter: embedderFunc(func(context.Context, *provider.EmbedRequest) (*provider.EmbedResponse, error) {
			second = true
			return &provider.EmbedResponse{Vector: []float32{1, 0}}, nil
		}),
	}, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := e.Embed(ctx, twoVendors, targets(provider.VendorGemini, provider.VendorOpenRouter), "x")
	if !errors.Is(err, context.Canceled) || second {
		t.Errorf("err = %v, second target tried = %v", err, second)
	}
}

func TestEmbedNoEmbedder(t *testing.T) {
	e := NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorGemini: fixed(1, 0)}, time.Second)
	for name, ts := range map[string][]EmbedTarget{
		"no targets":     nil,
		"outside family": targets(provider.VendorAnthropic),
		"no adapter":     targets(provider.VendorOpenRouter),
	} {
		if _, err := e.Embed(t.Context(), twoVendors, ts, "x"); !errors.Is(err, ErrNoEmbedder) {
			t.Errorf("%s: err = %v, want ErrNoEmbedder", name, err)
		}
	}
}
