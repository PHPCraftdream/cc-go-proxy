package connpool

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// withChoice returns a request context that goes out the way c says.
func withChoice(c proxydial.Choice) context.Context {
	return proxydial.WithChoice(context.Background(), c)
}

func TestRouterPrunesIdleRouteAndRecreatesFreshTransport(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	r := NewRouter(time.Hour, 1, 2, plain)
	c := &clock{t: time.Unix(1000, 0)}
	r.now = c.now
	client := &http.Client{Transport: r}
	choice := proxydial.Choice{Direct: true}
	key := choice.Key()
	getCtx(t, client, srv.URL, withChoice(choice))
	old := r.ts[key]
	p := old.lanes[0].cur
	warm, peer := net.Pipe()
	p.warm = &warmSlot{}
	p.warm.put("warm", warm)
	t.Cleanup(func() { _ = peer.Close() })

	c.t = c.t.Add(routeIdleTimeout + time.Second)
	get(t, client, srv.URL) // prune the idle direct route while creating the env route
	if _, ok := r.ts[key]; ok {
		t.Fatal("inactive direct route was retained")
	}
	if p.warm.conn != nil {
		t.Fatal("pruned route retained its warm connection")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil || (isTimeout(err)) {
		t.Fatalf("pruned route did not close its warm connection: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("pruned route's idle socket was not closed")
	}

	getCtx(t, client, srv.URL, withChoice(choice))
	if fresh := r.ts[key]; fresh == old || fresh == nil {
		t.Fatal("same choice did not get a fresh Transport")
	}
}

func TestRouterKeepsBusyRouteUntilResponseCloses(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "start")
			w.(http.Flusher).Flush()
			close(started)
			<-finish
		}
		_, _ = io.WriteString(w, "end")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	})
	r := NewRouter(time.Hour, 1, 2, plain)
	c := &clock{t: time.Unix(1000, 0)}
	r.now = c.now
	client := &http.Client{Transport: r}
	initial, err := http.NewRequestWithContext(withChoice(proxydial.Choice{Direct: true}), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(initial)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	key := (proxydial.Choice{Direct: true}).Key()
	busy := r.ts[key]

	c.t = c.t.Add(routeIdleTimeout + time.Second)
	get(t, client, srv.URL)
	if r.ts[key] != busy {
		t.Fatal("pruned route with a response body still in use")
	}
	close(finish)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := r.lastUse[key]; !got.Equal(c.t) {
		t.Fatalf("route last use = %v, want response-close time %v", got, c.t)
	}

	c.t = c.t.Add(routeIdleTimeout - time.Second)
	get(t, client, srv.URL)
	if r.ts[key] != busy {
		t.Fatal("route expired before a full idle interval after response close")
	}
	c.t = c.t.Add(2 * time.Second)
	get(t, client, srv.URL)
	if _, ok := r.ts[key]; ok {
		t.Fatal("route remained after a full idle interval")
	}
}

func TestRouterAdmitsRouteBeforeTransportRoundTrip(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(blocked.Close)
	trigger := okServer(t)
	started, resume := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	blockedAddr := blocked.Listener.Addr().String()
	newT := func() *http.Transport {
		return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == blockedAddr {
				once.Do(func() { close(started) })
				<-resume
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	}
	r := NewRouter(time.Hour, 1, 2, newT)
	c := &clock{t: time.Unix(1000, 0)}
	r.now = c.now
	client := &http.Client{Transport: r}
	choice := proxydial.Choice{Direct: true}
	request, err := http.NewRequestWithContext(withChoice(choice), http.MethodGet, blocked.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(request)
		done <- result{resp: resp, err: err}
	}()
	<-started // Route ref is held while the Transport is still connecting.
	key := choice.Key()
	admitted := r.ts[key]
	c.t = c.t.Add(routeIdleTimeout + time.Second)
	get(t, client, trigger.URL) // an unrelated request triggers the inactivity sweep
	if r.ts[key] != admitted {
		t.Fatal("route was pruned between router admission and Transport.RoundTrip")
	}
	unblock()
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	_ = got.resp.Body.Close()
}

func TestRouterPreservesCurrentConfiguredDefaultRoute(t *testing.T) {
	proxydial.SetDefault(&url.URL{Scheme: "http", Host: "default.invalid:8080"})
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	r := NewRouter(time.Hour, 1, 2, plain)
	c := &clock{t: time.Unix(1000, 0)}
	r.now = c.now
	defaultChoice := proxydial.Choice{URL: proxydial.Default()}
	defaultRoute := r.transport(defaultChoice)
	getCtx(t, &http.Client{Transport: r}, okServer(t).URL, withChoice(proxydial.Choice{Direct: true}))
	c.t = c.t.Add(routeIdleTimeout + time.Second)
	getCtx(t, &http.Client{Transport: r}, okServer(t).URL, withChoice(proxydial.Choice{Direct: true}))
	if r.ts[defaultChoice.Key()] != defaultRoute {
		t.Fatal("current configured default route was pruned")
	}
}

func TestRouterKeepsOneTransportPerWayOut(t *testing.T) {
	target := okServer(t)
	proxyA, tunnelsA := connectProxy(t)
	proxyB, tunnelsB := connectProxy(t)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	r := NewRouter(time.Hour, 1, 2, plain)
	client := &http.Client{Transport: r}

	get(t, client, target.URL) // no choice, no default: the environment's way, here direct
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyA}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyA}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyB}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{Direct: true}))
	if a, b := tunnelsA.Load(), tunnelsB.Load(); a != 1 || b != 1 {
		t.Fatalf("tunnels A %d, B %d: want 1 and 1 (the second request to A reuses its connection)", a, b)
	}
	if n := len(r.ts); n != 4 {
		t.Errorf("transports = %d, want 4 (env, A, B, direct)", n)
	}

	// Without a choice on the request the default proxy applies.
	proxydial.SetDefault(proxyB)
	get(t, client, target.URL)
	if b := tunnelsB.Load(); b != 1 {
		t.Errorf("B tunnels = %d: the default proxy is the same way out as B, which already has its connection", b)
	}
	proxydial.SetDefault(nil)
}

