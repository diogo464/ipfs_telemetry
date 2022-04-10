package monitor

import (
	"context"
	"sync"

	pb "git.d464.sh/adc/telemetry/pkg/proto/monitor"
	"git.d464.sh/adc/telemetry/pkg/telemetry"
	"git.d464.sh/adc/telemetry/pkg/waker"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"go.uber.org/zap"
)

const (
	ActionDiscover = iota
	ActionTelemetry
	ActionBandwidth
	ActionRemovePeer
)

type action struct {
	kind int
	pid  peer.ID
}

type peerState struct {
	mu            sync.Mutex
	id            peer.ID
	failedAttemps int
	lastSession   telemetry.Session
	lastSeqN      uint64
}

type Monitor struct {
	pb.UnimplementedMonitorServer
	h        host.Host
	opts     *options
	actions  *waker.Waker[action]
	peers    map[peer.ID]*peerState
	exporter Exporter
}

func NewMonitor(ctx context.Context, exporter Exporter, o ...Option) (*Monitor, error) {
	opts := defaults()
	if err := apply(opts, o...); err != nil {
		return nil, err
	}

	if opts.Host == nil {
		h, err := createDefaultHost(ctx)
		if err != nil {
			return nil, err
		}
		opts.Host = h
	}

	return &Monitor{
		h:        opts.Host,
		opts:     opts,
		actions:  waker.NewWaker[action](),
		peers:    make(map[peer.ID]*peerState),
		exporter: exporter,
	}, nil
}

func (s *Monitor) Close() {
	s.actions.Close()
	s.h.Close()
}

func (s *Monitor) PeerDiscovered(p peer.ID) {
	s.actions.PushNow(&action{
		kind: ActionDiscover,
		pid:  p,
	})
}

func (s *Monitor) Run(ctx context.Context) {
LOOP:
	for {
		select {
		case action := <-s.actions.Receive():
			switch action.kind {
			case ActionDiscover:
				zap.S().Debugw("discover", "peer", action.pid)
				s.onActionDiscover(action.pid)
			case ActionTelemetry:
				zap.S().Debugw("telemetry", "peer", action.pid)
				s.onActionTelemetry(action.pid)
			case ActionBandwidth:
				zap.S().Debugw("bandwidth", "peer", action.pid)
				s.onActionBandwidth(action.pid)
			case ActionRemovePeer:
				zap.S().Debugw("removing", "peer", action.pid)
				delete(s.peers, action.pid)
			}
		case <-ctx.Done():
			break LOOP
		}
	}
}

func (s *Monitor) onActionDiscover(p peer.ID) {
	if err := s.setupPeer(p); err == nil {
		s.onActionTelemetry(p)
		s.onActionBandwidth(p)
	}
}

func (s *Monitor) onActionTelemetry(p peer.ID) {
	// if !ok then the peer was removed but the action was still queued
	if state, ok := s.peers[p]; ok {
		go s.collectTelemetry(state)
	}
}

func (s *Monitor) onActionBandwidth(p peer.ID) {
	if state, ok := s.peers[p]; ok {
		go s.collectBandwidth(state)
	}
}

func (s *Monitor) setupPeer(p peer.ID) error {
	if _, ok := s.peers[p]; ok {
		return nil
	}
	s.peers[p] = &peerState{
		mu:            sync.Mutex{},
		id:            p,
		failedAttemps: 0,
		lastSession:   telemetry.InvalidSession,
		lastSeqN:      0,
	}
	return nil
}

func (s *Monitor) collectTelemetry(state *peerState) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if err := s.tryCollectTelemetry(state); err == nil {
		s.actions.Push(&action{
			kind: ActionTelemetry,
			pid:  state.id,
		}, s.opts.CollectPeriod)
	} else {
		s.peerError(state, err)
	}
}

func (s *Monitor) tryCollectTelemetry(state *peerState) error {
	ctx := context.Background()

	zap.S().Debug("creating client")
	client, err := telemetry.NewClient(s.h, state.id)
	if err != nil {
		return err
	}
	defer client.Close()

	zap.S().Debug("getting session info")
	session, err := client.SessionInfo(ctx)
	if err != nil {
		return err
	}

	since := state.lastSeqN
	if session.Session != state.lastSession {
		since = 0
		state.lastSession = session.Session
	}

	zap.S().Debug("streaming snapshots")
	stream := make(chan telemetry.SnapshotStreamItem)
	go func() {
		for item := range stream {
			state.lastSeqN = item.NextSeqN
			zap.S().Debug("exporting ", len(item.Snapshots), " snapshots")
			s.exporter.ExportSnapshots(state.id, session.Session, item.Snapshots)
		}
	}()

	err = client.Snapshots(context.Background(), since, stream)
	if err != nil {
		return err
	}

	return nil
}

func (s *Monitor) collectBandwidth(state *peerState) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if err := s.tryCollectBandwidth(state); err == nil {
		s.actions.Push(&action{
			kind: ActionBandwidth,
			pid:  state.id,
		}, s.opts.BandwidthPeriod)
	} else {
		s.peerError(state, err)
	}
}

func (s *Monitor) tryCollectBandwidth(state *peerState) error {
	ctx := context.Background()
	client, err := telemetry.NewClient(s.h, state.id)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.SessionInfo(ctx)
	if err != nil {
		return err
	}

	bandwidth, err := client.Bandwidth(ctx, telemetry.DEFAULT_PAYLOAD_SIZE)
	if err != nil {
		return err
	}

	s.exporter.ExportBandwidth(state.id, session.Session, bandwidth)

	return nil
}

// must be holding the state's lock
func (s *Monitor) peerError(state *peerState, err error) {
	zap.S().Error("peer error", err, state.id)
	state.failedAttemps += 1
	if state.failedAttemps > s.opts.MaxFailedAttemps {
		s.actions.PushNow(&action{
			kind: ActionRemovePeer,
			pid:  state.id,
		})
	}
}
