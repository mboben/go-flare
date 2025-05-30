// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.uber.org/zap"

	"github.com/ava-labs/avalanchego/api/health"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/message"
	"github.com/ava-labs/avalanchego/proto/pb/p2p"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/networking/tracker"
	"github.com/ava-labs/avalanchego/snow/networking/worker"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/subnets"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
)

const (
	numDispatchersToClose = 3
	// If a consensus message takes longer than this to process, the handler
	// will log a warning.
	syncProcessingTimeWarnLimit = 30 * time.Second
)

var (
	_ Handler = (*handler)(nil)

	errMissingEngine  = errors.New("missing engine")
	errNoStartingGear = errors.New("failed to select starting gear")
)

type Handler interface {
	common.Timer
	health.Checker

	Context() *snow.ConsensusContext
	// ShouldHandle returns true if the node with the given ID is allowed to send
	// messages to this chain. If the node is not allowed to send messages to
	// this chain, the message should be dropped.
	ShouldHandle(nodeID ids.NodeID) bool

	SetEngineManager(engineManager *EngineManager)
	GetEngineManager() *EngineManager

	SetOnStopped(onStopped func())
	Start(ctx context.Context, recoverPanic bool)
	Push(ctx context.Context, msg Message)
	Len() int
	Stop(ctx context.Context)
	StopWithError(ctx context.Context, err error)
	Stopped() chan struct{}
}

// handler passes incoming messages from the network to the consensus engine.
// (Actually, it receives the incoming messages from a ChainRouter, but same difference.)
type handler struct {
	metrics *metrics

	// Useful for faking time in tests
	clock mockable.Clock

	ctx *snow.ConsensusContext
	// The validator set that validates this chain
	validators validators.Set
	// Receives messages from the VM
	msgFromVMChan   <-chan common.Message
	preemptTimeouts chan struct{}
	gossipFrequency time.Duration

	engineManager *EngineManager

	// onStopped is called in a goroutine when this handler finishes shutting
	// down. If it is nil then it is skipped.
	onStopped func()

	// Tracks cpu/disk usage caused by each peer.
	resourceTracker tracker.ResourceTracker

	// Holds messages that [engine] hasn't processed yet.
	// [unprocessedMsgsCond.L] must be held while accessing [syncMessageQueue].
	syncMessageQueue MessageQueue
	// Holds messages that [engine] hasn't processed yet.
	// [unprocessedAsyncMsgsCond.L] must be held while accessing [asyncMessageQueue].
	asyncMessageQueue MessageQueue
	// Worker pool for handling asynchronous consensus messages
	asyncMessagePool worker.Pool
	timeouts         chan struct{}

	closeOnce            sync.Once
	closingChan          chan struct{}
	numDispatchersClosed int
	// Closed when this handler and [engine] are done shutting down
	closed chan struct{}

	subnetConnector validators.SubnetConnector

	subnetAllower subnets.Allower
}

// Initialize this consensus handler
// [engine] must be initialized before initializing this handler
func New(
	ctx *snow.ConsensusContext,
	validators validators.Set,
	msgFromVMChan <-chan common.Message,
	gossipFrequency time.Duration,
	threadPoolSize int,
	resourceTracker tracker.ResourceTracker,
	subnetConnector validators.SubnetConnector,
	subnet subnets.Subnet,
) (Handler, error) {
	h := &handler{
		ctx:              ctx,
		validators:       validators,
		msgFromVMChan:    msgFromVMChan,
		preemptTimeouts:  subnet.OnBootstrapCompleted(),
		gossipFrequency:  gossipFrequency,
		asyncMessagePool: worker.NewPool(threadPoolSize),
		timeouts:         make(chan struct{}, 1),
		closingChan:      make(chan struct{}),
		closed:           make(chan struct{}),
		resourceTracker:  resourceTracker,
		subnetConnector:  subnetConnector,
		subnetAllower:    subnet,
	}

	var err error

	h.metrics, err = newMetrics("handler", h.ctx.Registerer)
	if err != nil {
		return nil, fmt.Errorf("initializing handler metrics errored with: %w", err)
	}
	cpuTracker := resourceTracker.CPUTracker()
	h.syncMessageQueue, err = NewMessageQueue(h.ctx.Log, h.validators, cpuTracker, "handler", h.ctx.Registerer, message.SynchronousOps)
	if err != nil {
		return nil, fmt.Errorf("initializing sync message queue errored with: %w", err)
	}
	h.asyncMessageQueue, err = NewMessageQueue(h.ctx.Log, h.validators, cpuTracker, "handler_async", h.ctx.Registerer, message.AsynchronousOps)
	if err != nil {
		return nil, fmt.Errorf("initializing async message queue errored with: %w", err)
	}
	return h, nil
}

