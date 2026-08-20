package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/session"
	luminateam "LuminaCode/team"

	"github.com/google/uuid"
)

type dispatchIdentityContextKey struct{}

func withDispatchIdentity(ctx context.Context, identity cluster.RuntimeIdentity) context.Context {
	return context.WithValue(ctx, dispatchIdentityContextKey{}, identity)
}

func dispatchIdentity(ctx context.Context) cluster.RuntimeIdentity {
	identity, _ := ctx.Value(dispatchIdentityContextKey{}).(cluster.RuntimeIdentity)
	return identity
}

type clusterRouteTarget struct {
	sessionID string
	commandID string
	create    bool
	direct    bool
	mutating  bool
}

// ClusterRouter is the ownership and transparent-forwarding boundary. It is
// deliberately outside DaemonServer so command consumers and WebSocket
// gateways use exactly the same local dispatcher.
type ClusterRouter struct {
	cfg        config.Config
	runtime    *cluster.RedisRuntime
	repository session.SessionRepository
	sessions   *SessionManager
	teams      *luminateam.Manager
	hub        *EventHub

	handlerMu sync.RWMutex
	handler   func(context.Context, *wsClient, RPCRequest) RPCResponse

	ctx        context.Context
	cancel     context.CancelFunc
	startOnce  sync.Once
	stopOnce   sync.Once
	wg         sync.WaitGroup
	draining   atomic.Bool
	registered atomic.Bool
	startedAt  time.Time

	leaseMu sync.Mutex
	leases  map[string]cluster.SessionLease
	subMu   sync.Mutex
	subs    map[string]func()
}

func NewClusterRouter(cfg config.Config, runtime *cluster.RedisRuntime, repository session.SessionRepository,
	sessions *SessionManager, teams *luminateam.Manager, hub *EventHub) *ClusterRouter {
	return &ClusterRouter{cfg: cfg, runtime: runtime, repository: repository, sessions: sessions,
		teams: teams, hub: hub, leases: map[string]cluster.SessionLease{}, subs: map[string]func(){},
		startedAt: time.Now().UTC()}
}

func (r *ClusterRouter) Enabled() bool {
	return r != nil && r.cfg.UsesClusterRuntime() && r.runtime != nil
}

func (r *ClusterRouter) Draining() bool { return r != nil && r.draining.Load() }

func (r *ClusterRouter) SetHandler(handler func(context.Context, *wsClient, RPCRequest) RPCResponse) {
	r.handlerMu.Lock()
	r.handler = handler
	r.handlerMu.Unlock()
}

func (r *ClusterRouter) Start(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	var startErr error
	r.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		r.ctx, r.cancel = context.WithCancel(ctx)
		if err := r.register(r.ctx, false); err != nil {
			startErr = err
			r.cancel()
			return
		}
		r.registered.Store(true)
		r.wg.Add(3)
		go r.heartbeatLoop()
		go r.commandLoop()
		go r.outboxLoop()
	})
	return startErr
}

func (r *ClusterRouter) Stop(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	var stopErr error
	r.stopOnce.Do(func() {
		if err := r.Drain(ctx); err != nil {
			stopErr = err
		}
		if r.cancel != nil {
			r.cancel()
		}
		r.subMu.Lock()
		for key, cancel := range r.subs {
			cancel()
			delete(r.subs, key)
		}
		r.subMu.Unlock()
		r.wg.Wait()
		if r.registered.Swap(false) {
			if err := r.runtime.UnregisterInstance(context.WithoutCancel(ctx), r.cfg.InstanceID); err != nil && stopErr == nil {
				stopErr = err
			}
		}
	})
	return stopErr
}

