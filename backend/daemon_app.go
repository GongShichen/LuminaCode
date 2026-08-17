package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
)

type DaemonApp struct {
	opts       DaemonOptions
	endpoint   EndpointInfo
	listener   net.Listener
	httpServer *http.Server
	server     *DaemonServer
	shutdown   *ShutdownSignal
}

func NewDaemonApp(opts NormalizedDaemonOptions, endpoint EndpointInfo, listener net.Listener, httpServer *http.Server,
	server *DaemonServer, shutdown *ShutdownSignal) *DaemonApp {
	return &DaemonApp{opts: opts.DaemonOptions, endpoint: endpoint, listener: listener, httpServer: httpServer,
		server: server, shutdown: shutdown}
}

func (a *DaemonApp) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := writeEndpoint(a.opts.EndpointPath, a.endpoint); err != nil {
		return err
	}
	defer os.Remove(a.opts.EndpointPath)
	a.server.startManagedServices()
	defer a.server.stopManagedServices()
	go a.server.startIdleHeartbeat(runCtx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpServer.Serve(a.listener) }()
	fmt.Fprintf(os.Stderr, "lumina-backend daemon listening on %s:%d\n", a.endpoint.Host, a.endpoint.Port)

	select {
	case err := <-serveErr:
		return normalizeHTTPServeError(err)
	case <-runCtx.Done():
	case <-a.shutdown.Done():
	}
	_ = a.httpServer.Shutdown(context.Background())
	return normalizeHTTPServeError(<-serveErr)
}

func normalizeHTTPServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
