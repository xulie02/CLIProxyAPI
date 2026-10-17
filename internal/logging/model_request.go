package logging

import "strconv"

var modelRequestLogFieldNames = map[string]string{
	"client_ip":                  "来源IP",
	"provider":                   "渠道",
	"model":                      "上游模型",
	"upstream_reasoning_effort":  "上游思考等级",
	"upstream_thinking":          "上游思考状态",
	"requested_model":            "请求模型",
	"requested_reasoning_effort": "请求思考等级",
}

var modelRequestEffortNames = map[string]string{
	"unspecified": "未指定",
	"none":        "关闭(none)",
	"minimal":     "最低(minimal)",
	"low":         "低(low)",
	"medium":      "中(medium)",
	"high":        "高(high)",
	"xhigh":       "极高(xhigh)",
	"max":         "最高(max)",
	"auto":        "自动(auto)",
}

func formatModelRequestLogField(key string, value any) (string, string) {
	name, ok := modelRequestLogFieldNames[key]
	if !ok {
		return key, formatLogFieldValue(key, value)
	}
	text, ok := value.(string)
	if !ok {
		return name, formatLogFieldValue(key, value)
	}
	switch key {
	case "upstream_reasoning_effort", "requested_reasoning_effort":
		if translated, found := modelRequestEffortNames[text]; found {
			text = translated
		}
	case "upstream_thinking":
		switch text {
		case "unspecified":
			text = "未指定"
		case "enabled":
			text = "开启"
		case "disabled":
			text = "关闭"
		case "adaptive":
			text = "自适应"
		}
	case "client_ip", "requested_model":
		if text == "unknown" {
			text = "未知"
		}
	}
	return name, strconv.Quote(text)
}
