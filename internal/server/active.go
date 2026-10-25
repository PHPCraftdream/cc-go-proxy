package server

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gofiber/fiber/v2"
)

// localsActiveGuard holds the per-request release guard under /call/.
const localsActiveGuard = "ccgp:active-guard"

// inFlight counts the requests currently being served under /call/.
type inFlight struct{ n atomic.Int64 }

// activeGuard owns the single decrement of one counted request: release is
// safe to call any number of times, from the middleware after the handler
// returned or from a stream writer that took the release over.
type activeGuard struct {
	f     *inFlight
	once  sync.Once
	taken bool // a stream writer owns the release; middleware must not fire
}

func (g *activeGuard) release() { g.once.Do(func() { g.f.dec() }) }

// middleware counts requests under /call/ (model endpoints); /health itself
// must not count. Streaming handlers keep counting until the stream ends:
// deferActive moves the release into the stream writer; otherwise the count
// would drop when the handler returns, while the response still streams.
func (f *inFlight) middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !strings.HasPrefix(c.Path(), callPrefix) {
			return c.Next()
		}
		f.n.Add(1)
		g := &activeGuard{f: f}
		c.Locals(localsActiveGuard, g)
		// Deferred so a panic unwinding to the recover middleware still releases.
		defer func() {
			if !g.taken {
				g.release()
			}
		}()
		return c.Next()
	}
}

// deferActive moves the in-flight release of the current request into the
// caller: the returned func decrements the counter exactly once. Call it
// right before installing a body stream writer and defer the result as the
// first statement inside the writer closure (fiber reuses the context after
// the handler returns, so the closure must not touch c).
func deferActive(c *fiber.Ctx) (release func()) {
	g, ok := c.Locals(localsActiveGuard).(*activeGuard)
	if !ok {
		return func() {}
	}
	g.taken = true
	return g.release
}

func (f *inFlight) dec() { f.n.Add(-1) }

func (f *inFlight) count() int64 { return f.n.Load() }

// exeInfo captures the running binary's path and modification time; called
// once at server start so /health reports the code the server runs.
func exeInfo() (path string, mod int64) {
	path, err := os.Executable()
	if err != nil {
		return "", 0
	}
	if fi, err := os.Stat(path); err == nil {
		mod = fi.ModTime().UnixNano()
	}
	return path, mod
}

// healthPayload is the /health body. exe/exe_mod let the CLI detect a server
// started from an older binary; active reports in-flight model requests.
func healthPayload(f *inFlight, exe string, exeMod int64) fiber.Map {
	return fiber.Map{
		"status":  "ok",
		"service": "ccgp",
		"version": ProxyVersion,
		"pid":     os.Getpid(),
		"exe":     exe,
		"exe_mod": exeMod,
		"active":  f.count(),
	}
}