func (r *ClusterRouter) Drain(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	r.draining.Store(true)
	_ = r.register(ctx, true)
	r.leaseMu.Lock()
	leases := make(map[string]cluster.SessionLease, len(r.leases))
	for key, lease := range r.leases {
		leases[key] = lease
	}
	r.leaseMu.Unlock()
	var errs []error
	for key, lease := range leases {
		tenantID, sessionID := splitClusterSessionKey(key)
		r.teams.ReleaseParentFor(tenantID, sessionID)
		r.sessions.ReleaseFor(cluster.Principal{TenantID: tenantID}, sessionID)
		if err := lease.Release(ctx); err != nil {
			errs = append(errs, err)
		}
		r.leaseMu.Lock()
		delete(r.leases, key)
		r.leaseMu.Unlock()
	}
	return errors.Join(errs...)
}

func (r *ClusterRouter) Ready(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	if !r.registered.Load() || r.draining.Load() {
		return errors.New("cluster instance is not accepting ownership")
	}
	if err := r.runtime.Ping(ctx); err != nil {
		return err
	}
	instance, err := r.runtime.Instance(ctx, r.cfg.InstanceID)
	if err != nil {
		return err
	}
	if instance == nil || instance.Draining {
		return errors.New("cluster instance registration is missing or draining")
	}
	if err := r.repository.Health(ctx); err != nil {
		return err
	}
	if r.cfg.LongTermMemoryEnabled {
		postgresRepository, ok := r.repository.(*session.PostgresRepository)
		if !ok {
			return errors.New("cluster Memory Fabric requires PostgreSQL repository")
		}
		var version int
		if err := postgresRepository.Pool().QueryRow(ctx, `SELECT version FROM lumina_schema_migrations
			WHERE component='memory'`).Scan(&version); err != nil {
			return err
		}
		if version != memory.PostgresSchemaVersion() {
			return fmt.Errorf("PostgreSQL Memory Fabric schema version=%d, want %d",
				version, memory.PostgresSchemaVersion())
		}
	}
	return nil
}

func (r *ClusterRouter) Dispatch(ctx context.Context, client *wsClient, request RPCRequest) RPCResponse {
	if !r.Enabled() {
		return r.callLocal(ctx, client, request)
	}
	if _, err := uuid.Parse(request.ID); err != nil {
		return RPCResponse{ID: request.ID, OK: false,
			Error: &RPCError{Code: "invalid_request_id", Message: "cluster RPC id must be a UUID"}}
	}
	target, err := r.routeTarget(ctx, client.principal, request)
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_route_failed", err)}
	}
	if target.direct || target.sessionID == "" {
		if request.Method == "cluster.instance.drain" {
			if err := r.Drain(ctx); err != nil {
				return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_drain_failed", err)}
			}
			return RPCResponse{ID: request.ID, OK: true, Result: map[string]any{"draining": true,
				"instance_id": r.cfg.InstanceID}}
		}
		return r.callLocal(ctx, client, request)
	}
	owner, identity, err := r.ensureOwner(ctx, client.principal, target.sessionID, target.create, request.Params)
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_owner_unavailable", err)}
	}
	r.watchSession(client.principal.TenantID, target.sessionID)
	if target.create {
		request.Params = mergeStringParam(request.Params, "session_id", target.sessionID)
	}
	if request.Method == "session.submit" {
		request.Params = ensureStringParam(request.Params, "command_id", request.ID)
	}
	target.commandID = stringParam(request.Params, "command_id")
	if target.commandID == "" {
		target.commandID = request.ID
	}
	if owner.InstanceID == r.cfg.InstanceID {
		ctx = withDispatchIdentity(ctx, identity)
		response := r.callIdempotentLocal(ctx, client, request, target)
		if target.create && !response.OK {
			r.releaseLease(client.principal, target.sessionID)
		}
		return response
	}
	deadline := time.Now().Add(time.Duration(r.cfg.ClusterRPCTimeoutSeconds) * time.Second)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	commandID := request.ID
	var parameters map[string]json.RawMessage
	_ = json.Unmarshal(request.Params, &parameters)
	if raw := parameters["command_id"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &commandID)
	}
	response, err := r.runtime.Forward(ctx, cluster.CommandEnvelope{RequestID: request.ID,
		CommandID: commandID, GatewayInstanceID: r.cfg.InstanceID, OwnerInstanceID: owner.InstanceID,
		TenantID: client.principal.TenantID, Subject: client.principal.Subject,
		SessionID: target.sessionID, Method: request.Method, Params: request.Params, Deadline: deadline,
		FenceToken: owner.FenceToken})
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_rpc_failed", err)}
	}
	result := RPCResponse{ID: request.ID, OK: response.OK}
	if response.OK {
		if len(response.Result) > 0 {
			_ = json.Unmarshal(response.Result, &result.Result)
		}
	} else {
		result.Error = &RPCError{Code: response.ErrorCode, Message: response.Error}
	}
	if request.Method == "session.create" || request.Method == "session.resume" || request.Method == "session.snapshot" {
		client.setSessionID(target.sessionID)
	}
	return result
}

