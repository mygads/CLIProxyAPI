package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeToolResultContent(t *testing.T) {
	tests := []struct {
		name       string
		wrapper    string
		wantResult string
		wantRaw    bool
		wantImages int
	}{
		{"string", `{"content":"alpha"}`, "alpha", false, 0},
		{"single text block", `{"content":[{"type":"text","text":"alpha"}]}`, `{"type":"text","text":"alpha"}`, true, 0},
		{"multiple blocks", `{"content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}`, `[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]`, true, 0},
		{"text and image", `{"content":[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`, `{"type":"text","text":"alpha"}`, true, 1},
		{"image only", `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`, "", false, 1},
		{"image without data", `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}`, "", false, 0},
		{"object", `{"content":{"foo":"bar"}}`, `{"foo":"bar"}`, true, 0},
		{"absent", `{}`, "", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertClaudeToolResultContent(gjson.Get(tt.wrapper, "content"))
			if got.Result != tt.wantResult || got.ResultIsRaw != tt.wantRaw || len(got.Images) != tt.wantImages {
				t.Fatalf("got result=%q raw=%v images=%d, want result=%q raw=%v images=%d", got.Result, got.ResultIsRaw, len(got.Images), tt.wantResult, tt.wantRaw, tt.wantImages)
			}
		})
	}
}

func TestConvertClaudeToolResultContentImageFields(t *testing.T) {
	content := gjson.Get(`{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`, "content")
	got := ConvertClaudeToolResultContent(content)
	if len(got.Images) != 1 || got.Images[0].MimeType != "image/png" || got.Images[0].Data != "aGVsbG8=" {
		t.Fatalf("unexpected image extraction: %+v", got.Images)
	}
}
