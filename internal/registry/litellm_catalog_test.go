package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestEmbeddedLiteLLMCatalogSnapshotAndProvenance(t *testing.T) {
	models, err := parseLiteLLMCatalog(embeddedLiteLLMModels)
	if err != nil || len(models) < 3000 {
		t.Fatalf("parsed model count=%d, error=%v", len(models), err)
	}
	var source struct {
		Source, SHA256 string
		Entries        int
	}
	data, err := os.ReadFile("models/litellm_models.source.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(embeddedLiteLLMModels)
	if source.Source != liteLLMCatalogURL || source.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("snapshot provenance does not match embedded bytes")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(embeddedLiteLLMModels, &raw); err != nil || len(raw) != source.Entries {
		t.Fatal("snapshot count does not match provenance")
	}
	for _, id := range []string{"deepseek-flash", "deepseek-v4-pro", "glm-5.3-flash"} {
		info := LookupModelCapabilityInfo(id)
		if info == nil || info.ContextLength <= 0 || info.MaxCompletionTokens <= 0 {
			t.Fatalf("missing latest model limits for %s: %+v", id, info)
		}
	}
}

func replaceLiteLLMCatalogForTest(t *testing.T, data string) {
	t.Helper()
	liteLLMCatalog.RLock()
	previous := liteLLMCatalog.models
	liteLLMCatalog.RUnlock()
	t.Cleanup(func() { liteLLMCatalog.Lock(); liteLLMCatalog.models = previous; liteLLMCatalog.Unlock() })
	if _, err := publishLiteLLMCatalog([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

func TestLiteLLMLimitsAndReasoningAreNotGuessed(t *testing.T) {
	replaceLiteLLMCatalogForTest(t, `{
	 "zai/native-model":{"litellm_provider":"zai","mode":"chat","max_input_tokens":123456,"max_output_tokens":8192,"max_tokens":8192,"reasoning_effort_levels":["low","high","max"],"default_reasoning_effort":"high"},
	 "boolean-only":{"litellm_provider":"deepseek","mode":"chat","max_input_tokens":1048576,"supports_reasoning":true,"supports_max_reasoning_effort":true},
	 "output-only":{"litellm_provider":"openai","mode":"chat","max_tokens":4096,"max_output_tokens":4096},
	 "disabled":{"litellm_provider":"openai","mode":"chat","supports_reasoning":false},
	 "sample_spec":{"max_input_tokens":999999},
	 "embedding-model":{"litellm_provider":"openai","mode":"embedding","max_input_tokens":1000}
	}`)
	info := LookupModelCapabilityInfo("native-model")
	if info == nil || info.ContextLength != 123456 || info.MaxCompletionTokens != 8192 || info.Thinking.DefaultLevel != "high" || !reflect.DeepEqual(info.Thinking.Levels, []string{"low", "high", "max"}) {
		t.Fatalf("incorrect metadata: %+v", info)
	}
	info.Thinking.Levels[0] = "changed"
	if LookupModelCapabilityInfo("native-model").Thinking.Levels[0] != "low" {
		t.Fatal("lookup shared mutable catalog storage")
	}
	if info := LookupModelCapabilityInfo("boolean-only"); info.Thinking != nil {
		t.Fatal("supports_reasoning alone invented effort levels")
	}
	if info := LookupModelCapabilityInfo("output-only"); info.ContextLength != 0 || info.MaxCompletionTokens != 4096 {
		t.Fatal("output token limit was used as context window")
	}
	if info := LookupModelCapabilityInfo("disabled"); info.Thinking == nil || len(info.Thinking.Levels) != 0 {
		t.Fatal("explicit unsupported reasoning was lost")
	}
	if LookupModelCapabilityInfo("sample_spec") != nil || LookupModelCapabilityInfo("embedding-model") != nil {
		t.Fatal("non-chat metadata imported as a chat model")
	}
}

func TestLiteLLMDoesNotResolveResellerModelOrPublicAlias(t *testing.T) {
	replaceLiteLLMCatalogForTest(t, `{
	 "hosting/native-model":{"litellm_provider":"hosting","mode":"chat","max_input_tokens":12000},
	 "zai/canonical-model":{"litellm_provider":"zai","mode":"chat","max_input_tokens":64000},
	 "zhipuai/canonical-model":{"litellm_provider":"zhipuai","mode":"chat","max_input_tokens":32000}
	}`)
	if LookupModelCapabilityInfo("native-model") != nil {
		t.Fatal("hosting-specific limits leaked into a bare model")
	}
	if LookupModelCapabilityInfo("public-alias") != nil {
		t.Fatal("unknown alias matched a model")
	}
	if LookupModelCapabilityInfo("canonical-model") != nil {
		t.Fatal("conflicting native metadata was selected arbitrarily")
	}
	if info := LookupModelCapabilityInfo("hosting/native-model"); info == nil || info.ContextLength != 12000 {
		t.Fatal("exact upstream ID could not be resolved")
	}
}

func TestLiteLLMRefreshReplacesMetadataAndRejectsInvalidData(t *testing.T) {
	replaceLiteLLMCatalogForTest(t, `{"claude-opus-4-6":{"litellm_provider":"anthropic","mode":"chat","max_input_tokens":123456}}`)
	base := LookupStaticModelInfo("claude-opus-4-6")
	info := LookupModelCapabilityInfo("claude-opus-4-6")
	if info.ContextLength != 123456 || !reflect.DeepEqual(info.Thinking, base.Thinking) {
		t.Fatal("fresh limits or CPA-specific reasoning metadata was lost")
	}
	changed, err := publishLiteLLMCatalog([]byte(`{"new-remote-model":{"litellm_provider":"openai","mode":"chat","max_input_tokens":256000}}`))
	if err != nil || !reflect.DeepEqual(changed, []string{"litellm"}) {
		t.Fatal("valid refresh did not notify capability consumers")
	}
	for _, data := range []string{`{}`, `not JSON`, `{"bad":{"litellm_provider":"openai","mode":"chat","max_input_tokens":-1}}`} {
		if _, err := publishLiteLLMCatalog([]byte(data)); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
		if info := LookupModelCapabilityInfo("new-remote-model"); info == nil || info.ContextLength != 256000 {
			t.Fatal("invalid refresh replaced last valid catalog")
		}
	}
}

func TestConfiguredAliasesFillMissingCapabilitiesOnly(t *testing.T) {
	replaceLiteLLMCatalogForTest(t, `{
	 "actual-model":{"litellm_provider":"openai","mode":"chat","max_input_tokens":64000,"reasoning_effort_levels":["low","high"]},
	 "canonical-alias":{"litellm_provider":"openai","mode":"chat","max_input_tokens":200000,"max_output_tokens":8192,"reasoning_effort_levels":["max"],"default_reasoning_effort":"max"},
	 "non-thinking":{"litellm_provider":"openai","mode":"chat","supports_reasoning":false}
	}`)
	info := LookupModelCapabilityInfo("auto", "canonical-alias")
	if info == nil || info.ID != "auto" || info.ContextLength != 200000 || info.MaxCompletionTokens != 8192 || !reflect.DeepEqual(info.Thinking.Levels, []string{"max"}) {
		t.Fatalf("alias fallback failed: %+v", info)
	}
	info.Thinking.Levels[0] = "changed"
	if LookupModelCapabilityInfo("auto", "canonical-alias").Thinking.Levels[0] != "max" {
		t.Fatal("alias metadata shared mutable storage")
	}
	primary := LookupModelCapabilityInfo("actual-model", "canonical-alias")
	if primary.ID != "actual-model" || primary.ContextLength != 64000 || primary.MaxCompletionTokens != 8192 || !reflect.DeepEqual(primary.Thinking.Levels, []string{"low", "high"}) {
		t.Fatalf("alias overwrote known primary fields: %+v", primary)
	}
	if disabled := LookupModelCapabilityInfo("non-thinking", "canonical-alias"); disabled.Thinking == nil || len(disabled.Thinking.Levels) != 0 {
		t.Fatal("explicit unsupported reasoning was replaced by alias")
	}
	if LookupModelCapabilityInfo("unknown", "", "unknown-alias") != nil {
		t.Fatal("unknown IDs invented capability metadata")
	}
}