func (r *ClusterRouter) callLocal(ctx context.Context, client *wsClient, request RPCRequest) RPCResponse {
	r.handlerMu.RLock()
	handler := r.handler
	r.handlerMu.RUnlock()
	if handler == nil {
		return RPCResponse{ID: request.ID, OK: false,
			Error: &RPCError{Code: "cluster_not_ready", Message: "local dispatcher is unavailable"}}
	}
	return handler(ctx, client, request)
}

func (r *ClusterRouter) callIdempotentLocal(ctx context.Context, client *wsClient, request RPCRequest,
	target clusterRouteTarget) RPCResponse {
	controller, _ := r.sessions.GetFor(client.principal, target.sessionID)
	cacheKey := target.commandID
	if cacheKey == "" {
		cacheKey = request.ID
	}
	if target.mutating && controller != nil {
		if cached, err := controller.CachedCommandResponse(ctx, cacheKey); err == nil && cached != nil {
			cached.ID = request.ID
			return *cached
		}
	}
	response := r.callLocal(ctx, client, request)
	if target.mutating && response.OK {
		if controller == nil {
			controller, _ = r.sessions.GetFor(client.principal, target.sessionID)
		}
		if controller != nil {
			storedResponse := response
			storedResponse.ID = cacheKey
			if err := controller.SaveCommandResponse(context.WithoutCancel(ctx), storedResponse); err != nil {
				return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("command_result_commit_failed", err)}
			}
		}
	}
	return response
}

func (r *ClusterRouter) ensureOwner(ctx context.Context, principal cluster.Principal, sessionID string,
	create bool, params json.RawMessage) (cluster.Owner, cluster.RuntimeIdentity, error) {
	owner, err := r.runtime.Lookup(ctx, principal.TenantID, sessionID)
	if err != nil {
		return cluster.Owner{}, cluster.RuntimeIdentity{}, err
	}
	if owner == nil {
		if r.draining.Load() {
			return cluster.Owner{}, cluster.RuntimeIdentity{}, errors.New("instance is draining")
		}
		lease, err := r.runtime.Acquire(ctx, principal.TenantID, sessionID, r.cfg.InstanceID,
			time.Duration(r.cfg.ClusterLeaseTTLSeconds)*time.Second,
			time.Duration(r.cfg.ClusterLeaseRenewSeconds)*time.Second)
		if err != nil {
			if errors.Is(err, cluster.ErrSessionOwned) {
				owner, err = r.runtime.Lookup(ctx, principal.TenantID, sessionID)
				if err == nil && owner != nil {
					return *owner, runtimeIdentity(principal, r.cfg.InstanceID, sessionID, *owner), nil
				}
			}
			return cluster.Owner{}, cluster.RuntimeIdentity{}, err
		}
		ownerValue := lease.Owner()
		identity := runtimeIdentity(principal, r.cfg.InstanceID, sessionID, ownerValue)
		cwd := stringParam(params, "cwd")
		r.installLease(principal, sessionID, lease)
		if !create {
			controller, resumeErr := r.sessions.ResumeFor(principal, sessionID, cwd, ownerValue.FenceToken)
			if resumeErr != nil {
				r.releaseLease(principal, sessionID)
				return cluster.Owner{}, cluster.RuntimeIdentity{}, resumeErr
			}
			if err = r.restoreTeams(principal.TenantID, controller, cwd); err != nil {
				r.releaseLease(principal, sessionID)
				return cluster.Owner{}, cluster.RuntimeIdentity{}, err
			}
		}
		return ownerValue, identity, nil
	}
	identity := runtimeIdentity(principal, r.cfg.InstanceID, sessionID, *owner)
	if owner.InstanceID == r.cfg.InstanceID {
		key := clusterSessionKey(principal.TenantID, sessionID)
		r.leaseMu.Lock()
		lease := r.leases[key]
		r.leaseMu.Unlock()
		if lease == nil || lease.Owner().LeaseID != owner.LeaseID {
			return cluster.Owner{}, cluster.RuntimeIdentity{}, errors.New("local owner lease is no longer attached")
		}
		if _, getErr := r.sessions.GetFor(principal, sessionID); getErr != nil {
			controller, resumeErr := r.sessions.ResumeFor(principal, sessionID, stringParam(params, "cwd"), owner.FenceToken)
			if resumeErr != nil {
				return cluster.Owner{}, cluster.RuntimeIdentity{}, resumeErr
			}
			if err := r.restoreTeams(principal.TenantID, controller, stringParam(params, "cwd")); err != nil {
				return cluster.Owner{}, cluster.RuntimeIdentity{}, err
			}
		}
	}
	return *owner, identity, nil
}

