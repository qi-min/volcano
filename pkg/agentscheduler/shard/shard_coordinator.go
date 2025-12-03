package shard

import (
	"context"
	"sync"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	shardv1alpha1 "volcano.sh/apis/pkg/client/listers/shard/v1alpha1"
	"volcano.sh/volcano/cmd/agent-scheduler/app/options"
	"volcano.sh/volcano/pkg/scheduler/api"
)

type ShardCoordinator struct {
	schedulerShardName string
	workerStates       map[uint32]*workerNodeShardState
	nodeShards         map[string]*api.NodeShardInfo // node shards for all schedulers
	schedulerNodeShard *api.NodeShardInfo            // node shard for this scheduler
	usedNodeInCache    sets.Set[string]
	mutex              sync.RWMutex
	shardingEnabled    bool
	nodeShardLister    shardv1alpha1.NodeShardLister
	vcClient           vcclientset.Interface
}

type workerNodeShardState struct {
	nodesInUse           sets.Set[string]
	mayHaveNodesToRemove bool // nodes may need be removed from inUsed list, avoid checking sets everytime
	mayHaveNodesToAdd    bool // nodes may need be added into inUsed list, avoid checking sets everytime
}

func NewShardCoordinator(workerCount int, schedulerName string, shardingMode string) *ShardCoordinator {
	klog.V(3).Infof("Shard Coordinator is initialized")

	return &ShardCoordinator{
		schedulerShardName: schedulerName,
		workerStates:       make(map[uint32]*workerNodeShardState, workerCount),
		shardingEnabled:    shardingMode == options.HardShardingMode || shardingMode == options.SoftShardingMode,
	}
}

// GetAndSyncNodesForWorker
func (sm *ShardCoordinator) GetAndSyncNodesForWorker(index uint32) sets.Set[string] {
	nodeToUse := sets.Set[string]{}
	if !sm.shardingEnabled {
		return nodeToUse
	}
	if sm.schedulerNodeShard == nil {
		//No NodeShard is created for scheuler
		return nodeToUse
	}
	tryUpdate := false
	state, exist := sm.workerStates[index]
	if !exist {
		sm.mutex.Lock()
		if len(sm.schedulerNodeShard.NodeInUse) > 0 {
			//use inused nodes as initial state when worker startup
			state = &workerNodeShardState{sm.schedulerNodeShard.NodeInUse, true, true}
			sm.workerStates[index] = state
			klog.V(5).Infof("worker %d: shard in worker is initialzed with inUse nodes in cr", index)
		} else {
			//no inused nodes set before, get current usable nodes
			state = &workerNodeShardState{sm.getUsableNodes(), false, false}
			sm.workerStates[index] = state
			tryUpdate = true
			klog.V(5).Infof("worker %d: no inUser node found in CR, get %d(%d) nodes from desired nodes in cr", index, len(state.nodesInUse), len(sm.schedulerNodeShard.NodeDesired))
		}
		sm.mutex.Unlock()
	}

	sm.mutex.RLock()
	if state.mayHaveNodesToAdd && sm.isNodeAddable() {
		//all disired node can be used
		nodeToUse = sm.schedulerNodeShard.NodeDesired
		state.nodesInUse = nodeToUse
		tryUpdate = true
	} else if state.mayHaveNodesToRemove {
		//move out nodes that not belong to this shard.
		//use usedNodeInCache to check in case nodesInUse is not synchronized between NodeShard CR and local Cache
		nodeToUse = sm.usedNodeInCache.Intersection(sm.schedulerNodeShard.NodeDesired)
		if !nodeToUse.Equal(state.nodesInUse) {
			state.nodesInUse = nodeToUse
			tryUpdate = true
		}
	} else {
		nodeToUse = state.nodesInUse
	}
	state.mayHaveNodesToRemove = false
	state.mayHaveNodesToAdd = false
	sm.mutex.RUnlock()

	if tryUpdate && sm.shouldUpdateInUse() {
		sm.usedNodeInCache = state.nodesInUse
		go sm.updateNodesShardStatus()
	}
	return nodeToUse
}

