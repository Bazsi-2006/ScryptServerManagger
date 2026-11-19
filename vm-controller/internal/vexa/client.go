// Package vexa is a deliberately small client for the Vexa-VM REST API.
//
// It can do exactly four things: read one VM, request start, request a
// graceful (ACPI) shutdown, and optionally read the status of a job that one
// of those requests created. Every URL is built from the configured base URL,
// a fixed path and a validated identifier; nothing supplied by a browser is
// ever forwarded. force-off, reboot, reset, suspend and every other Vexa
// operation are intentionally impossible to express with this package.
package vexa

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State is the VM power state as reported by Vexa-VM (VmState in the
// OpenAPI document).
type State string

const (
	StateRunning      State = "running"
	StateStopped      State = "stopped"
	StatePaused       State = "paused"
	StateCreating     State = "creating"
	StateReinstalling State = "reinstalling"
	StateMigrating    State = "migrating"
	StateError        State = "error"
	StateUnknown      State = "unknown"
)

func normalizeState(s string) State {
	switch st := State(strings.ToLower(strings.TrimSpace(s))); st {
	case StateRunning, StateStopped, StatePaused, StateCreating, StateReinstalling, StateMigrating, StateError:
		return st
	default:
		return StateUnknown
	}
}

// VM is the subset of the Vexa VM record this application uses.
type VM struct {
	ID    string
	Name  string
	State State
}

// JobStatus is the status of a durable Vexa job.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Finished reports whether the job reached a terminal status.
func (s JobStatus) Finished() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Job is the subset of a Vexa job record this application uses.
type Job struct {
	ID     string
	Status JobStatus
	// Error is Vexa's sanitized failure text. It is meant for the server
	// log, not for end users.
	Error string
}

// ErrorKind classifies failures so callers can react without parsing text.
type ErrorKind int

const (
	// KindUnavailable: Vexa could not be reached or did not answer in time.
	KindUnavailable ErrorKind = iota + 1
	// KindAuth: the API key was rejected (401) or lacks a scope (403).
	KindAuth
	// KindNotFound: Vexa does not know the VM or job (404).
	KindNotFound
	// KindConflict: Vexa refused because of the resource state (409).
	KindConflict
	// KindRateLimited: Vexa asked us to slow down (429).
	KindRateLimited
	// KindRejected: any other 4xx response.
	KindRejected
	// KindServer: a 5xx response.
	KindServer
	// KindBadResponse: a response this client cannot interpret.
	KindBadResponse
	// KindInvalidID: the identifier failed local validation; nothing was sent.
	KindInvalidID
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnavailable:
		return "unavailable"
	case KindAuth:
		return "auth"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindRateLimited:
		return "rate_limited"
	case KindRejected:
		return "rejected"
	case KindServer:
		return "server_error"
	case KindBadResponse:
		return "bad_response"
	case KindInvalidID:
		return "invalid_id"
	}
	return "error"
}

// Error describes a failed Vexa request. Its text is safe for server logs:
// it never contains the API key. It is not meant to be shown to end users.
type Error struct {
	Kind       ErrorKind
	Op         string
	StatusCode int
	Code       string // Vexa error code, e.g. "validation_error"
	Message    string // Vexa error message
	RequestID  string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "vexa %s: %s", e.Op, e.Kind)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.StatusCode)
		if e.Code != "" {
			fmt.Fprintf(&b, " %s", e.Code)
		}
		if e.RequestID != "" {
			fmt.Fprintf(&b, ", request_id %s", e.RequestID)
		}
		b.WriteString(")")
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the ErrorKind of err, or 0 if err is not an *Error.
func KindOf(err error) ErrorKind {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Kind
	}
	return 0
}

// Options configures a Client.
type Options struct {
	BaseURL   *url.URL
	APIKey    string
	CAFile    string // optional PEM bundle added to the system roots
	Timeout   time.Duration
	UserAgent string
}

// Client talks to one Vexa-VM node.
type Client struct {
	base      string
	apiKey    string
	userAgent string
	http      *http.Client
}

