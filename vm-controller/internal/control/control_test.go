package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexa"
	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexatest"
)

const (
	key       = "vxa_controller_test"
	stoppable = "aaaaaaaa-1111-4111-8111-11111111111a"
	startOnly = "bbbbbbbb-2222-4222-8222-22222222222b"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var testVMs = []config.VM{
	{ID: stoppable, Name: "Dev", AllowStop: true},
	{ID: startOnly, Name: "Minecraft", AllowStop: false},
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func setup(t *testing.T, opts Options) (*Controller, *vexatest.Server, *clock) {
	t.Helper()
	mock := vexatest.New(key)
	t.Cleanup(mock.Close)
	mock.AddVM(stoppable, "dev", "stopped")
	mock.AddVM(startOnly, "minecraft", "stopped")
	u, _ := url.Parse(mock.URL())
	client, err := vexa.New(vexa.Options{BaseURL: u, APIKey: key, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	opts.Now = clk.Now
	opts.Logger = quietLogger()
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 3 * time.Minute
	}
	if opts.StopTimeout == 0 {
		opts.StopTimeout = 7 * time.Minute
	}
	return New(client, testVMs, opts), mock, clk
}

func kind(err error) ErrorKind {
	var re *RequestError
	if errors.As(err, &re) {
		return re.Kind
	}
	return 0
}

func TestStartLifecycle(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	ctx := context.Background()
	mock.StartDelay = time.Hour // the job never finishes on its own

	if st := c.Status(ctx, stoppable); st.State != StateStopped {
		t.Fatalf("initial state %v", st)
	}
	if err := c.Request(ctx, stoppable, config.ActionStart, "alice"); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := c.Status(ctx, stoppable)
	if st.State != StateStarting || st.Pending != config.ActionStart {
		t.Fatalf("after start: %+v", st)
	}
	if c.PollInterval([]string{stoppable}) != 3*time.Second {
		t.Error("poll faster while pending")
	}

	// Repeated and conflicting requests are refused without calling Vexa.
	if err := c.Request(ctx, stoppable, config.ActionStart, "bob"); kind(err) != ErrBusy {
		t.Fatalf("second start: %v", err)
	}
	if err := c.Request(ctx, stoppable, config.ActionStop, "bob"); kind(err) != ErrBusy {
		t.Fatalf("conflicting stop: %v", err)
	}
	if calls := mock.PowerCalls(); len(calls) != 1 {
		t.Fatalf("power calls = %v", calls)
	}

	mock.SetState(stoppable, "running")
	clk.Advance(3 * time.Second) // past the pending cache TTL
	if st := c.Status(ctx, stoppable); st.State != StateRunning || st.Pending != "" {
		t.Fatalf("after Vexa reports running: %+v", st)
	}
	if c.PollInterval([]string{stoppable}) != 10*time.Second {
		t.Error("poll slower when idle")
	}
}

func TestStopLifecycleAndTimeout(t *testing.T) {
	c, mock, clk := setup(t, Options{StopTimeout: 7 * time.Minute})
	ctx := context.Background()
	mock.SetState(stoppable, "running")

	if err := c.Request(ctx, stoppable, config.ActionStop, "bob"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := mock.PowerCalls(); len(got) != 1 || got[0] != stoppable+" shutdown" {
		t.Fatalf("expected one graceful shutdown, got %v", got)
	}
	// Vexa keeps reporting "running" after an ACPI shutdown.
	if st := c.Status(ctx, stoppable); st.State != StateStopping {
		t.Fatalf("after stop: %+v", st)
	}
	clk.Advance(7*time.Minute + time.Second)
	st := c.Status(ctx, stoppable)
	if st.State != StateRunning || st.Pending != "" || !strings.Contains(st.Message, "ignored the shutdown") {
		t.Fatalf("after timeout: %+v", st)
	}
	// After the timeout a new attempt is possible.
	if err := c.Request(ctx, stoppable, config.ActionStop, "bob"); err != nil {
		t.Fatalf("retry stop: %v", err)
	}
	mock.SetState(stoppable, "stopped") // inventory sweep noticed
	clk.Advance(3 * time.Second)
	if st := c.Status(ctx, stoppable); st.State != StateStopped || st.Message != "" {
		t.Fatalf("after Vexa reports stopped: %+v", st)
	}
}

func TestPreconditions(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	ctx := context.Background()

	if err := c.Request(ctx, startOnly, config.ActionStop, "owner"); kind(err) != ErrForbidden {
		t.Fatalf("stop on allow_stop=false VM: %v", err)
	}
	if err := c.Request(ctx, stoppable, config.ActionStop, "owner"); kind(err) != ErrWrongState {
		t.Fatalf("stop on stopped VM: %v", err)
	}
	mock.SetState(stoppable, "running")
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrWrongState {
		t.Fatalf("start on running VM: %v", err)
	}
	// Starting a paused VM would resume it in Vexa; refuse it.
	mock.SetState(stoppable, "paused")
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrWrongState {
		t.Fatalf("start on paused VM: %v", err)
	}
	for _, s := range []string{"creating", "reinstalling", "error", "unknown"} {
		mock.SetState(stoppable, s)
		clk.Advance(10 * time.Second)
		if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrWrongState {
			t.Fatalf("start on %s VM: %v", s, err)
		}
	}
	if err := c.Request(ctx, "not-configured", config.ActionStart, "owner"); kind(err) != ErrForbidden {
		t.Fatalf("unknown VM: %v", err)
	}
	if err := c.Request(ctx, stoppable, config.Action("force-off"), "owner"); kind(err) != ErrForbidden {
		t.Fatalf("unknown action: %v", err)
	}
	if calls := mock.PowerCalls(); len(calls) != 0 {
		t.Fatalf("no power request should have reached Vexa, got %v", calls)
	}
}

func TestDisplayStates(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	ctx := context.Background()
	for vexaState, want := range map[string]State{
		"running": StateRunning, "stopped": StateStopped, "paused": StatePaused, "creating": StateBusy,
		"reinstalling": StateBusy, "migrating": StateBusy, "error": StateError, "unknown": StateUnknown, "weird": StateUnknown,
	} {
		mock.SetState(stoppable, vexaState)
		clk.Advance(10 * time.Second)
		if st := c.Status(ctx, stoppable); st.State != want {
			t.Errorf("vexa %q -> %v, want %v", vexaState, st.State, want)
		}
	}
}

func TestCacheSharedAcrossViewers(t *testing.T) {
	c, mock, clk := setup(t, Options{CacheTTL: 5 * time.Second})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.StatusMany(ctx, []string{stoppable, startOnly})
		}()
	}
	wg.Wait()
	if n := mock.CountRequests("GET", "/api/v1/vms/"+stoppable); n != 1 {
		t.Fatalf("50 viewers caused %d Vexa requests for one VM, want 1", n)
	}
	clk.Advance(6 * time.Second)
	c.Status(ctx, stoppable)
	if n := mock.CountRequests("GET", "/api/v1/vms/"+stoppable); n != 2 {
		t.Fatalf("cache did not expire: %d requests", n)
	}
}

func TestVexaUnavailableAndRateLimited(t *testing.T) {
	c, mock, clk := setup(t, Options{CacheTTL: 5 * time.Second})
	ctx := context.Background()

	mock.Fail("GET /api/v1/vms/", vexatest.Failure{Status: 503, Code: "hypervisor_error", Message: "libvirt is down"})
	st := c.Status(ctx, stoppable)
	if st.State != StateUnavailable || strings.Contains(st.Message, "libvirt") {
		t.Fatalf("503: %+v (internal details must not reach users)", st)
	}
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrUnavailable {
		t.Fatalf("start while Vexa is down: %v", err)
	}

	mock.ClearFailures()
	mock.Fail("GET /api/v1/vms/", vexatest.Failure{Status: 429, Code: "rate_limited", RetryAfter: "30"})
	clk.Advance(10 * time.Second)
	if st := c.Status(ctx, stoppable); st.State != StateUnavailable || !strings.Contains(st.Message, "busy") {
		t.Fatalf("429: %+v", st)
	}
	before := mock.CountRequests("GET", "/api/v1/vms/"+stoppable)
	clk.Advance(10 * time.Second) // cache expired, but Retry-After has not
	c.Status(ctx, stoppable)
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrUnavailable {
		t.Fatalf("start while rate limited: %v", err)
	}
	if after := mock.CountRequests("GET", "/api/v1/vms/"+stoppable); after != before {
		t.Fatalf("Retry-After was not honoured: %d -> %d", before, after)
	}
	mock.ClearFailures()
	clk.Advance(25 * time.Second)
	if st := c.Status(ctx, stoppable); st.State != StateStopped {
		t.Fatalf("after recovery: %+v", st)
	}
	if len(mock.PowerCalls()) != 0 {
		t.Fatal("no power request should have been sent")
	}

	mock.Fail("GET /api/v1/vms/", vexatest.Failure{Status: 404, Code: "not_found"})
	clk.Advance(10 * time.Second)
	if st := c.Status(ctx, stoppable); !strings.Contains(st.Message, "not found") {
		t.Fatalf("404: %+v", st)
	}
}

func TestPowerRequestFailures(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	ctx := context.Background()
	cases := []struct {
		f    vexatest.Failure
		want ErrorKind
	}{
		{vexatest.Failure{Status: 409, Code: "conflict"}, ErrWrongState},
		{vexatest.Failure{Status: 403, Code: "forbidden", Message: "missing scope vms:power"}, ErrUpstream},
		{vexatest.Failure{Status: 500, Code: "internal_error", Message: "secret detail"}, ErrUnavailable},
		{vexatest.Failure{Status: 429, Code: "rate_limited", RetryAfter: "5"}, ErrUnavailable},
		{vexatest.Failure{Status: 400, Code: "validation_error"}, ErrUpstream},
	}
	for _, tc := range cases {
		mock.ClearFailures()
		mock.Fail("POST /api/v1/vms/", tc.f)
		clk.Advance(10 * time.Second) // past cooldown
		err := c.Request(ctx, stoppable, config.ActionStart, "owner")
		if kind(err) != tc.want {
			t.Errorf("HTTP %d: %v, want kind %v", tc.f.Status, err, tc.want)
		}
		var re *RequestError
		if errors.As(err, &re) && strings.Contains(re.Message, "secret detail") {
			t.Errorf("user message leaks Vexa details: %q", re.Message)
		}
		if st := c.Status(ctx, stoppable); st.Pending != "" {
			t.Errorf("HTTP %d left a pending operation", tc.f.Status)
		}
	}
	// A failed attempt still starts the cooldown.
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); kind(err) != ErrBusy {
		t.Fatalf("cooldown not applied: %v", err)
	}
}

// fakeVexa lets tests produce errors that are awkward over HTTP.
type fakeVexa struct {
	state    vexa.State
	powerErr error
	calls    int
}

func (f *fakeVexa) GetVM(context.Context, string) (vexa.VM, error) {
	return vexa.VM{State: f.state}, nil
}
func (f *fakeVexa) Start(context.Context, string) (vexa.Job, error) {
	f.calls++
	return vexa.Job{}, f.powerErr
}
func (f *fakeVexa) Shutdown(context.Context, string) (vexa.Job, error) {
	f.calls++
	return vexa.Job{}, f.powerErr
}
func (f *fakeVexa) GetJob(context.Context, string) (vexa.Job, error) { return vexa.Job{}, nil }

func TestUncertainPowerRequest(t *testing.T) {
	f := &fakeVexa{state: vexa.StateStopped, powerErr: &vexa.Error{Kind: vexa.KindUnavailable, Op: "start", Err: errors.New("timeout")}}
	c := New(f, testVMs, Options{Logger: quietLogger()})
	err := c.Request(context.Background(), stoppable, config.ActionStart, "owner")
	if kind(err) != ErrUncertain || !strings.Contains(err.Error(), "may still have been applied") {
		t.Fatalf("timeout on power request: %v", err)
	}
	if err := c.Request(context.Background(), stoppable, config.ActionStart, "owner"); kind(err) != ErrBusy {
		t.Fatalf("immediate retry should hit the cooldown: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d", f.calls)
	}
}

func TestJobTracking(t *testing.T) {
	c, mock, clk := setup(t, Options{TrackJobs: true})
	ctx := context.Background()
	mock.FailJobs = true
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3 * time.Second)
	st := c.Status(ctx, stoppable)
	if st.State != StateStopped || st.Pending != "" || !strings.Contains(st.Message, "start request failed") {
		t.Fatalf("failed job: %+v", st)
	}
	if mock.CountRequests("GET", "/api/v1/jobs/") != 1 {
		t.Fatal("job was not polled")
	}

	// Without the jobs:read scope tracking switches itself off.
	mock.FailJobs = false
	mock.JobsForbidden = true
	mock.StartDelay = time.Hour
	clk.Advance(10 * time.Second)
	if err := c.Request(ctx, stoppable, config.ActionStart, "owner"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(3 * time.Second)
		if st := c.Status(ctx, stoppable); st.State != StateStarting {
			t.Fatalf("pending start lost: %+v", st)
		}
	}
	if n := mock.CountRequests("GET", "/api/v1/jobs/"); n != 2 {
		t.Fatalf("job polling should stop after a 403, got %d job requests", n)
	}
}

