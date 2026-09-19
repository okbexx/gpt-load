package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// CPA is an offline TEST oracle only. Production Pi counting must not invoke it.
func TestPiCountOfflineBaselineParity(t *testing.T) {
	fixtures := []struct{ name, format, body string }{
		{"responses-empty", "openai-response", `{}`},
		{"responses-null", "openai-response", `{"input":null}`},
		{"responses-string", "openai-response", `{"input":"  hello 世界 👩‍💻\n café  ","instructions":"  be precise  "}`},
		{"responses-untyped", "openai-response", `{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"world"}]}]}`},
		{"responses-typed", "openai-response", `{"instructions":" \n ","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":" first "},{"type":"text","text":" \n "}]},{"type":"message","role":"assistant","content":[{"type":"text","text":"second"}]}]}`},
		{"responses-string-content", "openai-response", `{"input":[{"type":"message","role":"user","content":"text"}]}`},
		{"claude-empty", "claude", `{}`},
		{"claude-string", "claude", `{"system":" be precise ","messages":[{"role":"user","content":" hello 世界 👩‍💻 "},{"role":"assistant","content":"café"}]}`},
		{"claude-parts", "claude", `{"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":[{"type":"text","text":" a "},{"type":"text","text":" "},{"type":"text","text":"b"}]}]}`},
		{"claude-roles", "claude", `{"messages":[{"role":"system","content":"notice"},{"role":"developer","content":"dev"},{"content":"missing"},{"role":"other","content":"other"}]}`},
		{"claude-attribution", "claude", `{"system":"x-anthropic-billing-header: cc_version=1;","messages":[]}`},
		{"gemini-empty", "gemini", `{}`},
		{"gemini-parts", "gemini", `{"contents":[{"role":"user","parts":[{"text":" hello 世界 👩‍💻 "},{"text":" "}]},{"role":"model","parts":[{"text":"café"},{"text":"終"}]}]}`},
		{"gemini-roles", "gemini", `{"contents":[{"parts":[{"text":"missing"}]},{"role":"system","parts":[{"text":"notice"}]}]}`},
	}
	models := []string{"gpt-5", "gpt-5.2", "gpt-4.1", "gpt-4o", "gpt-4", "gpt-3.5-turbo", "gpt-3"}
	oracle := NewExecutor()
	for _, model := range models {
		for _, f := range fixtures {
			t.Run(model+"/"+f.name, func(t *testing.T) {
				q := ExecuteRequest{Model: model, Format: f.format, Payload: []byte(f.body)}
				before := string(q.Payload)
				want, err := oracle.CountTokens(context.Background(), "offline-parity", Credential{}, q)
				if err != nil {
					t.Fatal(err)
				}
				got, err := EstimatePiTokens(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				var a, b any
				if json.Unmarshal(want.Payload, &a) != nil || json.Unmarshal(got.Payload, &b) != nil || !reflect.DeepEqual(a, b) {
					t.Fatalf("got %s; offline CPA oracle %s", got.Payload, want.Payload)
				}
				if string(q.Payload) != before {
					t.Fatal("mutated input")
				}
			})
		}
	}
}

func TestPiCountRejectsUnsupportedDirectCalls(t *testing.T) {
	for _, q := range []ExecuteRequest{
		{Model: "unknown", Format: "openai-response", Payload: []byte(`{"input":"hi"}`)},
		{Model: "gpt-5", Format: "openai", Payload: []byte(`{}`)},
		{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"tools":[]}`)},
		{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":[{"content":[{"type":"input_image","image_url":"x"}]}]}`)},
		{Model: "gpt-5", Format: "claude", Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","unknown":true}]}]}`)},
		{Model: "gpt-5", Format: "gemini", Payload: []byte(`{"contents":[{"parts":[{"inlineData":{}}]}]}`)},
		{Model: "gpt-5", Format: "gemini", Payload: []byte(`{"systemInstruction":{}}`)},
		{Model: "gpt-5", Format: "openai-response", Payload: []byte(`null`)},
		{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{`)},
	} {
		if _, err := EstimatePiTokens(context.Background(), q); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
}

// Gate cancellation at an in-flight checkpoint without sleep-based timing or
// depending on tokenizer speed. The estimator itself starts no goroutines.
type piCountGatedContext struct {
	context.Context
	calls   atomic.Int32
	reached chan struct{}
}

func (c *piCountGatedContext) Err() error {
	if c.calls.Add(1) == 2 {
		close(c.reached)
		<-c.Done()
	}
	return c.Context.Err()
}
func TestPiCountInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gated := &piCountGatedContext{Context: ctx, reached: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := EstimatePiTokens(gated, ExecuteRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"hello"}`)})
		result <- err
	}()
	select {
	case <-gated.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("no in-flight checkpoint")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("count did not return after cancellation")
	}
}

func TestPiCountCancellationAndConcurrency(t *testing.T) {
	q := ExecuteRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"hello"}`)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EstimatePiTokens(ctx, q); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := EstimatePiTokens(context.Background(), q); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	q.Payload = []byte(`{"input":"` + strings.Repeat("x", piMaxRequestBytes) + `"}`)
	if _, err := EstimatePiTokens(context.Background(), q); err == nil {
		t.Fatal("unbounded request accepted")
	}
}
