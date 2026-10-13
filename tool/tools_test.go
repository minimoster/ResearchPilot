package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFakeLLMEncodesArgumentsAndHandlesEmptyHistory(t *testing.T) {
	if got := FakeLLM(nil); got.ToolCall != nil {
		t.Fatal("empty history invoked tool")
	}
	query := "quotes: \"hello\"\nbackslash: \\"
	response := FakeLLM([]Message{{Role: "user", Content: query}})
	if response.ToolCall == nil {
		t.Fatal("missing search call")
	}
	var input SearchInput
	if err := json.Unmarshal(response.ToolCall.Arguments, &input); err != nil || input.Query != query {
		t.Fatalf("invalid arguments: %s, %v", response.ToolCall.Arguments, err)
	}
}

func TestToolsRejectInvalidArguments(t *testing.T) {
	for _, tool := range []Tool{SearchTool{}, DeepResearchTool{}, AIAnswerTool{}} {
		for _, input := range []string{`{"query":`, `{"query":123}`, `{}`, `null`, `{"query":"  "}`, `{"query":"ok","query":3}`} {
			if _, err := tool.Execute(context.Background(), json.RawMessage(input)); err == nil {
				t.Errorf("%s accepted %s", tool.Definition().Name, input)
			}
		}
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"test"}`)); err != nil {
			t.Fatal(err)
		}
	}
}