// maxResponseBytes bounds how much of a Vexa response is read.
const maxResponseBytes = 2 << 20

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	jobPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidID reports whether id is acceptable as a VM identifier.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// New creates a client. The base URL must already be validated.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == nil || (opts.BaseURL.Scheme != "http" && opts.BaseURL.Scheme != "https") || opts.BaseURL.Host == "" {
		return nil, errors.New("vexa: invalid base URL")
	}
	if opts.APIKey == "" {
		return nil, errors.New("vexa: API key is empty")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("vexa: reading ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("vexa: ca_file %s contains no PEM certificates", opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	dialTimeout := min(timeout, 5*time.Second)
	transport := &http.Transport{
		// The destination is fixed by configuration; never route the API
		// key through a proxy picked up from the environment.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "vm-controller"
	}
	base := *opts.BaseURL
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawPath = ""
	base.RawQuery, base.Fragment = "", ""
	return &Client{
		base:      base.String(),
		apiKey:    opts.APIKey,
		userAgent: ua,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect could carry the bearer key to another host;
			// never follow one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// powerAction is unexported so that no caller outside this package can ask
// for anything other than the two actions below.
type powerAction string

const (
	actionStart    powerAction = "start"
	actionShutdown powerAction = "shutdown" // graceful ACPI shutdown, not force-off
)

// GetVM reads the VM's current state: GET /api/v1/vms/{vm_id}.
func (c *Client) GetVM(ctx context.Context, id string) (VM, error) {
	if !ValidID(id) {
		return VM{}, &Error{Kind: KindInvalidID, Op: "get vm"}
	}
	var body struct {
		VM *struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"vm"`
	}
	if err := c.do(ctx, "get vm", http.MethodGet, "/api/v1/vms/"+url.PathEscape(id), &body); err != nil {
		return VM{}, err
	}
	if body.VM == nil {
		return VM{}, &Error{Kind: KindBadResponse, Op: "get vm", Message: "response has no vm object"}
	}
	return VM{ID: body.VM.ID, Name: body.VM.Name, State: normalizeState(body.VM.State)}, nil
}

// Start requests that the VM be started:
// POST /api/v1/vms/{vm_id}/actions/start.
// Acceptance only means Vexa queued a job.
func (c *Client) Start(ctx context.Context, id string) (Job, error) {
	return c.power(ctx, id, actionStart)
}

// Shutdown requests a graceful guest shutdown:
// POST /api/v1/vms/{vm_id}/actions/shutdown.
// The guest decides whether and when it powers off.
func (c *Client) Shutdown(ctx context.Context, id string) (Job, error) {
	return c.power(ctx, id, actionShutdown)
}

func (c *Client) power(ctx context.Context, id string, action powerAction) (Job, error) {
	op := string(action)
	if action != actionStart && action != actionShutdown {
		return Job{}, &Error{Kind: KindRejected, Op: op, Message: "unsupported action"}
	}
	if !ValidID(id) {
		return Job{}, &Error{Kind: KindInvalidID, Op: op}
	}
	var body jobEnvelope
	if err := c.do(ctx, op, http.MethodPost, "/api/v1/vms/"+url.PathEscape(id)+"/actions/"+string(action), &body); err != nil {
		return Job{}, err
	}
	return body.job(), nil
}

// GetJob reads a job created by Start or Shutdown: GET /api/v1/jobs/{job_id}.
// It needs the jobs:read scope and is only used when track_jobs is enabled.
func (c *Client) GetJob(ctx context.Context, jobID string) (Job, error) {
	if !jobPattern.MatchString(jobID) {
		return Job{}, &Error{Kind: KindInvalidID, Op: "get job"}
	}
	var body jobEnvelope
	if err := c.do(ctx, "get job", http.MethodGet, "/api/v1/jobs/"+jobID, &body); err != nil {
		return Job{}, err
	}
	j := body.job()
	if j.ID == "" {
		return Job{}, &Error{Kind: KindBadResponse, Op: "get job", Message: "response has no operation"}
	}
	return j, nil
}

type jobEnvelope struct {
	Operation *struct {
		ID     string  `json:"id"`
		Status string  `json:"status"`
		Error  *string `json:"error"`
	} `json:"operation"`
}

func (e jobEnvelope) job() Job {
	if e.Operation == nil || !jobPattern.MatchString(e.Operation.ID) {
		return Job{}
	}
	j := Job{ID: e.Operation.ID, Status: JobStatus(strings.ToLower(e.Operation.Status))}
	if e.Operation.Error != nil {
		j.Error = truncate(*e.Operation.Error, 500)
	}
	return j
}

func (c *Client) do(ctx context.Context, op, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return &Error{Kind: KindBadResponse, Op: op, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Kind: KindUnavailable, Op: op, Err: redactURLError(err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return &Error{Kind: KindUnavailable, Op: op, StatusCode: resp.StatusCode, Err: err}
	}
	if len(data) > maxResponseBytes {
		return &Error{Kind: KindBadResponse, Op: op, StatusCode: resp.StatusCode, Message: "response too large"}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Kind: KindBadResponse, Op: op, StatusCode: resp.StatusCode, Message: "invalid JSON in response"}
		}
		return nil
	}
	e := &Error{Op: op, StatusCode: resp.StatusCode}
	var env struct {
		Error *struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error != nil {
		e.Code = truncate(env.Error.Code, 64)
		e.Message = truncate(env.Error.Message, 300)
		e.RequestID = truncate(env.Error.RequestID, 64)
	}
	switch code := resp.StatusCode; {
	case code >= 300 && code < 400:
		e.Kind = KindBadResponse
		e.Message = "unexpected redirect (check vexa.base_url)"
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		e.Kind = KindAuth
	case code == http.StatusNotFound:
		e.Kind = KindNotFound
	case code == http.StatusConflict:
		e.Kind = KindConflict
	case code == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	case code >= 500:
		e.Kind = KindServer
		if code == http.StatusServiceUnavailable {
			e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
	default:
		e.Kind = KindRejected
	}
	return e
}

// redactURLError drops the request URL from transport errors. The URL holds
// no secret, but the operation name already identifies the request and this
// keeps log lines short and uniform.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("timeout: %w", ue.Err)
		}
		return ue.Err
	}
	return err
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return max(0, min(d, 5*time.Minute))
}

func truncate(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	cut := 0
	for i := range s { // stop on a rune boundary
		if i > n {
			break
		}
		cut = i
	}
	return s[:cut] + "…"
}
