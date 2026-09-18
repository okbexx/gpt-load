package codex

// The Pi bridge is an explicit experimental data-plane only. Credentials,
// refresh, account selection, quota accounting and retry remain GPT-Load owned.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const piMaxFrameBytes = 16 << 20
const piMaxRequestBytes = 8 << 20

// PiError never contains bridge bodies, transport errors, or credential data.
// Even a failed local transport is maybe_sent: the bridge may have dispatched.
type PiError struct {
	code, dispatch string
	status         int
}

func (e *PiError) Error() string          { return "experimental pi driver: " + e.code }
func (e *PiError) ErrorCode() string      { return e.code }
func (e *PiError) ErrorType() string      { return "pi_driver_error" }
func (e *PiError) StatusCode() int        { return e.status }
func (e *PiError) ConversionCode() string { return e.code }
func (e *PiError) DispatchState() string  { return e.dispatch }
func piError(code, state string) *PiError { return &PiError{code: code, dispatch: state} }

type piExecutor struct {
	endpoint, secret string
	client           *http.Client
}

// NewPiExecutor accepts only literal loopback HTTP endpoints. No DNS, redirects
// or environment proxies are used when transmitting the selected credential.
func NewPiExecutor(endpoint, secret string) (Executor, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, errors.New("PI_BRIDGE_URL must be an HTTP literal loopback origin with a port")
	}
	port, portErr := strconv.Atoi(u.Port())
	if portErr != nil || port < 1 || port > 65535 || u.ForceQuery || u.RawPath != "" {
		return nil, errors.New("invalid PI_BRIDGE_URL origin")
	}
	for _, ch := range secret {
		if ch < 33 || ch > 126 {
			return nil, errors.New("invalid PI_BRIDGE_SECRET")
		}
	}
	if len(secret) < 32 || len(secret) > 4096 || strings.ContainsAny(secret, " \t\r\n\x00") {
		return nil, errors.New("PI_BRIDGE_SECRET must be 32-4096 non-whitespace bytes")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8}
	return &piExecutor{endpoint: strings.TrimSuffix(endpoint, "/") + "/v1/execute", secret: secret, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type piFrame struct {
	Type     string            `json:"type"`
	Driver   string            `json:"driver"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	Dispatch string            `json:"dispatch_state"`
	Data     []byte            `json:"data"`
	Response json.RawMessage   `json:"response"`
	Code     string            `json:"code"`
}
type piReader struct {
	body     io.ReadCloser
	scanner  *bufio.Scanner
	headers  bool
	stream   bool
	result   bool
	terminal bool
	status   int
}

func (r *piReader) next() (piFrame, error) {
	bad := func() (piFrame, error) { return piFrame{}, piError("pi_bridge_protocol_error", "maybe_sent") }
	if r.terminal || !r.scanner.Scan() {
		return bad()
	}
	var f piFrame
	if json.Unmarshal(r.scanner.Bytes(), &f) != nil {
		return bad()
	}
	if f.Type == "error" {
		if (f.Dispatch != "not_sent" && f.Dispatch != "maybe_sent") || f.Status < 0 || (f.Status != 0 && (f.Status < 100 || f.Status > 599)) || (r.headers && f.Dispatch != "maybe_sent") {
			return bad()
		}
		r.terminal = true
		// Never trust arbitrary bridge error strings as safe log content.
		code := "pi_bridge_error"
		switch f.Code {
		case "unsupported_capability", "invalid_request", "unauthorized", "upstream_error", "transport_error", "cancelled", "internal_error", "overloaded":
			code = "pi_" + f.Code
		}
		return f, &PiError{code: code, dispatch: f.Dispatch, status: f.Status}
	}
	switch f.Type {
	case "headers":
		if r.headers || f.Driver != "pi" || f.Status != 200 || f.Dispatch != "maybe_sent" || r.status != 200 {
			return bad()
		}
		r.headers = true
	case "chunk":
		if !r.headers || !r.stream || len(f.Data) == 0 {
			return bad()
		}
	case "result":
		if !r.headers || r.stream || r.result || len(f.Response) == 0 || f.Response[0] != '{' {
			return bad()
		}
		r.result = true
	case "done":
		if !r.headers || (!r.stream && !r.result) || f.Dispatch != "maybe_sent" {
			return bad()
		}
		r.terminal = true
	default:
		return bad()
	}
	return f, nil
}

// ValidatePiRequest rejects unsupported semantics without contacting the bridge.
func ValidatePiRequest(q ExecuteRequest) error {
	if q.Format != "openai-response" || (q.RequestPath != "" && q.RequestPath != "/v1/responses") || len(q.ConfiguredHeaders) > 0 || q.ProxyFromEnvironment || (q.ProxyURL != "" && q.ProxyURL != "direct") {
		return piError("pi_unsupported_capability", "not_sent")
	}
	for key := range q.Headers {
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "host", "content-type", "content-length", "accept", "accept-encoding", "connection", "user-agent", "x-forwarded-for", "x-forwarded-proto", "x-forwarded-host":
			// Downstream authentication and HTTP transport metadata are not upstream options.
		default:
			return piError("pi_unsupported_headers", "not_sent")
		}
	}
	if len(q.Payload) > piMaxRequestBytes || !json.Valid(q.Payload) {
		return piError("pi_invalid_request", "not_sent")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(q.Payload, &root) != nil || root == nil {
		return piError("pi_invalid_request", "not_sent")
	}
	if model, ok := root["model"]; ok {
		var bodyModel string
		if json.Unmarshal(model, &bodyModel) != nil || bodyModel != q.Model {
			return piError("pi_model_mismatch", "not_sent")
		}
	}
	// Stateful/background execution cannot be faithfully represented by this slice.
	for _, key := range []string{"previous_response_id", "conversation", "background", "store"} {
		if v, ok := root[key]; ok && string(v) != "null" && string(v) != "false" {
			return piError("pi_unsupported_capability", "not_sent")
		}
	}
	return nil
}
func (e *piExecutor) open(ctx context.Context, c Credential, q ExecuteRequest, stream bool) (*piReader, piFrame, error) {
	if err := ValidatePiRequest(q); err != nil {
		return nil, piFrame{}, err
	}
	var bodyStream struct {
		Stream *bool `json:"stream"`
	}
	_ = json.Unmarshal(q.Payload, &bodyStream)
	if bodyStream.Stream != nil && *bodyStream.Stream != stream {
		return nil, piFrame{}, piError("pi_stream_mismatch", "not_sent")
	}
	envelope := struct {
		Version    int    `json:"version"`
		Provider   string `json:"provider"`
		Format     string `json:"format"`
		Operation  string `json:"operation"`
		Model      string `json:"model"`
		Stream     bool   `json:"stream"`
		Credential struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"credential"`
		Body      json.RawMessage   `json:"body"`
		Headers   map[string]string `json:"headers"`
		BaseURL   string            `json:"base_url"`
		ProxyURL  string            `json:"proxy_url"`
		SessionID string            `json:"session_id"`
	}{Version: 1, Provider: "codex", Format: q.Format, Operation: "responses", Model: q.Model, Stream: stream, Body: q.Payload, Headers: map[string]string{}, BaseURL: q.BaseURL, ProxyURL: q.ProxyURL, SessionID: q.ContinuityKey}
	envelope.Credential.AccessToken = c.AccessToken
	envelope.Credential.AccountID = c.AccountID
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, piFrame{}, piError("pi_invalid_request", "not_sent")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, piFrame{}, piError("pi_invalid_request", "not_sent")
	}
	req.Header.Set("Authorization", "Bearer "+e.secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, piFrame{}, piError("pi_bridge_transport_error", "maybe_sent")
	}
	reader := &piReader{body: resp.Body, scanner: bufio.NewScanner(resp.Body), stream: stream, status: resp.StatusCode}
	reader.scanner.Buffer(make([]byte, 4096), piMaxFrameBytes)
	frame, err := reader.next()
	if err != nil {
		resp.Body.Close()
		return nil, frame, err
	}
	if frame.Type != "headers" {
		resp.Body.Close()
		return nil, piFrame{}, piError("pi_bridge_protocol_error", "maybe_sent")
	}
	return reader, frame, nil
}
func piHeaders(f piFrame) (http.Header, map[string]string) {
	h := make(http.Header)
	quota := map[string]string{}
	for k, v := range f.Headers {
		lower := strings.ToLower(k)
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			continue
		}
		switch lower {
		case "x-request-id", "request-id", "openai-request-id", "x-oai-request-id", "retry-after":
			h.Set(k, v)
		}
		if strings.HasPrefix(lower, "x-codex-") {
			quota[k] = v
		}
	}
	h.Set("X-GPT-Load-Driver", "pi-experimental")
	return h, quota
}
func (e *piExecutor) Execute(ctx context.Context, _ string, c Credential, q ExecuteRequest) (ExecuteResponse, error) {
	r, f, err := e.open(ctx, c, q, false)
	if err != nil {
		return ExecuteResponse{}, err
	}
	defer r.body.Close()
	h, quota := piHeaders(f)
	response := ExecuteResponse{StatusCode: 200, Headers: h, UpstreamRequestPath: "/v1/responses", QuotaObservedAt: time.Now(), QuotaSignals: quota}
	for {
		f, err = r.next()
		if err != nil {
			return response, err
		}
		if f.Type == "result" {
			response.Payload = append([]byte(nil), f.Response...)
		}
		if f.Type == "done" {
			return response, nil
		}
	}
}
func (e *piExecutor) CountTokens(context.Context, string, Credential, ExecuteRequest) (ExecuteResponse, error) {
	return ExecuteResponse{}, piError("pi_unsupported_capability", "not_sent")
}
func (e *piExecutor) ExecuteStream(ctx context.Context, _ string, c Credential, q ExecuteRequest) (*ExecuteStreamResponse, error) {
	r, f, err := e.open(ctx, c, q, true)
	if err != nil {
		return nil, err
	}
	h, quota := piHeaders(f)
	chunks := make(chan ExecuteStreamChunk)
	go func() {
		defer close(chunks)
		defer r.body.Close()
		for {
			f, err := r.next()
			if err == nil && f.Type == "done" {
				return
			}
			chunk := ExecuteStreamChunk{Payload: f.Data, Err: err}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &ExecuteStreamResponse{Headers: h, Chunks: chunks, UpstreamRequestPath: "/v1/responses", QuotaObservedAt: time.Now(), QuotaSignals: quota}, nil
}

var _ Executor = (*piExecutor)(nil)
