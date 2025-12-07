/*
Copyright 2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"sync"
	"sync/atomic"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/sets"
	"volcano.sh/volcano/cmd/agent-scheduler/app/options"
	"volcano.sh/volcano/pkg/scheduler/api"
)

type ShardCoordinator struct {
	schedulerShardName     string
	workerStates           []*workerNodeShardState
	nodeShardInfos         map[string]*api.NodeShardInfo // node shards for all schedulers
	schedulerNodeShardInfo *api.NodeShardInfo            // node shard for this scheduler
	nodeToUse              sets.Set[string]              // nodes should be used by workers
	mutex                  sync.RWMutex
	shardingEnabled        bool
	lastSynced             int64 //last synced revision to nodeshard cr
	latestRevision         int64 //last revision of nodeshard cr change
	cache                  Cache
}

type workerNodeShardState struct {
	revision                    int64
	schedulingWithUnsyncedNodes bool
}

func NewShardCoordinator(cache Cache, workerCount int, schedulerName string, shardingMode string) *ShardCoordinator {
	klog.V(3).Infof("Shard Coordinator is initialized")

	return &ShardCoordinator{
		schedulerShardName: schedulerName,
		workerStates:       make([]*workerNodeShardState, workerCount),
		shardingEnabled:    shardingMode == options.HardShardingMode || shardingMode == options.SoftShardingMode,
		cache:              cache,
	}
}

// GetNodesForWorker get nodes can be involved in worker
func (sm *ShardCoordinator) GetNodesForWorker(index int) sets.Set[string] {
	klog.V(5).Infof("Worker %d will schedule with nodes%v", index, sm.nodeToUse.UnsortedList())
	if index > len(sm.workerStates) {
		klog.Errorf("Worker %d does not exist, no nodes are returned", index)
		return sets.Set[string]{}
	}
	state := sm.workerStates[index]
	if state == nil {
		sm.workerStates[index] = &workerNodeShardState{revision: sm.latestRevision}
	} else {
		state.revision = sm.latestRevision
	}
	return sm.nodeToUse
}

// RefreshNodeShards update node shards cached in coordinator
func (sm *ShardCoordinator) RefreshNodeShards(nodeShards map[string]*api.NodeShardInfo) {
	if !sm.shardingEnabled {
		return
	}

	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	sm.nodeShardInfos = nodeShards
	shardForSchedulerFound := false
	for shardName, shard := range nodeShards {
		if shardName == sm.schedulerShardName {
			sm.schedulerNodeShardInfo = shard
			shardForSchedulerFound = true
			break
		}
	}
	if !shardForSchedulerFound && sm.shardingEnabled {
		klog.Errorf("Sharding is enabled but not shard is defined for this scheduler!")
		sm.schedulerNodeShardInfo = nil
		return
	}

	if usableNodes := sm.getUsableNodes(); !sm.nodeToUse.Equal(usableNodes) {
		atomic.AddInt64(&sm.latestRevision, 1)
		sm.nodeToUse = usableNodes
		klog.V(3).Infof("Try to update nodeshard status after nodeshart refresh")
		sm.tryUpdateNodeShardStatus()
	}
}

func (sm *ShardCoordinator) tryUpdateNodeShardStatus() {
	latest := atomic.LoadInt64(&sm.latestRevision)
	//skip upate if status has been updated
	if atomic.LoadInt64(&sm.lastSynced) >= latest {
		return
	}

	for index, state := range sm.workerStates {
		if state == nil {
			state = &workerNodeShardState{}
			sm.workerStates[index] = state
		}
		//skip update if any worker is scheduling with nodes before this revision
		if state.schedulingWithUnsyncedNodes && state.revision < latest {
			klog.V(3).Infof("Worker %d is scheduling with old nodes, skip nodeshard update", index)
			return
		}
	}

	atomic.StoreInt64(&sm.lastSynced, latest)
	sm.cache.UpdateNodesShardStatus(sm.schedulerShardName, sm.nodeToUse)
}

func (sm *ShardCoordinator) OnWorkerStartSchedulingCycle(index int) {
	if index > len(sm.workerStates) {
		klog.Errorf("Worker %d does not exist", index)
		return
	}

	latest := atomic.LoadInt64(&sm.latestRevision)
	state := sm.workerStates[index]
	if state == nil {
		sm.mutex.Lock()
		sm.workerStates[index] = &workerNodeShardState{
			revision: latest,
		}
		sm.mutex.Unlock()
		return
	}
	//worker has pickup nodes in latest revision, avoid acquiring lock when no revision changed
	if state.revision == latest {
		return
	}
	//worker start to use nodes in latest revision
	sm.mutex.Lock()
	state.schedulingWithUnsyncedNodes = true
	sm.mutex.Unlock()
}

func (sm *ShardCoordinator) OnWorkerEndSchedulingCycle(index int) {
	if index > len(sm.workerStates) {
		klog.Errorf("Worker %d does not exist", index)
		return
	}
	latest := atomic.LoadInt64(&sm.latestRevision)
	state := sm.workerStates[index]
	if state == nil {
		klog.Errorf("Worker %d state was not initialized before", index)
		return
	}
	//worker has pickup nodes in latest revision, avoid acquiring lock when no revision changed
	if state.revision == latest {
		return
	}

	// worker used nodes in old revision, try to update nodeshard status after worker end
	// because worker in next schedule must pick new nodes.
	sm.mutex.Lock()
	state.schedulingWithUnsyncedNodes = false
	klog.V(3).Infof("Try to update nodeshard status after worker %d end scheduling cycle", index)
	sm.tryUpdateNodeShardStatus()
	sm.mutex.Unlock()
}

// getUsableNodes get usable nodes based on desired nodes
func (sm *ShardCoordinator) getUsableNodes() sets.Set[string] {
	nodes := sm.schedulerNodeShardInfo.NodeDesired
	for shardName, nodeShard := range sm.nodeShardInfos {
		if shardName != sm.schedulerShardName {
			nodes = nodes.Difference(nodeShard.NodeInUse)
		}
	}
	return nodes
}