func (h *handler) Context() *snow.ConsensusContext {
	return h.ctx
}

func (h *handler) ShouldHandle(nodeID ids.NodeID) bool {
	return h.subnetAllower.IsAllowed(nodeID, h.validators.Contains(nodeID))
}

func (h *handler) SetEngineManager(engineManager *EngineManager) {
	h.engineManager = engineManager
}

func (h *handler) GetEngineManager() *EngineManager {
	return h.engineManager
}

func (h *handler) SetOnStopped(onStopped func()) {
	h.onStopped = onStopped
}

func (h *handler) selectStartingGear(ctx context.Context) (common.Engine, error) {
	state := h.ctx.State.Get()
	engines := h.engineManager.Get(state.Type)
	if engines == nil {
		return nil, errNoStartingGear
	}
	if engines.StateSyncer == nil {
		return engines.Bootstrapper, nil
	}

	stateSyncEnabled, err := engines.StateSyncer.IsEnabled(ctx)
	if err != nil {
		return nil, err
	}

	if !stateSyncEnabled {
		return engines.Bootstrapper, nil
	}

	// drop bootstrap state from previous runs before starting state sync
	return engines.StateSyncer, engines.Bootstrapper.Clear()
}

func (h *handler) Start(ctx context.Context, recoverPanic bool) {
	h.ctx.Lock.Lock()
	defer h.ctx.Lock.Unlock()

	gear, err := h.selectStartingGear(ctx)
	if err != nil {
		h.ctx.Log.Error("chain failed to select starting gear",
			zap.Error(err),
		)
		h.shutdown(ctx)
		return
	}

	if err := gear.Start(ctx, 0); err != nil {
		h.ctx.Log.Error("chain failed to start",
			zap.Error(err),
		)
		h.shutdown(ctx)
		return
	}

	detachedCtx := utils.Detach(ctx)
	dispatchSync := func() {
		h.dispatchSync(detachedCtx)
	}
	dispatchAsync := func() {
		h.dispatchAsync(detachedCtx)
	}
	dispatchChans := func() {
		h.dispatchChans(detachedCtx)
	}
	if recoverPanic {
		go h.ctx.Log.RecoverAndExit(dispatchSync, func() {
			h.ctx.Log.Error("chain was shutdown due to a panic in the sync dispatcher")
		})
		go h.ctx.Log.RecoverAndExit(dispatchAsync, func() {
			h.ctx.Log.Error("chain was shutdown due to a panic in the async dispatcher")
		})
		go h.ctx.Log.RecoverAndExit(dispatchChans, func() {
			h.ctx.Log.Error("chain was shutdown due to a panic in the chan dispatcher")
		})
	} else {
		go h.ctx.Log.RecoverAndPanic(dispatchSync)
		go h.ctx.Log.RecoverAndPanic(dispatchAsync)
		go h.ctx.Log.RecoverAndPanic(dispatchChans)
	}
}