func (r *ClusterRouter) restoreTeams(tenantID string, controller *SessionController, cwd string) error {
	if controller == nil {
		return nil
	}
	checkpoints, err := controller.TeamRuntimeCheckpoints(context.Background())
	if err != nil {
		return err
	}
	if len(checkpoints) > 0 {
		r.teams.RestoreRuntimeCheckpointsWithIdentity(controller.RuntimeIdentity(), cwd, checkpoints)
	}
	return nil
}

func runtimeIdentity(principal cluster.Principal, instanceID, sessionID string,
	owner cluster.Owner) cluster.RuntimeIdentity {
	return cluster.RuntimeIdentity{TenantID: principal.TenantID, Subject: principal.Subject,
		InstanceID: instanceID, SessionID: sessionID, FenceToken: owner.FenceToken}
}

func (r *ClusterRouter) installLease(principal cluster.Principal, sessionID string, lease cluster.SessionLease) {
	key := clusterSessionKey(principal.TenantID, sessionID)
	r.leaseMu.Lock()
	r.leases[key] = lease
	r.leaseMu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case <-lease.Lost():
		case <-r.ctx.Done():
		}
		r.teams.ReleaseParentFor(principal.TenantID, sessionID)
		r.sessions.ReleaseFor(principal, sessionID)
		r.leaseMu.Lock()
		if r.leases[key] == lease {
			delete(r.leases, key)
		}
		r.leaseMu.Unlock()
	}()
}

func (r *ClusterRouter) routeTarget(ctx context.Context, principal cluster.Principal,
	request RPCRequest) (clusterRouteTarget, error) {
	target := clusterRouteTarget{mutating: isMutatingRPC(request.Method)}
	switch request.Method {
	case "backend.status", "backend.shutdown", "cluster.instance.drain", "session.list", "session.events",
		"session.pin", "storage.status", "storage.cleanup", "team.list", "team.create_template",
		"memory.search", "memory.doctor":
		target.direct = true
		return target, nil
	case "session.create":
		target.create = true
		target.sessionID = request.ID
		return target, nil
	case "session.exit":
		target.direct = true
		return target, nil
	}
	target.sessionID = stringParam(request.Params, "session_id")
	if target.sessionID != "" {
		return target, nil
	}
	teamSessionID := stringParam(request.Params, "team_session_id")
	if teamSessionID != "" {
		parent, err := r.repository.ResolveParentSession(ctx, principal.TenantID, teamSessionID)
		if err != nil {
			return target, err
		}
		target.sessionID = parent
		return target, nil
	}
	// Static commands with no session identity can execute at any gateway.
	target.direct = true
	return target, nil
}

