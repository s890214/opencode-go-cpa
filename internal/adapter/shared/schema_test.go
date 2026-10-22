package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

// CleanSchema is the guard that keeps tool schemas acceptable to strict
// Chat/Responses backends. These cases pin the behaviours that upstream
// rejections depend on.
func TestCleanSchemaFillsRootObject(t *testing.T) {
	out := string(CleanSchema(json.RawMessage(`{"properties":{"a":{"type":"string"}}}`)))
	if !strings.Contains(out, `"type":"object"`) || !strings.Contains(out, `"properties"`) {
		t.Fatalf("root object not filled: %s", out)
	}
}

func TestCleanSchemaDropsURIfFormat(t *testing.T) {
	out := string(CleanSchema(json.RawMessage(`{"type":"object","properties":{"u":{"type":"string","format":"uri"}}}`)))
	if strings.Contains(out, `"format":"uri"`) {
		t.Fatalf("format:uri must be dropped: %s", out)
	}
}

func TestCleanSchemaRecursesIntoItems(t *testing.T) {
	out := string(CleanSchema(json.RawMessage(`{"type":"object","properties":{"xs":{"type":"array","items":{"type":"string","format":"uri"}}}}`)))
	if strings.Contains(out, `"format":"uri"`) {
		t.Fatalf("nested items not cleaned: %s", out)
	}
}

func TestCleanSchemaEmptyBecomesEmptyObject(t *testing.T) {
	out := string(CleanSchema(nil))
	if !strings.Contains(out, `"type":"object"`) || !strings.Contains(out, `"properties"`) {
		t.Fatalf("empty schema = %s", out)
	}
}

// InvalidToolInput is the degradation target for malformed arguments: the raw
// text must survive inside an object, because Anthropic tool_use.input has to
// be an object and losing the payload loses the model's only output.
func TestInvalidToolInputPreservesRawText(t *testing.T) {
	v := InvalidToolInput(`{oops "x": `)
	if v["input"] != `{oops "x": ` {
		t.Fatalf("raw payload lost: %#v", v)
	}
	b, err := json.Marshal(v)
	if err != nil || !json.Valid(b) {
		t.Fatalf("degraded input is not valid JSON: %s", b)
	}
}
