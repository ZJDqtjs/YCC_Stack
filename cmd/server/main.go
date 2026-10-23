package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/hub"
	"starstack/internal/store"
)

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	data := flag.String("data", "./data", "job/segment storage directory")
	base := flag.String("base-url", "", "public base URL advertised to workers (default: derived from --addr)")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary")
	ffprobe := flag.String("ffprobe", "ffprobe", "ffprobe binary")
	public := flag.Bool("public", false, "internet-facing profile: no broker-local paths, "+
		"no pairing-code list, no self-service recharge, agents must present --agent-token")
	agentToken := flag.String("agent-token", "", "shared secret agents present in X-Agent-Token (required with --public); "+
		"prefer STARSTACK_AGENT_TOKEN in the service environment so the secret does not show up in the process list")
	maxUpload := flag.Int64("max-upload-mb", 0, "request body cap for a job, MiB (0 = profile default)")
	cookiePath := flag.String("cookie-path", "", "Path the session cookie is scoped to, e.g. /starstack/ "+
		"(set this when a reverse proxy mounts the broker under a prefix)")
	trustProxy := flag.Bool("trust-proxy", false, "read the client address from X-Forwarded-For "+
		"(only correct when a proxy you control sets that header)")
	nodeBin := flag.String("node-bin-dir", "", "directory holding starstack-worker-<os>-<arch>[.exe], "+
		"served under /dl/ so the console can hand out a double-click launcher")
	flag.Parse()

	ffx.Ffmpeg, ffx.Ffprobe = *ffmpeg, *ffprobe
	if *agentToken == "" {
		*agentToken = os.Getenv("STARSTACK_AGENT_TOKEN")
	}
	*data, _ = filepath.Abs(*data)
	os.MkdirAll(*data, 0o700)

	pol := hub.DefaultPolicy()
	if *public {
		pol = hub.PublicPolicy()
		if *agentToken == "" {
			log.Fatal("--public needs an agent token (--agent-token or STARSTACK_AGENT_TOKEN): " +
				"without it anyone can register a fake node and bid for real work")
		}
	}
	pol.AgentToken = *agentToken
	pol.TrustProxy = *trustProxy
	if *cookiePath != "" {
		if !strings.HasPrefix(*cookiePath, "/") {
			log.Fatal("--cookie-path must start with /")
		}
		if !strings.HasSuffix(*cookiePath, "/") {
			*cookiePath += "/"
		}
		pol.CookiePath = *cookiePath
		if !pol.TrustProxy {
			log.Printf("note: --cookie-path set but --trust-proxy off; the auth rate limiter "+
				"will count every request as coming from the proxy at %s", *cookiePath)
		}
	}
	if *maxUpload > 0 {
		pol.MaxUploadBytes = *maxUpload << 20
		if pol.MemoryBuffer > pol.MaxUploadBytes {
			pol.MemoryBuffer = pol.MaxUploadBytes / 4
		}
	}

	st, err := store.Open(filepath.Join(*data, "starstack.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if *base == "" {
		p := *addr
		if i := strings.LastIndex(p, ":"); i >= 0 {
			p = p[i:]
		} else {
			p = ":8787"
		}
		*base = "http://127.0.0.1" + p
		log.Printf("note: --base-url not set, workers on other machines must be able to reach %s", *base)
	}
	log.Printf("starstack broker on %s (data=%s, source URL handed to workers: %s, profile=%s)",
		*addr, *data, *base, map[bool]string{true: "public", false: "demo"}[pol.Public])

	srv := hub.NewServer(*data, *base, st, pol)
	srv.SetNodeBinDir(*nodeBin)
	if *nodeBin == "" {
		log.Printf("note: --node-bin-dir not set, the console's one-click launcher has no " +
			"starstack-worker binary to hand out (put starstack-worker-<os>-<arch>[.exe] in a directory and point at it)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunScheduler(ctx)

	hs := &http.Server{
		Addr: *addr, Handler: srv.Handlers(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       3 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		// Read/Write stay open: agents long-poll /api/claim for minutes and a job
		// upload is limited by MaxBytesReader, not by a socket deadline.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(shutCtx)
	}()
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