func (r *ClusterRouter) releaseLease(principal cluster.Principal, sessionID string) {
	key := clusterSessionKey(principal.TenantID, sessionID)
	r.leaseMu.Lock()
	lease := r.leases[key]
	delete(r.leases, key)
	r.leaseMu.Unlock()
	if lease != nil {
		r.teams.ReleaseParentFor(principal.TenantID, sessionID)
		r.sessions.ReleaseFor(principal, sessionID)
		_ = lease.Release(context.Background())
	}
}

func isMutatingRPC(method string) bool {
	switch method {
	case "backend.status", "session.list", "session.snapshot", "session.events", "session.tokens",
		"runtime.describe", "storage.status", "team.list", "team.snapshot", "team.status",
		"team.artifacts", "team.timeline", "team.dialogue", "team.summary", "team.detail",
		"memory.search", "memory.doctor", "slash.list", "skills.list", "mcp.list":
		return false
	default:
		return true
	}
}

func (r *ClusterRouter) commandLoop() {
	defer r.wg.Done()
	consumer := r.cfg.InstanceID + "-" + uuid.NewString()
	for {
		commands, err := r.runtime.Read(r.ctx, r.cfg.InstanceID, consumer, 16, time.Second)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			slog.Warn("read cluster commands", "error", err)
			continue
		}
		for _, command := range commands {
			r.handleCommand(command)
		}
	}
}

func (r *ClusterRouter) handleCommand(command cluster.CommandEnvelope) {
	ctx, cancel := context.WithDeadline(r.ctx, command.Deadline)
	defer cancel()
	owner, err := r.runtime.Lookup(ctx, command.TenantID, command.SessionID)
	if err != nil || owner == nil || owner.InstanceID != r.cfg.InstanceID || owner.FenceToken != command.FenceToken {
		r.respondCommand(ctx, command, RPCResponse{ID: command.RequestID, OK: false,
			Error: &RPCError{Code: "owner_changed", Message: "session owner changed before dispatch"}})
		return
	}
	principal := cluster.Principal{TenantID: command.TenantID, Subject: command.Subject,
		Scopes: map[string]struct{}{"lumina:admin": {}}}
	controller, controllerErr := r.sessions.GetFor(principal, command.SessionID)
	if controllerErr != nil {
		controller, controllerErr = r.sessions.ResumeFor(principal, command.SessionID, "", command.FenceToken)
		if controllerErr != nil {
			r.respondCommand(ctx, command, RPCResponse{ID: command.RequestID, OK: false,
				Error: toRPCError("session_resume_failed", controllerErr)})
			return
		}
		if err := r.restoreTeams(command.TenantID, controller, ""); err != nil {
			r.respondCommand(ctx, command, RPCResponse{ID: command.RequestID, OK: false,
				Error: toRPCError("team_restore_failed", err)})
			return
		}
	}
	if strings.HasPrefix(command.Method, "a2a.") {
		response := r.handleLocalA2A(ctx, principal, controller, command.RequestID, command.TeamSessionID,
			command.A2AAgentID, strings.TrimPrefix(command.Method, "a2a."), command.Params)
		r.respondCommand(context.WithoutCancel(ctx), command, response)
		return
	}
	client := &wsClient{principal: principal, clusterMode: true}
	client.setSessionID(command.SessionID)
	request := RPCRequest{ID: command.RequestID, Method: command.Method, Params: command.Params}
	target := clusterRouteTarget{sessionID: command.SessionID, commandID: command.CommandID,
		mutating: isMutatingRPC(command.Method)}
	response := r.callIdempotentLocal(withDispatchIdentity(ctx,
		runtimeIdentity(principal, r.cfg.InstanceID, command.SessionID, *owner)), client, request, target)
	r.respondCommand(context.WithoutCancel(ctx), command, response)
}