func (h *handler) HealthCheck(ctx context.Context) (interface{}, error) {
	h.ctx.Lock.Lock()
	defer h.ctx.Lock.Unlock()

	state := h.ctx.State.Get()
	engine, ok := h.engineManager.Get(state.Type).Get(state.State)
	if !ok {
		return nil, fmt.Errorf(
			"%w %s running %s",
			errMissingEngine,
			state.State,
			state.Type,
		)
	}
	return engine.HealthCheck(ctx)
}

// Push the message onto the handler's queue
func (h *handler) Push(ctx context.Context, msg Message) {
	switch msg.Op() {
	case message.AppRequestOp, message.AppRequestFailedOp, message.AppResponseOp, message.AppGossipOp,
		message.CrossChainAppRequestOp, message.CrossChainAppRequestFailedOp, message.CrossChainAppResponseOp:
		h.asyncMessageQueue.Push(ctx, msg)
	default:
		h.syncMessageQueue.Push(ctx, msg)
	}
}

func (h *handler) Len() int {
	return h.syncMessageQueue.Len() + h.asyncMessageQueue.Len()
}

func (h *handler) RegisterTimeout(d time.Duration) {
	go func() {
		timer := time.NewTimer(d)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-h.preemptTimeouts:
		}

		// If there is already a timeout ready to fire - just drop the
		// additional timeout. This ensures that all goroutines that are spawned
		// here are able to close if the chain is shutdown.
		select {
		case h.timeouts <- struct{}{}:
		default:
		}
	}()
}

// Note: It is possible for Stop to be called before/concurrently with Start.
func (h *handler) Stop(ctx context.Context) {
	h.closeOnce.Do(func() {
		// Must hold the locks here to ensure there's no race condition in where
		// we check the value of [h.closing] after the call to [Signal].
		h.syncMessageQueue.Shutdown()
		h.asyncMessageQueue.Shutdown()
		close(h.closingChan)

		// TODO: switch this to use a [context.Context] with a cancel function.
		//
		// Don't process any more bootstrap messages. If a dispatcher is
		// processing a bootstrap message, stop. We do this because if we
		// didn't, and the engine was in the middle of executing state
		// transitions during bootstrapping, we wouldn't be able to grab
		// [h.ctx.Lock] until the engine finished executing state transitions,
		// which may take a long time. As a result, the router would time out on
		// shutting down this chain.
		state := h.ctx.State.Get()
		bootstrapper, ok := h.engineManager.Get(state.Type).Get(snow.Bootstrapping)
		if !ok {
			h.ctx.Log.Error("bootstrapping engine doesn't exists",
				zap.Stringer("type", state.Type),
			)
			return
		}
		bootstrapper.Halt(ctx)
	})
}

func (h *handler) StopWithError(ctx context.Context, err error) {
	h.ctx.Log.Fatal("shutting down chain",
		zap.String("reason", "received an unexpected error"),
		zap.Error(err),
	)
	h.Stop(ctx)
}

func (h *handler) Stopped() chan struct{} {
	return h.closed
}

func (h *handler) dispatchSync(ctx context.Context) {
	defer h.closeDispatcher(ctx)

	// Handle sync messages from the router
	for {
		// Get the next message we should process. If the handler is shutting
		// down, we may fail to pop a message.
		ctx, msg, ok := h.popUnexpiredMsg(h.syncMessageQueue, h.metrics.expired)
		if !ok {
			return
		}

		// If there is an error handling the message, shut down the chain
		if err := h.handleSyncMsg(ctx, msg); err != nil {
			h.StopWithError(ctx, fmt.Errorf(
				"%w while processing sync message: %s",
				err,
				msg,
			))
			return
		}
	}
}

func (h *handler) dispatchAsync(ctx context.Context) {
	defer func() {
		h.asyncMessagePool.Shutdown()
		h.closeDispatcher(ctx)
	}()

	// Handle async messages from the router
	for {
		// Get the next message we should process. If the handler is shutting
		// down, we may fail to pop a message.
		ctx, msg, ok := h.popUnexpiredMsg(h.asyncMessageQueue, h.metrics.asyncExpired)
		if !ok {
			return
		}

		h.handleAsyncMsg(ctx, msg)
	}
}