func TestJobsNotPolledByDefault(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	mock.StartDelay = time.Hour
	if err := c.Request(context.Background(), stoppable, config.ActionStart, "owner"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3 * time.Second)
	c.Status(context.Background(), stoppable)
	if n := mock.CountRequests("GET", "/api/v1/jobs/"); n != 0 {
		t.Fatalf("jobs polled without track_jobs: %d", n)
	}
}

func TestErrorStateClearsPending(t *testing.T) {
	c, mock, clk := setup(t, Options{})
	mock.StartDelay = time.Hour
	if err := c.Request(context.Background(), stoppable, config.ActionStart, "owner"); err != nil {
		t.Fatal(err)
	}
	mock.SetState(stoppable, "error")
	clk.Advance(3 * time.Second)
	st := c.Status(context.Background(), stoppable)
	if st.State != StateError || st.Pending != "" || st.Message == "" {
		t.Fatalf("%+v", st)
	}
}

func TestStartTimeoutNotice(t *testing.T) {
	c, mock, clk := setup(t, Options{StartTimeout: 3 * time.Minute})
	mock.StartDelay = time.Hour
	if err := c.Request(context.Background(), stoppable, config.ActionStart, "owner"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3*time.Minute + time.Second)
	st := c.Status(context.Background(), stoppable)
	if st.State != StateStopped || !strings.Contains(st.Message, "within 3 minutes") {
		t.Fatalf("%+v", st)
	}
	clk.Advance(11 * time.Minute)
	if st := c.Status(context.Background(), stoppable); st.Message != "" {
		t.Fatalf("notice should expire: %+v", st)
	}
}
