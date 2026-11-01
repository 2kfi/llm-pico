package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tools, assistant tool_calls and tool results must survive the translation:
// OpenAI keys function results by call id, Gemini by function name.
func TestToGeminiMapsToolsAndToolMessages(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"weather and time in Paris?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp_c\":21}"},
			{"role":"tool","tool_call_id":"call_2","content":"noon"}
		],
		"tools":[{"type":"function","function":{
			"name":"get_weather",
			"description":"current weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}
		}}],
		"tool_choice":"required"
	}`)
	out, err := toGemini(body)
	if err != nil {
		t.Fatal(err)
	}

	if out.SystemInstruction == nil || out.SystemInstruction.Parts[0].Text != "be terse" {
		t.Fatalf("system instruction = %+v", out.SystemInstruction)
	}
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v, want one declaration", out.Tools)
	}
	decl := out.Tools[0].FunctionDeclarations[0]
	if decl.Name != "get_weather" || decl.Description != "current weather" {
		t.Errorf("declaration = %+v", decl)
	}
	// The caller's JSON Schema is forwarded intact, including the keywords the
	// older OpenAPI-subset parameters field would reject.
	if !strings.Contains(string(decl.ParametersJSONSchema), `"additionalProperties":false`) {
		t.Errorf("parametersJsonSchema = %s, want the schema passed through", decl.ParametersJSONSchema)
	}
	if out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Errorf("toolConfig = %+v, want ANY from tool_choice required", out.ToolConfig)
	}

	if len(out.Contents) != 3 {
		t.Fatalf("contents = %d, want user, model and merged tool results: %+v", len(out.Contents), out.Contents)
	}
	model := out.Contents[1]
	if model.Role != "model" || len(model.Parts) != 2 {
		t.Fatalf("assistant turn = %+v, want two functionCall parts", model)
	}
	if call := model.Parts[0].FunctionCall; call == nil || call.Name != "get_weather" || string(call.Args) != `{"city":"Paris"}` {
		t.Errorf("first call = %+v", call)
	}

	tools := out.Contents[2]
	if tools.Role != "user" || len(tools.Parts) != 2 {
		t.Fatalf("tool turn = %+v, want two functionResponse parts in one user turn", tools)
	}
	first := tools.Parts[0].FunctionResponse
	if first == nil || first.Name != "get_weather" {
		t.Fatalf("first functionResponse = %+v, want the resolved name", first)
	}
	if string(first.Response) != `{"temp_c":21}` {
		t.Errorf("response = %s, want the JSON object passed through", first.Response)
	}
	second := tools.Parts[1].FunctionResponse
	if second == nil || second.Name != "get_time" || string(second.Response) != `{"result":"noon"}` {
		t.Errorf("second functionResponse = %+v, want a wrapped plain-text result", second)
	}
}

func TestToGeminiToolChoiceModes(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`
	cases := []struct {
		name       string
		choice     string
		wantConfig bool
		wantMode   string
		wantName   string
	}{
		{"auto is the default", `"tool_choice":"auto"`, false, "", ""},
		{"none disables calls", `"tool_choice":"none"`, true, "NONE", ""},
		{"required forces a call", `"tool_choice":"required"`, true, "ANY", ""},
		{"specific function", `"tool_choice":{"type":"function","function":{"name":"f"}}`, true, "ANY", "f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := toGemini([]byte(`{"messages":[],` + tools + `,` + tc.choice + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantConfig {
				if out.ToolConfig != nil {
					t.Fatalf("toolConfig = %+v, want none", out.ToolConfig)
				}
				return
			}
			if out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig.Mode != tc.wantMode {
				t.Fatalf("toolConfig = %+v, want mode %s", out.ToolConfig, tc.wantMode)
			}
			if tc.wantName != "" {
				names := out.ToolConfig.FunctionCallingConfig.AllowedFunctionNames
				if len(names) != 1 || names[0] != tc.wantName {
					t.Errorf("allowedFunctionNames = %v, want [%s]", names, tc.wantName)
				}
			}
		})
	}
}

