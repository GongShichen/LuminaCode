package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
)

type DaemonApp struct {
	opts     DaemonOptions
	endpoint EndpointInfo
	listener net.Listener
	hertz    *server.Hertz
	server   *DaemonServer
	shutdown *ShutdownSignal
	router   *ClusterRouter
}

func NewDaemonApp(opts NormalizedDaemonOptions, endpoint EndpointInfo, listener net.Listener,
	hertz *server.Hertz, daemonServer *DaemonServer, shutdown *ShutdownSignal, router *ClusterRouter,
	_ daemonMemoryPreflight) *DaemonApp {
	return &DaemonApp{opts: opts.DaemonOptions, endpoint: endpoint, listener: listener, hertz: hertz,
		server: daemonServer, shutdown: shutdown, router: router}
}

func (a *DaemonApp) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.router != nil {
		if err := a.router.Start(runCtx); err != nil {
			return err
		}
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), a.shutdownGrace())
			defer shutdownCancel()
			_ = a.router.Stop(shutdownCtx)
		}()
	}
	if err := writeEndpoint(a.opts.EndpointPath, a.endpoint); err != nil {
		return err
	}
	endpointPaths := []string{a.opts.EndpointPath}
	if a.opts.Config.UsesClusterRuntime() {
		preferred := DefaultEndpointPath()
		if preferred != "" && preferred != a.opts.EndpointPath {
			if err := writeEndpoint(preferred, a.endpoint); err != nil {
				return err
			}
			endpointPaths = append(endpointPaths, preferred)
		}
	}
	defer func() {
		for _, path := range endpointPaths {
			removeOwnedEndpoint(path, a.endpoint.PID)
		}
		if a.opts.Config.UsesClusterRuntime() {
			promoteClusterEndpoint(DefaultEndpointPath())
		}
	}()
	a.server.startManagedServices()
	defer a.server.stopManagedServices()
	if !a.opts.Config.UsesClusterRuntime() {
		go a.server.startIdleHeartbeat(runCtx)
	}

	processSignals := make(chan os.Signal, 1)
	signal.Notify(processSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(processSignals)
	observed := make(chan error, 1)
	a.hertz.SetCustomSignalWaiter(func(serverErrors chan error) error {
		var cause error
		var drainErr error
		select {
		case cause = <-serverErrors:
		case <-runCtx.Done():
		case <-a.shutdown.Done():
		case <-processSignals:
		}
		if a.router != nil && a.router.Enabled() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), a.shutdownGrace())
			drainErr = a.router.Drain(shutdownCtx)
			shutdownCancel()
		}
		a.server.eventHub.Close()
		if cause == nil && drainErr != nil {
			observed <- drainErr
		} else {
			observed <- cause
		}
		return cause
	})
	spinDone := make(chan struct{})
	go func() {
		defer close(spinDone)
		a.hertz.Spin()
	}()
	fmt.Fprintf(os.Stderr, "lumina-backend daemon listening on %s:%d\n", a.endpoint.Host, a.endpoint.Port)
	cause := <-observed
	<-spinDone
	return cause
}

func removeOwnedEndpoint(path string, pid int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var endpoint EndpointInfo
	if json.Unmarshal(data, &endpoint) == nil && endpoint.PID == pid {
		_ = os.Remove(path)
	}
}

func promoteClusterEndpoint(preferred string) {
	if preferred == "" {
		return
	}
	extension := filepath.Ext(preferred)
	base := strings.TrimSuffix(filepath.Base(preferred), extension)
	candidates, _ := filepath.Glob(filepath.Join(filepath.Dir(preferred), base+"-*"+extension))
	sort.Strings(candidates)
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var endpoint EndpointInfo
		if json.Unmarshal(data, &endpoint) != nil || endpoint.Port <= 0 {
			continue
		}
		host := endpoint.Host
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(endpoint.Port)),
			200*time.Millisecond)
		if err != nil {
			continue
		}
		_ = connection.Close()
		_ = writeEndpoint(preferred, endpoint)
		return
	}
}

func (a *DaemonApp) shutdownGrace() time.Duration {
	seconds := a.opts.Config.ClusterShutdownGraceSeconds
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}
