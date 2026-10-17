package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterLocalizesModelRequest(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "模型请求"
	entry.Data = log.Fields{
		"client_ip": "203.0.113.5", "provider": "openai-compatible-light-ai",
		"model": "auto", "upstream_reasoning_effort": "unspecified", "upstream_thinking": "unspecified",
		"requested_model": "glm-5.3-flash", "requested_reasoning_effort": "high",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := `模型请求 来源IP="203.0.113.5" 渠道="openai-compatible-light-ai" 上游模型="auto" 上游思考等级="未指定" 上游思考状态="未指定" 请求模型="glm-5.3-flash" 请求思考等级="高(high)"`
	if !strings.HasSuffix(string(formatted), want+"\n") {
		t.Fatalf("formatted log = %s, want suffix %s", formatted, want)
	}
}

func TestModelRequestLogPreservesLevelsAndEscapesValues(t *testing.T) {
	tests := []struct {
		key, value, want string
	}{
		{"upstream_reasoning_effort", "low", `"低(low)"`},
		{"upstream_reasoning_effort", "max", `"最高(max)"`},
		{"upstream_reasoning_effort", "none", `"关闭(none)"`},
		{"upstream_reasoning_effort", "custom-level", `"custom-level"`},
		{"upstream_thinking", "enabled", `"开启"`},
		{"upstream_thinking", "disabled", `"关闭"`},
		{"client_ip", "unknown", `"未知"`},
		{"provider", "channel\nforged log", `"channel\nforged log"`},
	}
	for _, tc := range tests {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			_, got := formatModelRequestLogField(tc.key, tc.value)
			if got != tc.want {
				t.Errorf("formatted value = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLogFormatterKeepsOtherMessagesUnchanged(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "another message"
	entry.Data = log.Fields{"model": "model-id", "upstream_reasoning_effort": "high"}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), `another message model=model-id upstream_reasoning_effort="high"`) {
		t.Fatalf("unrelated log changed: %s", formatted)
	}
}
