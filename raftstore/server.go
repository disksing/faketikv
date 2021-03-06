// Copyright 2019-present PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"sync"
	"time"

	"github.com/ngaut/unistore/config"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/store/mockstore/unistore/pd"
	"go.uber.org/zap"
)

// RaftInnerServer implements the tikv.InnerServer interface.
type RaftInnerServer struct {
	engines       *Engines
	raftConfig    *Config
	globalConfig  *config.Config
	storeMeta     metapb.Store
	eventObserver PeerEventObserver

	node        *Node
	snapManager *SnapManager
	router      *router
	batchSystem *raftBatchSystem
	pdWorker    *worker
	snapWorker  *worker
	lsDumper    *lockStoreDumper
	raftCli     *RaftClient
}

// Raft implements the tikv.InnerServer Raft method.
func (ris *RaftInnerServer) Raft(stream tikvpb.Tikv_RaftServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := ris.router.sendRaftMessage(msg); err != nil {
			log.S().Error(err)
		}
	}
}

// BatchRaft implements the tikv.InnerServer BatchRaft method.
func (ris *RaftInnerServer) BatchRaft(stream tikvpb.Tikv_BatchRaftServer) error {
	for {
		msgs, err := stream.Recv()
		if err != nil {
			return err
		}
		for _, msg := range msgs.GetMsgs() {
			if err := ris.router.sendRaftMessage(msg); err != nil {
				log.S().Error(err)
			}
		}
	}
}

// Snapshot implements the tikv.InnerServer Snapshot method.
func (ris *RaftInnerServer) Snapshot(stream tikvpb.Tikv_SnapshotServer) error {
	var err error
	done := make(chan struct{})
	ris.snapWorker.sender <- task{
		tp: taskTypeSnapRecv,
		data: recvSnapTask{
			stream: stream,
			callback: func(e error) {
				err = e
				close(done)
			},
		},
	}
	<-done
	return err
}

// NewRaftInnerServer returns a new RaftInnerServer.
func NewRaftInnerServer(globalConfig *config.Config, engines *Engines, raftConfig *Config) *RaftInnerServer {
	return &RaftInnerServer{
		engines:      engines,
		raftConfig:   raftConfig,
		globalConfig: globalConfig,
	}
}

// Setup implements the tikv.InnerServer Setup method.
func (ris *RaftInnerServer) Setup(pdClient pd.Client) {
	var wg sync.WaitGroup
	ris.pdWorker = newWorker("pd-worker", &wg)
	ris.snapWorker = newWorker("snap-worker", &wg)

	// TODO: create local reader
	// TODO: create storage read pool
	// TODO: create cop read pool
	// TODO: create cop endpoint

	cfg := ris.raftConfig
	router, batchSystem := createRaftBatchSystem(ris.globalConfig, cfg)

	ris.router = router // TODO: init with local reader
	ris.snapManager = NewSnapManager(cfg.SnapPath, router)
	ris.batchSystem = batchSystem
	ris.lsDumper = &lockStoreDumper{
		stopCh:      make(chan struct{}),
		engines:     ris.engines,
		fileNumDiff: 2,
	}
}

// GetRaftstoreRouter gets the raftstore Router.
func (ris *RaftInnerServer) GetRaftstoreRouter() *Router {
	return &Router{router: ris.router}
}

// GetStoreMeta gets the store meta of the RaftInnerServer.
func (ris *RaftInnerServer) GetStoreMeta() *metapb.Store {
	return &ris.storeMeta
}

// SetPeerEventObserver sets the peer event observer.
func (ris *RaftInnerServer) SetPeerEventObserver(ob PeerEventObserver) {
	ris.eventObserver = ob
}

// Start implements the tikv.InnerServer Start method.
func (ris *RaftInnerServer) Start(pdClient pd.Client) error {
	ris.node = NewNode(ris.batchSystem, &ris.storeMeta, ris.raftConfig, pdClient, ris.eventObserver)

	raftClient := newRaftClient(ris.raftConfig, pdClient)
	trans := NewServerTransport(raftClient, ris.snapWorker.sender, ris.router)
	err := ris.node.Start(context.TODO(), ris.engines, trans, ris.snapManager, ris.pdWorker, ris.router)
	if err != nil {
		return err
	}
	ris.raftCli = raftClient
	snapRunner := newSnapRunner(ris.snapManager, ris.raftConfig, ris.router, pdClient)
	ris.snapWorker.start(snapRunner)
	go ris.lsDumper.run()
	return nil
}

// Stop implements the tikv.InnerServer Stop method.
func (ris *RaftInnerServer) Stop() error {
	ris.snapWorker.stop()
	ris.node.stop()
	ris.raftCli.Stop()
	if err := ris.engines.raft.Close(); err != nil {
		return err
	}
	return ris.engines.kv.DB.Close()
}

// LockstoreFileName defines the lockstore file name.
const LockstoreFileName = "lockstore.dump"

type lockStoreDumper struct {
	stopCh      chan struct{}
	engines     *Engines
	fileNumDiff uint64
}

func (dumper *lockStoreDumper) run() {
	ticker := time.NewTicker(time.Second * 10)
	lastFileNum := dumper.engines.raft.GetVLogOffset() >> 32
	for {
		select {
		case <-ticker.C:
			vlogOffset := dumper.engines.raft.GetVLogOffset()
			currentFileNum := vlogOffset >> 32
			if currentFileNum-lastFileNum >= dumper.fileNumDiff {
				meta := make([]byte, 8)
				binary.LittleEndian.PutUint64(meta, vlogOffset)
				// Waiting for the raft log to be applied.
				// TODO: it is possible that some log is not applied after sleep, find a better way to make sure this.
				time.Sleep(5 * time.Second)
				err := dumper.engines.kv.LockStore.DumpToFile(filepath.Join(dumper.engines.kvPath, LockstoreFileName), meta)
				if err != nil {
					log.Error("dump lock store failed", zap.Error(err))
					continue
				}
				lastFileNum = currentFileNum
			}
		case <-dumper.stopCh:
			return
		}
	}
}