// Non-function tools have no Gemini equivalent and must be skipped silently.
func TestToGeminiSkipsNonFunctionTools(t *testing.T) {
	out, err := toGemini([]byte(`{"messages":[],"tools":[{"type":"web_search"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 0 {
		t.Errorf("tools = %+v, want none", out.Tools)
	}
}

// "null" is a legal argument string for a tool that takes no parameters, but
// Gemini's functionCall.args is a Struct and null is not one.
func TestToolArgumentsNormalization(t *testing.T) {
	cases := map[string]string{
		"":         `{}`,
		"null":     `{}`,
		`{"a":1}`:  `{"a":1}`,
		"[1,2]":    `{"value":[1,2]}`,
		"not json": `{}`,
	}
	for in, want := range cases {
		if got := string(toolArguments(in)); got != want {
			t.Errorf("toolArguments(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestToGeminiJSONMode(t *testing.T) {
	cases := []struct {
		name       string
		format     string
		wantMime   string
		wantSchema bool
	}{
		{"json_object sets the MIME type", `{"type":"json_object"}`, "application/json", false},
		{"text leaves JSON mode off", `{"type":"text"}`, "", false},
		{"json_schema forwards the schema", `{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}}}`, "application/json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := toGemini([]byte(`{"messages":[],"response_format":` + tc.format + `}`))
			if err != nil {
				t.Fatal(err)
			}
			cfg := out.GenerationConfig
			if cfg.ResponseMimeType != tc.wantMime {
				t.Errorf("responseMimeType = %q, want %q", cfg.ResponseMimeType, tc.wantMime)
			}
			if got := len(cfg.ResponseJSONSchema) > 0; got != tc.wantSchema {
				t.Errorf("responseJsonSchema present = %v, want %v (%s)", got, tc.wantSchema, cfg.ResponseJSONSchema)
			}
		})
	}
}

// A functionCall reply becomes an OpenAI assistant message with tool_calls and
// finish_reason tool_calls; content is null when the model only called a tool.
func TestGeminiCompleteReturnsToolCalls(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[
			{"thought":true,"text":"internal reasoning"},
			{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}
		]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"weather?"}],
			"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
			"tool_choice":"auto"}`),
		Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The outbound payload carries the declarations and no forced mode.
	tools, _ := request["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools = %v, want one", request["tools"])
	}
	if _, ok := request["toolConfig"]; ok {
		t.Errorf("toolConfig = %v, want it omitted for auto", request["toolConfig"])
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	choice := out.Choices[0]
	if string(choice.Message.Content) != "null" {
		t.Errorf("content = %s, want null for a call-only reply (thought parts must not leak)", choice.Message.Content)
	}
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID == "" || !strings.HasPrefix(call.ID, "call_") || call.Type != "function" {
		t.Errorf("call envelope = %+v", call)
	}
	if call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("call = %+v, want name and JSON-string arguments", call.Function)
	}
}

func TestGeminiStreamEmitsToolCallDeltas(t *testing.T) {
	events := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}]}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"weather?"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawCall, sawFinish bool
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Data, &out); err != nil {
			t.Fatal(err)
		}
		for _, c := range out.Choices {
			if len(c.Delta.ToolCalls) > 0 {
				call := c.Delta.ToolCalls[0]
				if call.Index != 0 || call.Type != "function" || call.Function.Name != "get_weather" {
					t.Errorf("streamed tool call = %+v", call)
				}
				if call.Function.Arguments != `{"city":"Paris"}` {
					t.Errorf("arguments = %q, want the JSON string", call.Function.Arguments)
				}
				sawCall = true
			}
			if reason, ok := c.FinishReason.(string); ok && reason == "tool_calls" {
				sawFinish = true
			}
		}
	}
	if !sawCall {
		t.Error("no tool_call delta was emitted")
	}
	if !sawFinish {
		t.Error("no tool_calls finish_reason was emitted")
	}
}