func (h *handler) dispatchChans(ctx context.Context) {
	gossiper := time.NewTicker(h.gossipFrequency)
	defer func() {
		gossiper.Stop()
		h.closeDispatcher(ctx)
	}()

	// Handle messages generated by the handler and the VM
	for {
		var msg message.InboundMessage
		select {
		case <-h.closingChan:
			return

		case vmMSG := <-h.msgFromVMChan:
			msg = message.InternalVMMessage(h.ctx.NodeID, uint32(vmMSG))

		case <-gossiper.C:
			msg = message.InternalGossipRequest(h.ctx.NodeID)

		case <-h.timeouts:
			msg = message.InternalTimeout(h.ctx.NodeID)
		}

		if err := h.handleChanMsg(msg); err != nil {
			h.StopWithError(ctx, fmt.Errorf(
				"%w while processing async message: %s",
				err,
				msg,
			))
			return
		}
	}
}

// Any returned error is treated as fatal
func (h *handler) handleSyncMsg(ctx context.Context, msg Message) error {
	var (
		nodeID    = msg.NodeID()
		op        = msg.Op()
		body      = msg.Message()
		startTime = h.clock.Time()
		// Check if the chain is in normal operation at the start of message
		// execution (may change during execution)
		isNormalOp = h.ctx.State.Get().State == snow.NormalOp
	)
	h.ctx.Log.Debug("forwarding sync message to consensus",
		zap.Stringer("nodeID", nodeID),
		zap.Stringer("messageOp", op),
	)
	h.ctx.Log.Verbo("forwarding sync message to consensus",
		zap.Stringer("nodeID", nodeID),
		zap.Stringer("messageOp", op),
		zap.Any("message", body),
	)
	h.resourceTracker.StartProcessing(nodeID, startTime)
	h.ctx.Lock.Lock()
	lockAcquiredTime := h.clock.Time()
	defer func() {
		h.ctx.Lock.Unlock()

		var (
			endTime           = h.clock.Time()
			messageHistograms = h.metrics.messages[op]
			msgHandlingTime   = lockAcquiredTime.Sub(startTime)
			processingTime    = endTime.Sub(startTime)
		)
		h.resourceTracker.StopProcessing(nodeID, endTime)
		messageHistograms.msgHandlingTime.Observe(float64(msgHandlingTime))
		messageHistograms.processingTime.Observe(float64(processingTime))
		msg.OnFinishedHandling()
		h.ctx.Log.Debug("finished handling sync message",
			zap.Stringer("messageOp", op),
		)
		if processingTime > syncProcessingTimeWarnLimit && isNormalOp {
			h.ctx.Log.Warn("handling sync message took longer than expected",
				zap.Duration("processingTime", processingTime),
				zap.Duration("msgHandlingTime", msgHandlingTime),
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", op),
				zap.Any("message", body),
			)
		}
	}()

	// We will attempt to pass the message to the requested type for the state
	// we are currently in.
	currentState := h.ctx.State.Get()
	if msg.EngineType == p2p.EngineType_ENGINE_TYPE_SNOWMAN &&
		currentState.Type == p2p.EngineType_ENGINE_TYPE_AVALANCHE {
		// The peer is requesting an engine type that hasn't been initialized
		// yet. This means we know that this isn't a response, so we can safely
		// drop the message.
		h.ctx.Log.Debug("dropping sync message",
			zap.String("reason", "uninitialized engine type"),
			zap.Stringer("messageOp", op),
			zap.Stringer("currentEngineType", currentState.Type),
			zap.Stringer("requestedEngineType", msg.EngineType),
		)
		return nil
	}

	var engineType p2p.EngineType
	switch msg.EngineType {
	case p2p.EngineType_ENGINE_TYPE_AVALANCHE, p2p.EngineType_ENGINE_TYPE_SNOWMAN:
		// The peer is requesting an engine type that has been initialized, so
		// we should attempt to honor the request.
		engineType = msg.EngineType
	default:
		// Note: [msg.EngineType] may have been provided by the peer as an
		// invalid option. I.E. not one of AVALANCHE, SNOWMAN, or UNSPECIFIED.
		// In this case, we treat the value the same way as UNSPECIFIED.
		//
		// If the peer didn't request a specific engine type, we default to the
		// current engine.
		engineType = currentState.Type
	}

	engine, ok := h.engineManager.Get(engineType).Get(currentState.State)
	if !ok {
		// This should only happen if the peer is not following the protocol.
		// This can happen if the chain only has a Snowman engine and the peer
		// requested an Avalanche engine handle the message.
		h.ctx.Log.Debug("dropping sync message",
			zap.String("reason", "uninitialized engine state"),
			zap.Stringer("messageOp", op),
			zap.Stringer("currentEngineType", currentState.Type),
			zap.Stringer("requestedEngineType", msg.EngineType),
			zap.Stringer("engineState", currentState.State),
		)
		return nil
	}

	// Invariant: Response messages can never be dropped here. This is because
	//            the timeout has already been cleared. This means the engine
	//            should be invoked with a failure message if parsing of the
	//            response fails.
	switch msg := body.(type) {
	// State messages should always be sent to the snowman engine
	case *p2p.GetStateSummaryFrontier:
		return engine.GetStateSummaryFrontier(ctx, nodeID, msg.RequestId)

	case *p2p.StateSummaryFrontier:
		return engine.StateSummaryFrontier(ctx, nodeID, msg.RequestId, msg.Summary)

	case *message.GetStateSummaryFrontierFailed:
		return engine.GetStateSummaryFrontierFailed(ctx, nodeID, msg.RequestID)

	case *p2p.GetAcceptedStateSummary:
		// TODO: Enforce that the numbers are sorted to make this verification
		//       more efficient.
		if !utils.IsUnique(msg.Heights) {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.GetAcceptedStateSummaryOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "Heights"),
			)
			return engine.GetAcceptedStateSummaryFailed(ctx, nodeID, msg.RequestId)
		}

		return engine.GetAcceptedStateSummary(
			ctx,
			nodeID,
			msg.RequestId,
			msg.Heights,
		)

	case *p2p.AcceptedStateSummary:
		summaryIDs, err := getIDs(msg.SummaryIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.AcceptedStateSummaryOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "SummaryIDs"),
				zap.Error(err),
			)
			return engine.GetAcceptedStateSummaryFailed(ctx, nodeID, msg.RequestId)
		}

		return engine.AcceptedStateSummary(ctx, nodeID, msg.RequestId, summaryIDs)

	case *message.GetAcceptedStateSummaryFailed:
		return engine.GetAcceptedStateSummaryFailed(ctx, nodeID, msg.RequestID)

	// Bootstrapping messages may be forwarded to either avalanche or snowman
	// engines, depending on the EngineType field
	case *p2p.GetAcceptedFrontier:
		return engine.GetAcceptedFrontier(ctx, nodeID, msg.RequestId)

	case *p2p.AcceptedFrontier:
		containerIDs, err := getIDs(msg.ContainerIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.AcceptedFrontierOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerIDs"),
				zap.Error(err),
			)
			return engine.GetAcceptedFrontierFailed(ctx, nodeID, msg.RequestId)
		}

		return engine.AcceptedFrontier(ctx, nodeID, msg.RequestId, containerIDs)

	case *message.GetAcceptedFrontierFailed:
		return engine.GetAcceptedFrontierFailed(ctx, nodeID, msg.RequestID)

	case *p2p.GetAccepted:
		containerIDs, err := getIDs(msg.ContainerIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.GetAcceptedOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerIDs"),
				zap.Error(err),
			)
			return nil
		}

		return engine.GetAccepted(ctx, nodeID, msg.RequestId, containerIDs)

	case *p2p.Accepted:
		containerIDs, err := getIDs(msg.ContainerIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.AcceptedOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerIDs"),
				zap.Error(err),
			)
			return engine.GetAcceptedFailed(ctx, nodeID, msg.RequestId)
		}

		return engine.Accepted(ctx, nodeID, msg.RequestId, containerIDs)

	case *message.GetAcceptedFailed:
		return engine.GetAcceptedFailed(ctx, nodeID, msg.RequestID)

	case *p2p.GetAncestors:
		containerID, err := ids.ToID(msg.ContainerId)
		if err != nil {
			h.ctx.Log.Debug("dropping message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.GetAncestorsOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerID"),
				zap.Error(err),
			)
			return nil
		}

		return engine.GetAncestors(ctx, nodeID, msg.RequestId, containerID)

	case *message.GetAncestorsFailed:
		return engine.GetAncestorsFailed(ctx, nodeID, msg.RequestID)

	case *p2p.Ancestors:
		return engine.Ancestors(ctx, nodeID, msg.RequestId, msg.Containers)

	case *p2p.Get:
		containerID, err := ids.ToID(msg.ContainerId)
		if err != nil {
			h.ctx.Log.Debug("dropping message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.GetOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerID"),
				zap.Error(err),
			)
			return nil
		}

		return engine.Get(ctx, nodeID, msg.RequestId, containerID)

	case *message.GetFailed:
		return engine.GetFailed(ctx, nodeID, msg.RequestID)

	case *p2p.Put:
		return engine.Put(ctx, nodeID, msg.RequestId, msg.Container)

	case *p2p.PushQuery:
		return engine.PushQuery(ctx, nodeID, msg.RequestId, msg.Container)

	case *p2p.PullQuery:
		containerID, err := ids.ToID(msg.ContainerId)
		if err != nil {
			h.ctx.Log.Debug("dropping message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.PullQueryOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "ContainerID"),
				zap.Error(err),
			)
			return nil
		}

		return engine.PullQuery(ctx, nodeID, msg.RequestId, containerID)

	case *p2p.Chits:
		votes, err := getIDs(msg.PreferredContainerIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.ChitsOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "PreferredContainerIDs"),
				zap.Error(err),
			)
			return engine.QueryFailed(ctx, nodeID, msg.RequestId)
		}

		accepted, err := getIDs(msg.AcceptedContainerIds)
		if err != nil {
			h.ctx.Log.Debug("message with invalid field",
				zap.Stringer("nodeID", nodeID),
				zap.Stringer("messageOp", message.ChitsOp),
				zap.Uint32("requestID", msg.RequestId),
				zap.String("field", "AcceptedContainerIDs"),
				zap.Error(err),
			)
			return engine.QueryFailed(ctx, nodeID, msg.RequestId)
		}

		return engine.Chits(ctx, nodeID, msg.RequestId, votes, accepted)

	case *message.QueryFailed:
		return engine.QueryFailed(ctx, nodeID, msg.RequestID)

	// Connection messages can be sent to the currently executing engine
	case *message.Connected:
		return engine.Connected(ctx, nodeID, msg.NodeVersion)

	case *message.ConnectedSubnet:
		return h.subnetConnector.ConnectedSubnet(ctx, nodeID, msg.SubnetID)

	case *message.Disconnected:
		return engine.Disconnected(ctx, nodeID)

	default:
		return fmt.Errorf(
			"attempt to submit unhandled sync msg %s from %s",
			op, nodeID,
		)
	}
}