func (r *ClusterRouter) HandleA2A(ctx context.Context, principal cluster.Principal, request RPCRequest,
	teamSessionID, agentID string) RPCResponse {
	if _, err := uuid.Parse(request.ID); err != nil {
		return RPCResponse{ID: request.ID, OK: false,
			Error: &RPCError{Code: "invalid_request_id", Message: "cluster RPC id must be a UUID"}}
	}
	parentSessionID, err := r.repository.ResolveParentSession(ctx, principal.TenantID, teamSessionID)
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("team_session_not_found", err)}
	}
	owner, _, err := r.ensureOwner(ctx, principal, parentSessionID, false, nil)
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_owner_unavailable", err)}
	}
	r.watchSession(principal.TenantID, parentSessionID)
	if owner.InstanceID == r.cfg.InstanceID {
		controller, getErr := r.sessions.GetFor(principal, parentSessionID)
		if getErr != nil {
			return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("session_not_found", getErr)}
		}
		return r.handleLocalA2A(ctx, principal, controller, request.ID, teamSessionID, agentID,
			request.Method, request.Params)
	}
	deadline := time.Now().Add(time.Duration(r.cfg.ClusterRPCTimeoutSeconds) * time.Second)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	response, err := r.runtime.Forward(ctx, cluster.CommandEnvelope{RequestID: request.ID,
		CommandID: request.ID, GatewayInstanceID: r.cfg.InstanceID, OwnerInstanceID: owner.InstanceID,
		TenantID: principal.TenantID, Subject: principal.Subject, SessionID: parentSessionID,
		TeamSessionID: teamSessionID, A2AAgentID: agentID, Method: "a2a." + request.Method,
		Params: request.Params, Deadline: deadline, FenceToken: owner.FenceToken})
	if err != nil {
		return RPCResponse{ID: request.ID, OK: false, Error: toRPCError("cluster_rpc_failed", err)}
	}
	result := RPCResponse{ID: request.ID, OK: response.OK}
	if response.OK {
		_ = json.Unmarshal(response.Result, &result.Result)
	} else {
		result.Error = &RPCError{Code: response.ErrorCode, Message: response.Error}
	}
	return result
}

func (r *ClusterRouter) handleLocalA2A(ctx context.Context, principal cluster.Principal,
	controller *SessionController, requestID, teamSessionID, agentID, method string,
	params json.RawMessage) RPCResponse {
	if cached, err := controller.CachedCommandResponse(ctx, requestID); err == nil && cached != nil {
		return *cached
	}
	result, err := r.teams.HandleA2AFor(ctx, principal.TenantID, teamSessionID, agentID, method, params)
	response := RPCResponse{ID: requestID, OK: err == nil, Result: result}
	if err != nil {
		response.Error = &RPCError{Code: "a2a_error", Message: err.Error()}
		return response
	}
	if err := controller.SaveCommandResponse(context.WithoutCancel(ctx), response); err != nil {
		return RPCResponse{ID: requestID, OK: false, Error: toRPCError("command_result_commit_failed", err)}
	}
	return response
}

func (r *ClusterRouter) respondCommand(ctx context.Context, command cluster.CommandEnvelope, response RPCResponse) {
	responseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(response.Result)
	clusterResponse := cluster.CommandResponse{RequestID: command.RequestID, OK: response.OK, Result: raw}
	if response.Error != nil {
		clusterResponse.ErrorCode = response.Error.Code
		clusterResponse.Error = response.Error.Message
	}
	if err := r.runtime.Respond(responseCtx, command.GatewayInstanceID, clusterResponse); err != nil {
		slog.Warn("respond cluster command", "request_id", command.RequestID, "error", err)
		return
	}
	if err := r.runtime.Ack(responseCtx, r.cfg.InstanceID, command.StreamID); err != nil {
		slog.Warn("ack cluster command", "request_id", command.RequestID, "error", err)
	}
}

func (r *ClusterRouter) heartbeatLoop() {
	defer r.wg.Done()
	interval := time.Duration(r.cfg.ClusterLeaseRenewSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			if err := r.register(r.ctx, r.draining.Load()); err != nil {
				r.registered.Store(false)
				slog.Warn("refresh cluster instance registration", "error", err)
			} else {
				r.registered.Store(true)
			}
		}
	}
}

