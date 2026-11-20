package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runTestStream(t *testing.T, sse string) []testSSEEvent {
	t.Helper()
	ps, _ := newTestResponsesProxy(t, "http://127.0.0.1:1")
	w := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	ps.streamResponsesSSE(w, resp, "gpt", "claude-sonnet-5", 42)
	return parseTestSSE(t, w.Body.String())
}

const streamTextFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_abc","model":"global.openai.gpt-6-luna","status":"in_progress","output":[],"usage":null}}

event: response.in_progress
data: {"type":"response.in_progress","response":{"id":"resp_abc","model":"global.openai.gpt-6-luna","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":1,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","content_index":0,"delta":"!"}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":1,"item_id":"msg_1","content_index":0,"text":"Hello!"}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":1,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":"Hello!"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello!"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_abc","model":"global.openai.gpt-6-luna","status":"completed","output":[{"type":"reasoning","id":"rs_1"},{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Hello!"}]}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"output_tokens":20}}}

`

func TestStreamText(t *testing.T) {
	events := runTestStream(t, streamTextFixture)

	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := strings.Join(eventTypes(events), ","); got != want {
		t.Fatalf("event sequence = %s\nwant %s", got, want)
	}

	start := firstEventOfType(events, "message_start")
	msg := start["message"].(map[string]interface{})
	if msg["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v, want original client model", msg["model"])
	}
	if msg["id"] != "msg_abc" {
		t.Errorf("id = %v", msg["id"])
	}
	usage := msg["usage"].(map[string]interface{})
	if usage["input_tokens"].(float64) != 42 {
		t.Errorf("message_start input_tokens = %v, want estimate 42", usage["input_tokens"])
	}

	blockStart := firstEventOfType(events, "content_block_start")
	block := blockStart["content_block"].(map[string]interface{})
	if blockStart["index"].(float64) != 0 || block["type"] != "text" {
		t.Errorf("content_block_start = %v", blockStart)
	}

	if text := collectTextDeltas(events); text != "Hello!" {
		t.Errorf("text = %q", text)
	}

	delta := firstEventOfType(events, "message_delta")
	if delta["delta"].(map[string]interface{})["stop_reason"] != "end_turn" {
		t.Errorf("message_delta = %v", delta)
	}
	finalUsage := delta["usage"].(map[string]interface{})
	if finalUsage["input_tokens"].(float64) != 50 {
		t.Errorf("final input_tokens = %v, want 100-40-10", finalUsage["input_tokens"])
	}
	if finalUsage["output_tokens"].(float64) != 20 {
		t.Errorf("final output_tokens = %v", finalUsage["output_tokens"])
	}
}

const streamToolFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_tool","model":"global.openai.gpt-6-luna","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"\"Beijing\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_1","arguments":"{\"city\":\"Beijing\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Beijing\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_tool","model":"global.openai.gpt-6-luna","status":"completed","output":[{"type":"reasoning","id":"rs_1"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}],"usage":{"input_tokens":80,"output_tokens":10}}}

`

func TestStreamToolCall(t *testing.T) {
	events := runTestStream(t, streamToolFixture)

	if got := strings.Join(eventTypes(events), ","); got != "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Fatalf("event sequence = %s", got)
	}

	blockStart := firstEventOfType(events, "content_block_start")
	block := blockStart["content_block"].(map[string]interface{})
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", block)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(collectJSONDeltas(events)), &args); err != nil {
		t.Fatalf("tool args JSON invalid: %v (%q)", err, collectJSONDeltas(events))
	}
	if args["city"] != "Beijing" {
		t.Errorf("args = %v", args)
	}
	if firstEventOfType(events, "message_delta")["delta"].(map[string]interface{})["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason must be tool_use")
	}
}

const streamToolNoAddedFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_tool2","model":"global.openai.gpt-6-luna","status":"in_progress","output":[]}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_9","delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_9","delta":"\"Shanghai\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_9","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"Shanghai\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_tool2","status":"completed","output":[],"usage":{"input_tokens":50,"output_tokens":8}}}

`

func TestStreamToolCallWithoutAddedEvent(t *testing.T) {
	events := runTestStream(t, streamToolNoAddedFixture)

	blockStart := firstEventOfType(events, "content_block_start")
	block := blockStart["content_block"].(map[string]interface{})
	if block["type"] != "tool_use" || block["id"] != "call_9" {
		t.Errorf("tool_use block = %v (args should be buffered until call_id is known)", block)
	}
	if args := collectJSONDeltas(events); args != `{"city":"Shanghai"}` {
		t.Errorf("buffered args = %q", args)
	}
}

const streamTruncateFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_trunc","model":"global.openai.gpt-6-luna","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"resp_trunc","model":"global.openai.gpt-6-luna","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1"}],"usage":{"input_tokens":16,"output_tokens":16}}}

`

func TestStreamTruncatedResponse(t *testing.T) {
	events := runTestStream(t, streamTruncateFixture)

	if got := strings.Join(eventTypes(events), ","); got != "message_start,content_block_start,content_block_stop,message_delta,message_stop" {
		t.Fatalf("event sequence = %s", got)
	}
	block := firstEventOfType(events, "content_block_start")["content_block"].(map[string]interface{})
	if block["type"] != "text" || block["text"] != "" {
		t.Errorf("empty text block expected, got %v", block)
	}
	if firstEventOfType(events, "message_delta")["delta"].(map[string]interface{})["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason must be max_tokens")
	}
}

const streamDoneOnlyFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_done","model":"global.openai.gpt-6-luna","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","part":{"type":"output_text","text":""}}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"fallback text"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_done","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3}}}

`

func TestStreamDoneOnlyFallback(t *testing.T) {
	events := runTestStream(t, streamDoneOnlyFixture)
	if text := collectTextDeltas(events); text != "fallback text" {
		t.Errorf("text = %q, want synthesized delta from item.done", text)
	}
}

const streamFailedFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_fail","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"partial"}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_fail","status":"failed","error":{"code":"server_error","message":"boom"}}}

`

func TestStreamFailure(t *testing.T) {
	events := runTestStream(t, streamFailedFixture)

	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %s, want error", last.Name)
	}
	errObj := last.Data["error"].(map[string]interface{})
	if errObj["message"] != "boom" {
		t.Errorf("error = %v", errObj)
	}
	for _, ev := range events {
		if ev.Name == "message_stop" {
			t.Error("message_stop must not follow an error event")
		}
	}
}

const streamUnterminatedFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_cut","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"cut off"}

`

func TestStreamUnterminatedClosesMessage(t *testing.T) {
	events := runTestStream(t, streamUnterminatedFixture)

	types := strings.Join(eventTypes(events), ",")
	if !strings.HasSuffix(types, "message_delta,message_stop") {
		t.Fatalf("unterminated stream should still close the message, got %s", types)
	}
	if firstEventOfType(events, "message_delta")["delta"].(map[string]interface{})["stop_reason"] != "end_turn" {
		t.Error("fallback stop_reason must be end_turn")
	}
}