func (h *handler) handleAsyncMsg(ctx context.Context, msg Message) {
	h.asyncMessagePool.Send(func() {
		if err := h.executeAsyncMsg(ctx, msg); err != nil {
			h.StopWithError(ctx, fmt.Errorf(
				"%w while processing async message: %s",
				err,
				msg,
			))
		}
	})
}

// Any returned error is treated as fatal
func (h *handler) executeAsyncMsg(ctx context.Context, msg Message) error {
	var (
		nodeID    = msg.NodeID()
		op        = msg.Op()
		body      = msg.Message()
		startTime = h.clock.Time()
	)
	h.ctx.Log.Debug("forwarding async message to consensus",
		zap.Stringer("nodeID", nodeID),
		zap.Stringer("messageOp", op),
	)
	h.ctx.Log.Verbo("forwarding async message to consensus",
		zap.Stringer("nodeID", nodeID),
		zap.Stringer("messageOp", op),
		zap.Any("message", body),
	)
	h.resourceTracker.StartProcessing(nodeID, startTime)
	defer func() {
		var (
			endTime           = h.clock.Time()
			messageHistograms = h.metrics.messages[op]
			processingTime    = endTime.Sub(startTime)
		)
		h.resourceTracker.StopProcessing(nodeID, endTime)
		// There is no lock grabbed here, so both metrics are identical
		messageHistograms.processingTime.Observe(float64(processingTime))
		messageHistograms.msgHandlingTime.Observe(float64(processingTime))
		msg.OnFinishedHandling()
		h.ctx.Log.Debug("finished handling async message",
			zap.Stringer("messageOp", op),
		)
	}()

	state := h.ctx.State.Get()
	engine, ok := h.engineManager.Get(state.Type).Get(state.State)
	if !ok {
		return fmt.Errorf(
			"%w %s running %s",
			errMissingEngine,
			state.State,
			state.Type,
		)
	}

	switch m := body.(type) {
	case *p2p.AppRequest:
		return engine.AppRequest(
			ctx,
			nodeID,
			m.RequestId,
			msg.Expiration(),
			m.AppBytes,
		)

	case *p2p.AppResponse:
		return engine.AppResponse(ctx, nodeID, m.RequestId, m.AppBytes)

	case *message.AppRequestFailed:
		return engine.AppRequestFailed(ctx, nodeID, m.RequestID)

	case *p2p.AppGossip:
		return engine.AppGossip(ctx, nodeID, m.AppBytes)

	case *message.CrossChainAppRequest:
		return engine.CrossChainAppRequest(
			ctx,
			m.SourceChainID,
			m.RequestID,
			msg.Expiration(),
			m.Message,
		)

	case *message.CrossChainAppResponse:
		return engine.CrossChainAppResponse(
			ctx,
			m.SourceChainID,
			m.RequestID,
			m.Message,
		)

	case *message.CrossChainAppRequestFailed:
		return engine.CrossChainAppRequestFailed(
			ctx,
			m.SourceChainID,
			m.RequestID,
		)

	default:
		return fmt.Errorf(
			"attempt to submit unhandled async msg %s from %s",
			op, nodeID,
		)
	}
}

