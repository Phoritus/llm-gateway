package provider

// Vendor names an upstream a workspace can store a credential for.
type Vendor string

const (
	VendorOpenAI     Vendor = "openai"
	VendorGroq       Vendor = "groq"
	VendorCerebras   Vendor = "cerebras"
	VendorOpenRouter Vendor = "openrouter"
	VendorMistral    Vendor = "mistral"
	VendorGemini     Vendor = "gemini"
	VendorAnthropic  Vendor = "anthropic"
	VendorMock       Vendor = "mock"
	VendorCustom     Vendor = "custom"
)

var vendors = []Vendor{
	VendorOpenAI, VendorGroq, VendorCerebras, VendorOpenRouter,
	VendorMistral, VendorGemini, VendorAnthropic, VendorMock, VendorCustom,
}

// Vendors returns every Vendor in a stable order. The caller owns the slice.
func Vendors() []Vendor { return append([]Vendor(nil), vendors...) }

// ParseVendor accepts only the exact lowercase name.
func ParseVendor(s string) (Vendor, bool) {
	for _, v := range vendors {
		if string(v) == s {
			return v, true
		}
	}
	return "", false
}

func (v Vendor) DevelopmentOnly() bool { return v == VendorMock }
