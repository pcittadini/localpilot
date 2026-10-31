package models

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeOllama(t *testing.T, handler func(w http.ResponseWriter, path string, body map[string]any)) *Ollama {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				t.Errorf("request is not JSON: %v", err)
			}
		}
		handler(w, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	o, err := NewOllama(srv.URL+"/", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestChatEncodesRequest(t *testing.T) {
	var got map[string]any
	o := fakeOllama(t, func(w http.ResponseWriter, path string, body map[string]any) {
		if path != "/api/chat" {
			t.Errorf("path = %s", path)
		}
		got = body
		io.WriteString(w, `{"model":"m","message":{"role":"assistant","content":"{}"},"done":true,"done_reason":"stop","prompt_eval_count":12,"eval_count":3}`)
	})

	think := false
	_, err := o.Chat(context.Background(), ChatRequest{
		Model:  "qwen3.5:9b",
		System: "be brief",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}}},
			{Role: RoleTool, ToolCallID: "c1", ToolName: "read_file", Content: "package a"},
		},
		Tools:          []ToolDefinition{{Name: "read_file", Description: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Temperature:    0.1,
		MaxTokens:      256,
		ContextWindow:  8192,
		Think:          &think,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got["model"] != "qwen3.5:9b" || got["stream"] != false || got["think"] != false {
		t.Errorf("top-level fields: %v", got)
	}
	if f, ok := got["format"].(map[string]any); !ok || f["type"] != "object" {
		t.Errorf("format = %v", got["format"])
	}
	opts := got["options"].(map[string]any)
	if opts["temperature"] != 0.1 || opts["num_predict"] != 256.0 || opts["num_ctx"] != 8192.0 {
		t.Errorf("options = %v", opts)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	call := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "c1" || call["function"].(map[string]any)["arguments"].(map[string]any)["path"] != "a.go" {
		t.Errorf("tool call = %v", call)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_name"] != "read_file" || toolMsg["tool_call_id"] != "c1" {
		t.Errorf("tool message = %v", toolMsg)
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["function"].(map[string]any)["name"] != "read_file" {
		t.Errorf("tools = %v", tool)
	}
}

func TestChatOmitsUnsetOptions(t *testing.T) {
	var got map[string]any
	o := fakeOllama(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		got = body
		io.WriteString(w, `{"message":{"content":"ok"},"done_reason":"stop"}`)
	})
	if _, err := o.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"think", "format", "tools"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should be omitted: %v", k, got)
		}
	}
	opts := got["options"].(map[string]any)
	if _, ok := opts["num_predict"]; ok {
		t.Errorf("num_predict should be omitted: %v", opts)
	}
	if _, ok := opts["num_ctx"]; ok {
		t.Errorf("num_ctx should be omitted: %v", opts)
	}
}

func TestChatDecodesResponse(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		finish FinishReason
		calls  int
	}{
		{"stop", `{"model":"m","message":{"content":"done"},"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`, FinishStop, 0},
		{"length", `{"message":{"content":"trunc"},"done_reason":"length","prompt_eval_count":10,"eval_count":4}`, FinishLength, 0},
		{"tool calls", `{"message":{"content":"","tool_calls":[{"id":"call_1","function":{"index":0,"name":"read_file","arguments":{"path":"x"}}}]},"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`, FinishToolCalls, 1},
		{"other", `{"message":{"content":"?"},"done_reason":"unload","prompt_eval_count":10,"eval_count":4}`, FinishOther, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := fakeOllama(t, func(w http.ResponseWriter, _ string, _ map[string]any) { io.WriteString(w, tc.body) })
			resp, err := o.Chat(context.Background(), ChatRequest{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if resp.FinishReason != tc.finish || len(resp.Message.ToolCalls) != tc.calls {
				t.Fatalf("resp = %+v", resp)
			}
			if resp.Usage != (Usage{InputTokens: 10, OutputTokens: 4}) {
				t.Errorf("usage = %+v", resp.Usage)
			}
			if tc.calls > 0 {
				c := resp.Message.ToolCalls[0]
				if c.ID != "call_1" || c.Name != "read_file" || string(c.Arguments) != `{"path":"x"}` {
					t.Errorf("tool call = %+v", c)
				}
			}
		})
	}
}

func TestChatErrors(t *testing.T) {
	t.Run("model not found", func(t *testing.T) {
		o := fakeOllama(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"model 'x' not found"}`)
		})
		_, err := o.Chat(context.Background(), ChatRequest{Model: "x"})
		if !errors.Is(err, ErrModelNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		o := fakeOllama(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"out of memory"}`)
		})
		_, err := o.Chat(context.Background(), ChatRequest{Model: "x"})
		if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMalformedOutput) || !strings.Contains(err.Error(), "out of memory") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("malformed tool call", func(t *testing.T) {
		o := fakeOllama(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"XML syntax error on line 4: element <function> closed by </parameter>"}`)
		})
		_, err := o.Chat(context.Background(), ChatRequest{Model: "x"})
		if !errors.Is(err, ErrMalformedOutput) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		o, _ := NewOllama("http://"+addr, nil)
		_, err = o.Chat(context.Background(), ChatRequest{Model: "x"})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		o := fakeOllama(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
			time.Sleep(300 * time.Millisecond)
			io.WriteString(w, `{}`)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := o.Chat(ctx, ChatRequest{Model: "x"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("invalid base url", func(t *testing.T) {
		if _, err := NewOllama("localhost:11434", nil); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestDescribeAndVersion(t *testing.T) {
	o := fakeOllama(t, func(w http.ResponseWriter, path string, body map[string]any) {
		switch path {
		case "/api/show":
			if body["model"] != "qwen3.5:9b" {
				t.Errorf("show body = %v", body)
			}
			io.WriteString(w, `{"parameters":"top_k                          20\nnum_ctx                        8192\ntemperature 1","capabilities":["completion","tools"],"model_info":{"qwen35.context_length":262144},"details":{"family":"qwen35","parameter_size":"9.0B","quantization_level":"Q4_K_M"}}`)
		case "/api/version":
			io.WriteString(w, `{"version":"0.40.13"}`)
		}
	})
	info, err := o.Describe(context.Background(), "qwen3.5:9b")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Has(CapabilityTools) || info.Has(CapabilityThinking) || info.ServedContext != 8192 ||
		info.TrainedContext != 262144 || info.Quantization != "Q4_K_M" {
		t.Fatalf("info = %+v", info)
	}
	v, err := o.Version(context.Background())
	if err != nil || v != "0.40.13" {
		t.Fatalf("version = %q, %v", v, err)
	}
}