// Any returned error is treated as fatal
func (h *handler) handleChanMsg(msg message.InboundMessage) error {
	var (
		op        = msg.Op()
		body      = msg.Message()
		startTime = h.clock.Time()
		// Check if the chain is in normal operation at the start of message
		// execution (may change during execution)
		isNormalOp = h.ctx.State.Get().State == snow.NormalOp
	)
	h.ctx.Log.Debug("forwarding chan message to consensus",
		zap.Stringer("messageOp", op),
	)
	h.ctx.Log.Verbo("forwarding chan message to consensus",
		zap.Stringer("messageOp", op),
		zap.Any("message", body),
	)
	h.ctx.Lock.Lock()
	lockAcquiredTime := h.clock.Time()
	defer func() {
		h.ctx.Lock.Unlock()

		var (
			endTime           = h.clock.Time()
			messageHistograms = h.metrics.messages[op]
			msgHandlingTime   = lockAcquiredTime.Sub(startTime)
			processingTime    = endTime.Sub(startTime)
		)
		messageHistograms.msgHandlingTime.Observe(float64(msgHandlingTime))
		messageHistograms.processingTime.Observe(float64(processingTime))
		msg.OnFinishedHandling()
		h.ctx.Log.Debug("finished handling chan message",
			zap.Stringer("messageOp", op),
		)
		if processingTime > syncProcessingTimeWarnLimit && isNormalOp {
			h.ctx.Log.Warn("handling chan message took longer than expected",
				zap.Duration("processingTime", processingTime),
				zap.Duration("msgHandlingTime", msgHandlingTime),
				zap.Stringer("messageOp", op),
				zap.Any("message", body),
			)
		}
	}()

	state := h.ctx.State.Get()
	engine, ok := h.engineManager.Get(state.Type).Get(state.State)
	if !ok {
		return fmt.Errorf(
			"%w %s running %s",
			errMissingEngine,
			state.State,
			state.Type,
		)
	}

	switch msg := body.(type) {
	case *message.VMMessage:
		return engine.Notify(context.TODO(), common.Message(msg.Notification))

	case *message.GossipRequest:
		// TODO: After Cortina is activated, this can be removed as everyone
		// will have accepted the StopVertex.
		if state.Type == p2p.EngineType_ENGINE_TYPE_SNOWMAN {
			avalancheEngine, ok := h.engineManager.Get(p2p.EngineType_ENGINE_TYPE_AVALANCHE).Get(state.State)
			if ok {
				// This chain was linearized, so we should gossip the Avalanche
				// accepted frontier to make sure everyone eventually linearizes
				// the chain.
				if err := avalancheEngine.Gossip(context.TODO()); err != nil {
					return err
				}
			}
		}
		return engine.Gossip(context.TODO())

	case *message.Timeout:
		return engine.Timeout(context.TODO())

	default:
		return fmt.Errorf(
			"attempt to submit unhandled chan msg %s",
			op,
		)
	}
}

