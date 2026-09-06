package sharing

import (
	"encoding/json"
	moirai "github.com/october-dev/moirai"
	"strings"
	"testing"
)

func TestPreparePreservesSourceAndToolPairs(t *testing.T) {
	secret := "ghp_" + strings.Repeat("x", 30)
	source := &moirai.Transcript{SchemaVersion: "1.0", Meta: moirai.Metadata{ID: "test", CWD: "/private/work"}, Messages: []moirai.Message{
		{Role: moirai.RoleUser, Content: []moirai.Block{{Type: moirai.BlockText, Text: "hello " + secret}}},
		{Role: moirai.RoleAssistant, Content: []moirai.Block{{Type: moirai.BlockThinking, Text: "sensitive thinking"}, {Type: moirai.BlockToolUse, ID: "call", Name: "shell", Input: json.RawMessage(`{"command":"echo hello"}`)}}},
		{Role: moirai.RoleUser, Content: []moirai.Block{{Type: moirai.BlockToolResult, ToolUseID: "call", Content: json.RawMessage(`"hello"`)}}},
	}}
	prepared, report, err := Prepare(source, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(prepared)
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "sensitive thinking") || strings.Contains(string(data), "/private/work") {
		t.Fatal("prepared content leaked")
	}
	if source.Meta.CWD != "/private/work" || source.Messages[0].Content[0].Text != "hello "+secret {
		t.Fatal("source modified")
	}
	if report.ThinkingRemoved != 1 || report.SecretMatches != 1 {
		t.Fatalf("report: %+v", report)
	}
	if err = moirai.Validate(prepared, moirai.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}
