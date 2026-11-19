package vexa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexatest"
)

const (
	testKey = "vxa_test_secret_key"
	vmID    = "aaaaaaaa-1111-4111-8111-11111111111a"
)

func newClient(t *testing.T, base string, timeout time.Duration) *Client {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{BaseURL: u, APIKey: testKey, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGetVMAndPowerAgainstMock(t *testing.T) {
	mock := vexatest.New(testKey)
	defer mock.Close()
	mock.AddVM(vmID, "minecraft", "stopped")
	c := newClient(t, mock.URL(), 2*time.Second)
	ctx := context.Background()

	vm, err := c.GetVM(ctx, vmID)
	if err != nil || vm.State != StateStopped || vm.Name != "minecraft" || vm.ID != vmID {
		t.Fatalf("GetVM = %+v, %v", vm, err)
	}

	job, err := c.Start(ctx, vmID)
	if err != nil || job.ID == "" || job.Status != JobQueued {
		t.Fatalf("Start = %+v, %v", job, err)
	}
	if got := mock.State(vmID); got != "running" {
		t.Fatalf("mock state after start = %s", got)
	}
	got, err := c.GetJob(ctx, job.ID)
	if err != nil || got.Status != JobSucceeded {
		t.Fatalf("GetJob = %+v, %v", got, err)
	}

	if _, err := c.Shutdown(ctx, vmID); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{vmID + " start", vmID + " shutdown"}
	if calls := mock.PowerCalls(); strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("power calls = %v, want %v", calls, want)
	}
	for _, r := range mock.Requests() {
		if !r.Authed {
			t.Errorf("request %s %s was sent without the API key", r.Method, r.Path)
		}
	}
}

func TestRequestShape(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		if r.ContentLength > 0 {
			t.Errorf("power requests must not carry a body")
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation":{"id":"00000000-0000-4000-8000-000000000001","status":"queued","error":null}}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/", time.Second) // trailing slash must not produce "//api"
	if _, err := c.Start(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Shutdown(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	want := "POST /api/v1/vms/" + vmID + "/actions/start,POST /api/v1/vms/" + vmID + "/actions/shutdown"
	if strings.Join(seen, ",") != want {
		t.Fatalf("requests = %v", seen)
	}
}

func TestInvalidIDsAreNeverSent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := newClient(t, srv.URL, time.Second)
	ctx := context.Background()
	for _, id := range []string{"", "..", "../jobs", "a/b", "a%2Fb", "vm?x=1", "vm#x", " vm", "vm/actions/force-off", strings.Repeat("a", 129)} {
		if _, err := c.GetVM(ctx, id); KindOf(err) != KindInvalidID {
			t.Errorf("GetVM(%q) err = %v", id, err)
		}
		if _, err := c.Start(ctx, id); KindOf(err) != KindInvalidID {
			t.Errorf("Start(%q) err = %v", id, err)
		}
		if _, err := c.Shutdown(ctx, id); KindOf(err) != KindInvalidID {
			t.Errorf("Shutdown(%q) err = %v", id, err)
		}
	}
	for _, id := range []string{"", "not-a-uuid", "../vms/x", "00000000-0000-4000-8000-00000000000g"} {
		if _, err := c.GetJob(ctx, id); KindOf(err) != KindInvalidID {
			t.Errorf("GetJob(%q) err = %v", id, err)
		}
	}
	if _, err := c.power(ctx, vmID, powerAction("force-off")); err == nil {
		t.Error("power must refuse actions other than start and shutdown")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		header map[string]string
		kind   ErrorKind
	}{
		{401, `{"success":false,"error":{"code":"unauthorized","message":"bad key","request_id":"r1"}}`, nil, KindAuth},
		{403, `{"success":false,"error":{"code":"forbidden","message":"missing scope","request_id":"r2"}}`, nil, KindAuth},
		{404, `{"success":false,"error":{"code":"not_found","message":"VM not found","request_id":"r3"}}`, nil, KindNotFound},
		{409, `{"success":false,"error":{"code":"conflict","message":"no domain","request_id":"r4"}}`, nil, KindConflict},
		{429, `{"success":false,"error":{"code":"rate_limited","message":"slow down","request_id":"r5"}}`, map[string]string{"Retry-After": "7"}, KindRateLimited},
		{400, `{"success":false,"error":{"code":"validation_error","message":"x","request_id":"r6"}}`, nil, KindRejected},
		{500, `not json`, nil, KindServer},
		{503, `{"success":false,"error":{"code":"hypervisor_error","message":"libvirt down","request_id":"r7"}}`, nil, KindServer},
		{200, `not json`, nil, KindBadResponse},
		{200, `{"other":true}`, nil, KindBadResponse},
		{302, ``, map[string]string{"Location": "http://evil.example/"}, KindBadResponse},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := newClient(t, srv.URL, time.Second)
		_, err := c.GetVM(context.Background(), vmID)
		srv.Close()
		if KindOf(err) != tc.kind {
			t.Errorf("HTTP %d: kind = %v (%v), want %v", tc.status, KindOf(err), err, tc.kind)
			continue
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("error text leaks the API key: %v", err)
		}
		if tc.status == 429 {
			var ve *Error
			if !asError(err, &ve) || ve.RetryAfter != 7*time.Second || ve.RequestID != "r5" {
				t.Errorf("429 details: %+v", ve)
			}
		}
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), testKey) {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte(`{"vm":{"id":"x","name":"x","state":"running"}}`))
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, time.Second)
	if _, err := c.GetVM(context.Background(), vmID); KindOf(err) != KindBadResponse {
		t.Fatalf("redirect: err = %v", err)
	}
	if _, err := c.Start(context.Background(), vmID); KindOf(err) != KindBadResponse {
		t.Fatalf("redirect on power: err = %v", err)
	}
	if leaked.Load() {
		t.Fatal("API key was sent to the redirect target")
	}
}

func TestUnavailableAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens any more
	c := newClient(t, base, time.Second)
	if _, err := c.GetVM(context.Background(), vmID); KindOf(err) != KindUnavailable {
		t.Fatalf("closed server: %v", err)
	}

	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	c = newClient(t, slow.URL, 200*time.Millisecond)
	start := time.Now()
	_, err := c.Start(context.Background(), vmID)
	if KindOf(err) != KindUnavailable {
		t.Fatalf("slow server: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout was not applied")
	}
}

func TestStateNormalization(t *testing.T) {
	for in, want := range map[string]State{
		"running": StateRunning, "Stopped": StateStopped, "paused": StatePaused, "creating": StateCreating,
		"reinstalling": StateReinstalling, "migrating": StateMigrating, "error": StateError,
		"unknown": StateUnknown, "": StateUnknown, "shut-off": StateUnknown, "<script>": StateUnknown,
	} {
		if got := normalizeState(in); got != want {
			t.Errorf("normalizeState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	if _, err := New(Options{BaseURL: u}); err == nil {
		t.Error("empty key must be rejected")
	}
	bad, _ := url.Parse("ftp://127.0.0.1")
	if _, err := New(Options{BaseURL: bad, APIKey: "k"}); err == nil {
		t.Error("ftp must be rejected")
	}
	if _, err := New(Options{BaseURL: u, APIKey: "k", CAFile: "/nonexistent/ca.pem"}); err == nil {
		t.Error("missing CA file must be rejected")
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := truncate(strings.Repeat("é", 400), 301)
	if !strings.HasSuffix(s, "…") || strings.ContainsRune(s, '�') {
		t.Fatalf("truncate produced %q", s)
	}
	if got := truncate("a\nb\x00c", 10); got != "a b c" {
		t.Fatalf("control characters not replaced: %q", got)
	}
}
