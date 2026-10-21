package shared

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"opencode-go-cpa/internal/errclass"
)

func TestToolResultPartsTextParity(t *testing.T) {
	for _, raw := range []string{
		`null`, `""`, `"plain"`, `[]`,
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`,
		`[{"type":"text","text":"before"},{"type":"tool_reference","tool_name":"web_search"},{"type":"tool_reference"},{"type":"text","text":"after"}]`,
	} {
		t.Run(raw, func(t *testing.T) {
			want, err := ToolResultText(json.RawMessage(raw), "test")
			if err != nil {
				t.Fatal(err)
			}
			parts, err := ToolResultParts(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			for _, p := range parts {
				if p.ImageURL != "" {
					t.Fatalf("unexpected image: %+v", p)
				}
				got.WriteString(p.Text)
			}
			if got.String() != want {
				t.Fatalf("text behavior changed: got %q, want %q", got.String(), want)
			}
		})
	}
}

func TestToolResultPartsMixedContent(t *testing.T) {
	raw := json.RawMessage(`[
  {"type":"text","text":"caption"},
  {"type":"tool_reference","tool_name":"web_search"},
  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},
  {"type":"tool_reference"},
  {"type":"image","source":{"type":"url","url":"https://example.com/image.png"}},
  {"type":"text","text":"tail"}
 ]`)
	got, err := ToolResultParts(raw)
	want := []ContentPart{
		{Text: "caption"}, {Text: "[tool: web_search]"}, {ImageURL: "data:image/png;base64,QUJD"},
		{Text: "[tool reference]"}, {ImageURL: "https://example.com/image.png"}, {Text: "tail"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed content = %+v, %v; want %+v", got, err, want)
	}
}

func TestToolResultPartsRejectsInvalidContent(t *testing.T) {
	for _, raw := range []string{
		`{`, `42`, `{}`, `[42]`, `[null]`,
		`[{"type":"video"}]`, `[{"type":"image"}]`,
		`[{"type":"image","source":"invalid"}]`,
		`[{"type":"image","source":{"type":"base64"}}]`,
		`[{"type":"image","source":{"type":"url"}}]`,
		`[{"type":"image","source":{"type":"s3"}}]`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ToolResultParts(json.RawMessage(raw)); err == nil || err.Class != errclass.ClassTranslation {
				t.Fatalf("invalid content accepted: %v", err)
			}
		})
	}
}