func (r *ClusterRouter) outboxLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
		records, err := r.repository.ClaimOutbox(r.ctx, r.cfg.InstanceID, 100, 30*time.Second)
		if err != nil {
			slog.Warn("claim session outbox", "error", err)
			continue
		}
		for _, record := range records {
			event := PushEvent{TenantID: record.TenantID, Type: "event", ProtocolVersion: 3,
				SessionID: record.SessionID, StreamID: record.Event.StreamID, Seq: record.Event.Seq,
				EventID: record.Event.ID, EventType: record.Event.Type, SchemaVersion: record.Event.SchemaVersion,
				Durable: true, Timestamp: record.Event.OccurredAt.UTC().Format(time.RFC3339Nano),
				Payload: json.RawMessage(record.Event.Payload),
				Event:   map[string]any{"type": "runtime.event", "payload": record.Event}}
			payload, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				continue
			}
			r.hub.Publish(event)
			publishErr := r.runtime.Publish(r.ctx, cluster.EventNotification{TenantID: record.TenantID,
				SessionID: record.SessionID, OriginInstance: r.cfg.InstanceID, EventID: event.EventID,
				Seq: event.Seq, Payload: payload})
			if publishErr != nil {
				slog.Warn("publish session outbox", "session_id", record.SessionID, "error", publishErr)
				continue
			}
			if err := r.repository.MarkOutboxPublished(r.ctx, r.cfg.InstanceID, []int64{record.ID}); err != nil {
				slog.Warn("mark session outbox published", "session_id", record.SessionID, "error", err)
			}
		}
	}
}

func (r *ClusterRouter) register(ctx context.Context, draining bool) error {
	return r.runtime.RegisterInstance(ctx, cluster.InstanceInfo{InstanceID: r.cfg.InstanceID,
		AdvertiseAddr: r.cfg.ClusterAdvertiseAddr, Draining: draining, StartedAt: r.startedAt},
		time.Duration(r.cfg.ClusterLeaseTTLSeconds)*time.Second)
}

func (r *ClusterRouter) watchSession(tenantID, sessionID string) {
	key := clusterSessionKey(tenantID, sessionID)
	r.subMu.Lock()
	if _, exists := r.subs[key]; exists {
		r.subMu.Unlock()
		return
	}
	ch, cancel, err := r.runtime.Subscribe(r.ctx, tenantID, sessionID)
	if err != nil {
		r.subMu.Unlock()
		slog.Warn("subscribe cluster session events", "session_id", sessionID, "error", err)
		return
	}
	r.subs[key] = cancel
	r.subMu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for notification := range ch {
			if notification.OriginInstance == r.cfg.InstanceID {
				continue
			}
			var event PushEvent
			if json.Unmarshal(notification.Payload, &event) != nil {
				continue
			}
			event.TenantID = notification.TenantID
			r.hub.Publish(event)
		}
	}()
}

func stringParam(raw json.RawMessage, key string) string {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return ""
	}
	var value string
	_ = json.Unmarshal(values[key], &value)
	return strings.TrimSpace(value)
}

func mergeStringParam(raw json.RawMessage, key, value string) json.RawMessage {
	values := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &values)
	encoded, _ := json.Marshal(value)
	values[key] = encoded
	result, _ := json.Marshal(values)
	return result
}

func ensureStringParam(raw json.RawMessage, key, value string) json.RawMessage {
	if stringParam(raw, key) != "" {
		return raw
	}
	return mergeStringParam(raw, key, value)
}

func clusterSessionKey(tenantID, sessionID string) string { return tenantID + "\x00" + sessionID }

func splitClusterSessionKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	if len(parts) != 2 {
		return "", key
	}
	return parts[0], parts[1]
}

func (r *ClusterRouter) String() string {
	return fmt.Sprintf("cluster=%s instance=%s", r.cfg.ClusterID, r.cfg.InstanceID)
}
