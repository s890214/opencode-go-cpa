package shared

import (
	"encoding/json"
	"testing"
)

func TestPlanChatToolOutputMovesImages(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"screenshot:"},{"type":"input_image","image_url":"https://example.test/shot.png"}]`)
	plan, eErr := PlanChatToolOutput(raw, "tool messages carry text only")
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if plan == nil {
		t.Fatal("expected a media plan")
	}
	if plan.Text != "screenshot:"+ToolMediaMovedMarker {
		t.Fatalf("tool text = %q", plan.Text)
	}
	if len(plan.Media) != 1 {
		t.Fatalf("media parts = %d", len(plan.Media))
	}
	if plan.Media[0]["type"] != "image_url" {
		t.Fatalf("media part type = %v", plan.Media[0]["type"])
	}
}

func TestPlanChatToolOutputTextOnlyUnchanged(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"ok"},{"type":"output_text","text":" more"}]`)
	plan, eErr := PlanChatToolOutput(raw, "tool messages carry text only")
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if plan != nil {
		t.Fatal("text-only output must keep the legacy path (nil plan)")
	}
}

func TestPlanChatToolOutputAcceptsImageObjectURL(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`)
	plan, eErr := PlanChatToolOutput(raw, "tool messages carry text only")
	if eErr != nil || plan == nil || len(plan.Media) != 1 {
		t.Fatalf("plan=%v err=%v", plan, eErr)
	}
}

func TestPlanChatToolOutputMovesFiles(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"pdf:"},{"type":"input_file","filename":"a.pdf","file_data":"JVBERi0="}]`)
	plan, eErr := PlanChatToolOutput(raw, "tool messages carry text only")
	if eErr != nil || plan == nil || len(plan.Media) != 1 {
		t.Fatalf("plan=%v err=%v", plan, eErr)
	}
	if plan.Media[0]["type"] != "file" {
		t.Fatalf("media type = %v", plan.Media[0]["type"])
	}
}

func TestPlanChatToolOutputRejectsUnknownPart(t *testing.T) {
	raw := json.RawMessage(`[{"type":"mystery_block"}]`)
	if _, eErr := PlanChatToolOutput(raw, "tool messages carry text only"); eErr == nil {
		t.Fatal("unknown part type must be rejected, not silently dropped")
	}
}

func TestPlanChatToolOutputScalarStringStaysText(t *testing.T) {
	raw := json.RawMessage(`"plain tool output"`)
	plan, eErr := PlanChatToolOutput(raw, "tool messages carry text only")
	if eErr != nil || plan != nil {
		t.Fatalf("scalar text must keep the legacy path: plan=%v err=%v", plan, eErr)
	}
}
