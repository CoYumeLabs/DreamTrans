package billing

import "testing"

func TestCanonicalSKUResolvesQualifiedProvider(t *testing.T) {
	provider, sku := CanonicalSKU("openai-compatible", "cerebras::llama-3.3-70b", "chat")
	if provider != "cerebras" || sku != "llama-3.3-70b" {
		t.Fatalf("qualified id: %s %s", provider, sku)
	}
	provider, sku = CanonicalSKU("", "meta-llama/llama-3.3-70b", "chat")
	if provider != "openai-compatible" || sku != "meta-llama/llama-3.3-70b" {
		t.Fatalf("gateway id stays with the default provider: %s %s", provider, sku)
	}
}