func (h *handler) popUnexpiredMsg(
	queue MessageQueue,
	expired prometheus.Counter,
) (context.Context, Message, bool) {
	for {
		// Get the next message we should process. If the handler is shutting
		// down, we may fail to pop a message.
		ctx, msg, ok := queue.Pop()
		if !ok {
			return nil, Message{}, false
		}

		// If this message's deadline has passed, don't process it.
		if expiration := msg.Expiration(); h.clock.Time().After(expiration) {
			h.ctx.Log.Debug("dropping message",
				zap.String("reason", "timeout"),
				zap.Stringer("nodeID", msg.NodeID()),
				zap.Stringer("messageOp", msg.Op()),
			)
			span := trace.SpanFromContext(ctx)
			span.AddEvent("dropping message", trace.WithAttributes(
				attribute.String("reason", "timeout"),
			))
			expired.Inc()
			msg.OnFinishedHandling()
			continue
		}

		return ctx, msg, true
	}
}

func (h *handler) closeDispatcher(ctx context.Context) {
	h.ctx.Lock.Lock()
	defer h.ctx.Lock.Unlock()

	h.numDispatchersClosed++
	if h.numDispatchersClosed < numDispatchersToClose {
		return
	}

	h.shutdown(ctx)
}

// Note: shutdown is only called after all message dispatchers have exited.
func (h *handler) shutdown(ctx context.Context) {
	defer func() {
		if h.onStopped != nil {
			go h.onStopped()
		}
		close(h.closed)
	}()

	state := h.ctx.State.Get()
	engine, ok := h.engineManager.Get(state.Type).Get(state.State)
	if !ok {
		h.ctx.Log.Error("failed fetching current engine during shutdown",
			zap.Stringer("type", state.Type),
			zap.Stringer("state", state.State),
		)
		return
	}

	if err := engine.Shutdown(ctx); err != nil {
		h.ctx.Log.Error("failed while shutting down the chain",
			zap.Error(err),
		)
	}
}