// RefreshNodeShards update node shards cached in coordinator
func (sm *ShardCoordinator) RefreshNodeShards(nodeShards map[string]*api.NodeShardInfo) {
	if !sm.shardingEnabled {
		return
	}
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	sm.nodeShards = nodeShards
	curentDesiredNodes := sm.schedulerNodeShard.NodeDesired
	shardForSchedulerFound := false
	for shardName, shard := range nodeShards {
		if shardName == sm.schedulerShardName {
			sm.schedulerNodeShard = shard
			shardForSchedulerFound = true
			break
		}
	}
	if !shardForSchedulerFound && sm.shardingEnabled {
		klog.Errorf("sharding is enabled but not shard is set for scheduler")
		sm.schedulerNodeShard = nil
	}
	if !curentDesiredNodes.Equal(sm.schedulerNodeShard.NodeDesired) {
		for _, state := range sm.workerStates {
			//new shard changed desired nodes, nodes used in worker may need to be updated
			state.mayHaveNodesToAdd = true
			state.mayHaveNodesToRemove = true
		}
	}
}

func (sm *ShardCoordinator) updateNodesShardStatus() {
	shardName := sm.schedulerShardName
	klog.V(3).Infof("Update NodeShard %s status...", shardName)
	nodeSahrd, err := sm.nodeShardLister.Get(shardName)
	if err != nil {
		// Check if the error happens because the HyperNode is deleted
		if errors.IsNotFound(err) {
			klog.Infof("NodeShard %s has been deleted, no status update needed", shardName)
			return
		}
		klog.Error(err, "Failed to get NodeShard", "name", shardName)
		return
	}

	nodesInUse := sets.New(nodeSahrd.Status.NodesInUse...)
	if sm.usedNodeInCache.Equal(nodesInUse) {
		return
	}

	// Create a deep copy to avoid modifying cache objects
	nodeShardCopy := nodeSahrd.DeepCopy()
	desiredNodes := sets.New(nodeSahrd.Spec.NodesDesired...)
	nodesToRemove := sm.usedNodeInCache.Difference(desiredNodes)
	nodesToAdd := desiredNodes.Difference(sm.usedNodeInCache)
	nodeShardCopy.Status.NodesInUse = sm.usedNodeInCache.UnsortedList()
	nodeShardCopy.Status.NodesToRemove = nodesToRemove.UnsortedList()
	nodeShardCopy.Status.NodesToAdd = nodesToAdd.UnsortedList()

	_, err = sm.vcClient.ShardV1alpha1().NodeShards().UpdateStatus(context.Background(), nodeShardCopy, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("Failed to update NodeShard %s status %v", shardName, err)
		return
	}
	klog.V(3).InfoS("Updated NodeShard %s status", shardName)
}

// shouldUpdateInUse check whehther shard status need be updated. Retun true if all worker states are the same and NodeInUse different from NodeShardInfo
func (sm *ShardCoordinator) shouldUpdateInUse() bool {
	if len(sm.workerStates) <= 0 {
		return false
	}
	//check whether inuse nodes in all workers are the same
	nodeSet := sm.usedNodeInCache
	for _, state := range sm.workerStates {
		if !nodeSet.Equal(state.nodesInUse) {
			return false
		}
	}
	return !nodeSet.Equal(sm.schedulerNodeShard.NodeInUse)
}

// isNodeAddable check whether newly added nodes in desired nodes can be used. Return true is all other shard do not have nodes to remove
func (sm *ShardCoordinator) isNodeAddable() bool {
	if len(sm.schedulerNodeShard.NodeToAdd) <= 0 {
		return false
	}
	for shardName, nodeShard := range sm.nodeShards {
		if shardName != sm.schedulerShardName {
			if len(nodeShard.NodeToRemove) > 0 {
				//other schedulers haven't moven nodes out
				return false
			}
		}
	}
	return true
}

// getUsableNodes get usable nodes based on desired nodes
func (sm *ShardCoordinator) getUsableNodes() sets.Set[string] {
	nodes := sm.schedulerNodeShard.NodeDesired
	for shardName, nodeShard := range sm.nodeShards {
		if shardName != sm.schedulerShardName {
			nodes = nodes.Difference(nodeShard.NodeInUse)
		}
	}
	return nodes
}
