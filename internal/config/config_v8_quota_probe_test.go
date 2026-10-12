package config

import (
	"fmt"
	"reflect"
	"testing"
)

func TestV8QuotaProbeAllAPIKeyFamilies(t *testing.T) {
	for _, tc := range []struct{ family, field string }{
		{"gemini", "GeminiKey"}, {"interactions", "InteractionsKey"},
		{"codex", "CodexKey"}, {"claude", "ClaudeKey"},
		{"xai", "XAIKey"}, {"meta", "MetaKey"}, {"vertex", "VertexCompatAPIKey"},
		{"openai-compatibility", "OpenAICompatibility"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`config-version: 8
api-keys:
  %s:
    - name: quota-test
      base-url: https://supplier.invalid/v1
      quota-probe:
        url: https://supplier.invalid/balance
        header: {Authorization: "Bearer $TOKEN$"}
        mapping:
          summary: [{key: balance, label: Balance, value: data.balance, currency: USD}]
      keys:
        - api-key: test-inherited
        - api-key: test-override
          quota-probe:
            url: https://supplier.invalid/credits
            mapping:
              summary: [{key: points, label: Points, value: data.points, unit: points}]
`, tc.family))
			if err := ValidateV8Config(data); err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			entries := reflect.ValueOf(cfg).Elem().FieldByName(tc.field)
			probe := entries.Index(0).FieldByName("QuotaProbe").Interface().(map[string]any)
			if probe["url"] != "https://supplier.invalid/balance" {
				t.Fatalf("group query not preserved: %v", probe)
			}
			var override map[string]any
			if tc.family == "openai-compatibility" {
				override = cfg.OpenAICompatibility[0].APIKeyEntries[1].QuotaProbe
			} else {
				override = entries.Index(1).FieldByName("QuotaProbe").Interface().(map[string]any)
			}
			if override["url"] != "https://supplier.invalid/credits" {
				t.Fatalf("key override not preserved: %v", override)
			}
			normalized, _, err := NormalizeConfigLayout(data, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(normalized); err != nil {
				t.Fatalf("quota query did not survive round trip: %v", err)
			}
		})
	}
}