func TestRouterDirectIgnoresTheEnvironmentProxy(t *testing.T) {
	target := okServer(t)
	proxy, tunnels := connectProxy(t)
	t.Setenv("HTTP_PROXY", proxy.String())
	t.Setenv("http_proxy", proxy.String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	base := func() *http.Transport { return &http.Transport{Proxy: http.ProxyURL(proxy)} }
	r := NewRouter(time.Hour, 1, 2, base)
	client := &http.Client{Transport: r}
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{Direct: true}))
	if n := tunnels.Load(); n != 0 {
		t.Fatalf("a direct request went through the proxy (%d tunnels)", n)
	}
}

func TestRouterLimitsReachEveryTransport(t *testing.T) {
	r := NewRouter(time.Hour, 3, 5, plain)
	a := r.transport(proxydial.Choice{})
	r.SetLimits(2, 4)
	b := r.transport(proxydial.Choice{Direct: true}) // created after the change
	for name, tr := range map[string]*Transport{"existing": a, "new": b} {
		if x, y := tr.Limits(); x != 2 || y != 4 {
			t.Errorf("%s transport limits = %d/%d, want 2/4", name, x, y)
		}
	}
	if x, y := r.Limits(); x != 2 || y != 4 {
		t.Errorf("router limits = %d/%d", x, y)
	}
	a.counts.dials.Add(2)
	b.counts.dials.Add(3)
	if st := r.Stats(); st.Dials != 5 {
		t.Errorf("stats dials = %d, want 5 (summed)", st.Dials)
	}
}

// Requests for different ways out run side by side without mixing their pools.
func TestRouterConcurrentWaysOut(t *testing.T) {
	srv := okServer(t)
	proxyA, tunnelsA := connectProxy(t)
	proxyB, tunnelsB := connectProxy(t)
	r := NewRouter(time.Hour, 2, 3, nil)
	client := &http.Client{Transport: r, Timeout: 20 * time.Second}
	choices := []proxydial.Choice{{URL: proxyA}, {URL: proxyB}, {Direct: true}}

	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := proxydial.WithChoice(context.Background(), choices[i%len(choices)])
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte("x")))
			resp, err := client.Do(req)
			if err != nil {
				failed.Add(1)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Fatalf("%d requests failed", n)
	}
	if a, b := tunnelsA.Load(), tunnelsB.Load(); a == 0 || b == 0 || a > 10 || b > 10 {
		t.Errorf("tunnels A %d, B %d: each proxy carries its 10 requests, and only those", a, b)
	}
	if n := len(r.ts); n != 3 {
		t.Errorf("transports = %d, want one per way out", n)
	}
}

// A Router built without a transport factory builds pools like New does; Pool
// used to dereference the missing factory.
func TestRouterWithoutFactory(t *testing.T) {
	r := NewRouter(0, 0, 0, nil)
	if p := r.Pool(); p == nil || p.Proxy == nil {
		t.Errorf("Pool() = %+v, want a clone of the default transport", p)
	}
	if a, n := r.Limits(); a != DefaultActive || n != DefaultTotal {
		t.Errorf("limits = %d/%d", a, n)
	}
}
