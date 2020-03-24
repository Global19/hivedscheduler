// MIT License
//
// Copyright (c) Microsoft Corporation. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE

package algorithm

import (
	"fmt"
	"github.com/microsoft/hivedscheduler/pkg/api"
	"github.com/microsoft/hivedscheduler/pkg/common"
	"github.com/microsoft/hivedscheduler/pkg/internal"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"sync"
)

// HivedAlgorithm implements an internal.SchedulerAlgorithm. It schedules affinity groups using the algorithm of HiveD.
// Note that the topologyAwareScheduler used in this struct is not another implementation of SchedulerAlgorithm;
// that is a specific algorithm for pod placement, used in intra-VC scheduling and opportunistic pod scheduling.
type HivedAlgorithm struct {
	// scheduler in each VC
	vcSchedulers map[api.VirtualClusterName]intraVCScheduler
	// scheduler for opportunistic pods
	opportunisticSchedulers map[CellChain]*topologyAwareScheduler
	// ChainCellLists of physical cells of each cell chain (including the children of the free cells)
	fullCellList map[CellChain]ChainCellList
	// ChainCellLists of free physical cells of each cell chain (used in buddy alloc)
	freeCellList map[CellChain]ChainCellList
	// all affinity groups that have been allocated cells
	allocatedAffinityGroups map[string]*AlgoAffinityGroup
	// all affinity groups that are preempting other groups
	preemptorAffinityGroups map[string]*AlgoAffinityGroup

	// vcFreeCellNum, allVCFreeCellNum, and totalLeftCellNum are used to track cell usage of the VCs.
	// Note that these numbers count both healthy and bad cells.

	// number of free preassigned cells of each VC of each cell type
	vcFreeCellNum map[api.VirtualClusterName]map[CellChain]map[CellLevel]int32
	// total number of preassigned free cells in all the VCs of each cell type
	allVCFreeCellNum map[CellChain]map[CellLevel]int32
	// number of cells left in the physical cluster of each cell type
	// (i.e., the cell is either in the free list or could be obtained by splitting such a free cell.)
	totalLeftCellNum map[CellChain]map[CellLevel]int32

	// number of bad free cells in the physical cluster of each cell type
	badFreeCellNum map[CellChain]map[CellLevel]int32
	// number of free cells in each VC that are doomed to be bad (i.e., when the healthy
	// free cells in the physical cluster is fewer that the VC's free cells)
	vcDoomedBadCellNum map[api.VirtualClusterName]map[CellChain]map[CellLevel]int32
	// bad nodes in the physical cluster
	badNodes common.Set
	// map each GPU type to all chains that contain this type
	chains map[string][]CellChain
	// map each level in a chain to the specific cell type name
	cellTypes map[CellChain]map[CellLevel]api.CellType
	// cluster status exposed to external
	apiClusterStatus api.ClusterStatus
	// lock
	algorithmLock sync.RWMutex
}

// NewHivedAlgorithm initializes a HivedAlgorithm from the config file.
func NewHivedAlgorithm(sConfig *api.Config) *HivedAlgorithm {
	fullPcl, freePcl, vcFreeCellNum, nonReservedFullVcl, nonReservedFreeVcl, reservedVcl, reservedPc,
		gpuNums, gpuTypeToChain, cellLevelToType := ParseConfig(sConfig)

	h := &HivedAlgorithm{
		vcSchedulers:            map[api.VirtualClusterName]intraVCScheduler{},
		opportunisticSchedulers: map[CellChain]*topologyAwareScheduler{},
		fullCellList:            fullPcl,
		freeCellList:            freePcl,
		vcFreeCellNum:           vcFreeCellNum,
		allVCFreeCellNum:        map[CellChain]map[CellLevel]int32{},
		totalLeftCellNum:        map[CellChain]map[CellLevel]int32{},
		badFreeCellNum:          map[CellChain]map[CellLevel]int32{},
		vcDoomedBadCellNum:      map[api.VirtualClusterName]map[CellChain]map[CellLevel]int32{},
		badNodes:                common.NewSet(),
		chains:                  gpuTypeToChain,
		cellTypes:               cellLevelToType,
		allocatedAffinityGroups: map[string]*AlgoAffinityGroup{},
		preemptorAffinityGroups: map[string]*AlgoAffinityGroup{},
		apiClusterStatus: api.ClusterStatus{
			PhysicalCluster: api.PhysicalClusterStatus{},
			VirtualClusters: map[api.VirtualClusterName]api.VirtualClusterStatus{},
		},
	}
	for vcName := range nonReservedFullVcl {
		// TODO: Support per-VC configurable intra VC scheduling algo.
		h.vcSchedulers[vcName] = newDefaultIntraVCScheduler(
			nonReservedFullVcl[vcName], nonReservedFreeVcl[vcName], reservedVcl[vcName], gpuNums)
	}
	for chain, ccl := range h.fullCellList {
		h.opportunisticSchedulers[chain] = NewTopologyAwareScheduler(ccl, gpuNums[chain], false, true)
	}
	h.initCellNums()
	h.initAPIClusterStatus()
	h.initReservations(reservedPc)
	h.initBadNodes()
	return h
}

func (h *HivedAlgorithm) AddNode(node *core.Node) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	if !internal.IsNodeHealthy(node) {
		// adding a bad node
		h.setBadNode(node.Name)
	} else {
		// possibly a bad node comes back again
		h.setHealthyNode(node.Name)
	}
}

func (h *HivedAlgorithm) UpdateNode(oldNode, newNode *core.Node) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	if oldHealthy := internal.IsNodeHealthy(oldNode); oldHealthy != internal.IsNodeHealthy(newNode) {
		if oldHealthy {
			h.setBadNode(newNode.Name)
		} else {
			h.setHealthyNode(newNode.Name)
		}
	}
}

func (h *HivedAlgorithm) DeleteNode(node *core.Node) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	h.setBadNode(node.Name)
}

func (h *HivedAlgorithm) Schedule(pod *core.Pod, suggestedNodes []string) internal.PodScheduleResult {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	klog.Infof("[%v]: Scheduling pod...", internal.Key(pod))
	s := internal.ExtractPodSchedulingSpec(pod)
	suggestedNodeSet := common.NewSet()
	for _, n := range suggestedNodes {
		suggestedNodeSet.Add(n)
	}
	var (
		groupPhysicalPlacement groupPhysicalPlacement // GPU number -> a set of pods -> a set of GPUs of each pod
		groupVirtualPlacement  groupVirtualPlacement  // GPU number -> a set of pods -> a set of GPUs of each pod
		preemptionVictims      map[string]common.Set  // node -> pods
		podIndex               int32                  // index of among the pods with the same GPU number in the group, 0 by default
	)

	if g := h.allocatedAffinityGroups[s.AffinityGroup.Name]; g != nil {
		klog.Infof("[%v]: Pod from allocated affinity group: %v", internal.Key(pod), s.AffinityGroup.Name)
		groupPhysicalPlacement = g.physicalGpuPlacement
		groupVirtualPlacement = g.virtualGpuPlacement
		if podIndex = getNewPodIndex(g.allocatedPods[s.GpuNumber]); podIndex == -1 {
			panic(internal.NewBadRequestError(fmt.Sprintf(
				"Requesting more pods than the configured number for %v GPUs (%v pods) in affinity group %v",
				s.GpuNumber, g.totalPodNums[s.GpuNumber], s.AffinityGroup.Name)))
		}
	} else if g = h.preemptorAffinityGroups[s.AffinityGroup.Name]; g != nil {
		klog.Infof("[%v]: Pod from preemptor affinity group: %v", internal.Key(pod), s.AffinityGroup.Name)
		groupPhysicalPlacement = g.physicalGpuPlacement
		groupVirtualPlacement = g.virtualGpuPlacement
		if preemptionVictims = collectPreemptionVictims(groupPhysicalPlacement); len(preemptionVictims) == 0 {
			klog.Infof("Preemption victims have been cleaned up for preemptor affinity group %v", g.name)
		}
		g.preemptorPods[internal.Key(pod)] = pod
	} else {
		klog.Infof("[%v]: Scheduling new affinity group %v", internal.Key(pod), s.AffinityGroup.Name)
		groupPhysicalPlacement, groupVirtualPlacement = h.scheduleNewAffinityGroup(pod, s, suggestedNodeSet)
		if preemptionVictims = collectPreemptionVictims(groupPhysicalPlacement); len(preemptionVictims) != 0 {
			h.createPreemptorAffinityGroup(s, groupPhysicalPlacement, groupVirtualPlacement, pod)
		}
	}

	return generatePodScheduleResult(
		groupPhysicalPlacement,
		groupVirtualPlacement,
		CellPriority(s.Priority),
		preemptionVictims,
		h.cellTypes,
		s.GpuNumber,
		podIndex,
		h.allocatedAffinityGroups[s.AffinityGroup.Name],
		s.AffinityGroup.Name,
		suggestedNodeSet,
		s.VirtualCluster,
		pod)
}

func (h *HivedAlgorithm) AddUnallocatedPod(pod *core.Pod) {
}

func (h *HivedAlgorithm) DeleteUnallocatedPod(pod *core.Pod) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	s := internal.ExtractPodSchedulingSpec(pod)
	if g := h.preemptorAffinityGroups[s.AffinityGroup.Name]; g != nil {
		if podKey := internal.Key(pod); g.preemptorPods[podKey] != nil {
			klog.Infof("[%v]: Deleting preemptor pod from affinity group %v...", podKey, g.name)
			delete(g.preemptorPods, podKey)
		}
		if len(g.preemptorPods) == 0 {
			h.deletePreemptorAffinityGroup(g, "")
		}
	}
}

func (h *HivedAlgorithm) AddAllocatedPod(pod *core.Pod) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	s := internal.ExtractPodSchedulingSpec(pod)
	info := internal.ExtractPodBindInfo(pod)
	klog.Infof("[%v]: Adding allocated pod to affinity group %v...", internal.Key(pod), s.AffinityGroup.Name)
	klog.Infof("[%v]: Adding to node %v, GPUs %v", internal.Key(pod), info.Node, common.ToJson(info.GpuIsolation))

	if g := h.preemptorAffinityGroups[s.AffinityGroup.Name]; g != nil {
		h.preemptorAffinityGroupToAllocated(g, pod)
	}
	podIndex := int32(0)
	if g := h.allocatedAffinityGroups[s.AffinityGroup.Name]; g == nil {
		h.createAllocatedAffinityGroup(s, info, pod)
	} else {
		if podIndex = getAllocatedPodIndex(info, s.GpuNumber); podIndex == -1 {
			klog.Errorf("[%v]: Pod placement not found in group %v: node %v, GPUs %v",
				internal.Key(pod), s.AffinityGroup.Name, info.Node, info.GpuIsolation)
			return
		}
	}
	h.allocatedAffinityGroups[s.AffinityGroup.Name].allocatedPods[s.GpuNumber][podIndex] = pod
}

func (h *HivedAlgorithm) DeleteAllocatedPod(pod *core.Pod) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	s := internal.ExtractPodSchedulingSpec(pod)
	info := internal.ExtractPodBindInfo(pod)
	klog.Infof("[%v]: Deleting allocated pod from affinity group %v...", internal.Key(pod), s.AffinityGroup.Name)
	klog.Infof("[%v]: Deleting from node %v, GPUs %v", internal.Key(pod), info.Node, common.ToJson(info.GpuIsolation))

	if g := h.allocatedAffinityGroups[s.AffinityGroup.Name]; g == nil {
		klog.Errorf("[%v]: Group %v not found when deleting pod", internal.Key(pod), s.AffinityGroup.Name)
		return
	} else {
		if podIndex := getAllocatedPodIndex(info, s.GpuNumber); podIndex == -1 {
			klog.Errorf("[%v]: Pod placement not found in group %v: node %v, GPUs %v",
				internal.Key(pod), s.AffinityGroup.Name, info.Node, info.GpuIsolation)
			return
		} else {
			g.allocatedPods[s.GpuNumber][podIndex] = nil
		}
		if allPodsReleased(g.allocatedPods) {
			h.deleteAllocatedAffinityGroup(g, pod)
		}
	}
}

func (h *HivedAlgorithm) GetAllAffinityGroups() api.AffinityGroupList {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	ags := api.AffinityGroupList{}
	for _, aag := range h.allocatedAffinityGroups {
		ags.Items = append(ags.Items, aag.ToAffinityGroup())
	}

	return ags
}

func (h *HivedAlgorithm) GetAffinityGroup(name string) api.AffinityGroup {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	if aag := h.allocatedAffinityGroups[name]; aag != nil {
		return aag.ToAffinityGroup()
	}

	panic(internal.NewBadRequestError(fmt.Sprintf(
		"Affinity group %v does not exist since it is not allocated",
		name)))
}

func (h *HivedAlgorithm) GetClusterStatus() api.ClusterStatus {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	s := api.ClusterStatus{
		PhysicalCluster: h.apiClusterStatus.PhysicalCluster.DeepCopy(),
		VirtualClusters: map[api.VirtualClusterName]api.VirtualClusterStatus{},
	}
	for vcn, vcs := range h.apiClusterStatus.VirtualClusters {
		s.VirtualClusters[vcn] = vcs.DeepCopy()
	}
	return s
}

func (h *HivedAlgorithm) GetPhysicalClusterStatus() api.PhysicalClusterStatus {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	return h.apiClusterStatus.PhysicalCluster.DeepCopy()
}

func (h *HivedAlgorithm) GetAllVirtualClustersStatus() map[api.VirtualClusterName]api.VirtualClusterStatus {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	allVcs := map[api.VirtualClusterName]api.VirtualClusterStatus{}
	for vcn, vcs := range h.apiClusterStatus.VirtualClusters {
		allVcs[vcn] = vcs.DeepCopy()
	}
	return allVcs
}

func (h *HivedAlgorithm) GetVirtualClusterStatus(vcn api.VirtualClusterName) api.VirtualClusterStatus {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	return h.apiClusterStatus.VirtualClusters[vcn].DeepCopy()
}

// initCellNums initiates the data structures for tracking cell usages and healthiness,
// i.e., h.allVCFreeCellNum, h.totalLeftCellNum, h.badFreeCellNum, and h.vcDoomedBadCellNum.
// This method also validates the initial cell assignment to the VCs to make sure that
// all the assigned cells can be fit into the configured physical cells.
func (h *HivedAlgorithm) initCellNums() {
	for vc, vcFreeCellNum := range h.vcFreeCellNum {
		h.vcDoomedBadCellNum[vc] = map[CellChain]map[CellLevel]int32{}
		for chain, chainFreeCellNum := range vcFreeCellNum {
			h.vcDoomedBadCellNum[vc][chain] = map[CellLevel]int32{}
			if h.allVCFreeCellNum[chain] == nil {
				h.allVCFreeCellNum[chain] = map[CellLevel]int32{}
			}
			for level, levelFreeCellNum := range chainFreeCellNum {
				h.allVCFreeCellNum[chain][level] += levelFreeCellNum
			}
		}
	}
	for chain, chainFreeCellNum := range h.allVCFreeCellNum {
		if ccl := h.fullCellList[chain]; ccl == nil {
			panic(fmt.Sprintf(
				"Illegal initial VC assignment: Chain %v does not exists in physical cluster", chain))
		} else {
			top := CellLevel(len(ccl))
			available := int32(len(ccl[top]))
			h.totalLeftCellNum[chain] = map[CellLevel]int32{}
			h.badFreeCellNum[chain] = map[CellLevel]int32{}
			h.totalLeftCellNum[chain][top] = available
			h.badFreeCellNum[chain][top] = 0
			for l := top; l >= lowestLevel; l-- {
				left := available - chainFreeCellNum[l]
				if left < 0 {
					panic(fmt.Sprintf(
						"Illegal initial VC assignment: "+
							"Insufficient physical cells at chain %v level %v: %v needed, %v available",
						chain, l, chainFreeCellNum[l], available))
				}
				if l > lowestLevel {
					childNum := int32(len(ccl[l][0].GetChildren()))
					available = left * childNum
					h.totalLeftCellNum[chain][l-1] = h.totalLeftCellNum[chain][l] * childNum
					h.badFreeCellNum[chain][l-1] = 0
				}
			}
		}
	}
}

// initAPIClusterStatus initiates the status of the physical cluster and the VCs that will be exposed to users.
func (h *HivedAlgorithm) initAPIClusterStatus() {
	for _, ccl := range h.fullCellList {
		for _, c := range ccl[CellLevel(len(ccl))] {
			h.apiClusterStatus.PhysicalCluster = append(h.apiClusterStatus.PhysicalCluster, c.(*PhysicalCell).GetAPIStatus())
		}
	}
	for vc, vcs := range h.vcSchedulers {
		h.apiClusterStatus.VirtualClusters[vc] = []*api.VirtualCellStatus{}
		for _, ccl := range vcs.getNonReservedFreeCellList() {
			for _, cl := range ccl {
				for _, c := range cl {
					h.apiClusterStatus.VirtualClusters[vc] = append(
						h.apiClusterStatus.VirtualClusters[vc], c.(*VirtualCell).GetAPIStatus())
				}
			}
		}
		for _, ccl := range vcs.getReservedCellList() {
			for _, c := range ccl[CellLevel(len(ccl))] {
				h.apiClusterStatus.VirtualClusters[vc] = append(
					h.apiClusterStatus.VirtualClusters[vc], c.(*VirtualCell).GetAPIStatus())
			}
		}
	}
}

// initReservations creates static bindings for the reserved cells, and removes the
// reserved physical cells from the free cell list.
func (h *HivedAlgorithm) initReservations(reservedCells map[api.VirtualClusterName]map[api.ReservationId]*PhysicalCell) {
	for vcn, vcReservation := range reservedCells {
		for rid, physical := range vcReservation {
			h.vcFreeCellNum[vcn][physical.GetChain()][physical.GetLevel()]--
			h.allVCFreeCellNum[physical.GetChain()][physical.GetLevel()]--
			h.removeCellFromFreeList(physical)
			virtualList := h.vcSchedulers[vcn].getReservedCellList()[rid]
			virtual := virtualList[CellLevel(len(virtualList))][0].(*VirtualCell)
			bindCell(physical, virtual)
		}
	}
}

// initBadNodes marks all the physical nodes defined in the config as bad,
// and wait for K8s's AddNode calls to inform the healthy nodes in them.
func (h *HivedAlgorithm) initBadNodes() {
	klog.Info("Init all nodes defined in the config to bad first, " +
		"and wait for K8s to inform the healthy nodes (addNode)")
	for _, ccl := range h.fullCellList {
		for _, c := range ccl[CellLevel(len(ccl))] {
			nodes, _ := c.(*PhysicalCell).GetPhysicalPlacement()
			for _, n := range nodes {
				h.setBadNode(n)
			}
		}
	}
}

// setBadNode marks a node and the cells in it as bad.
func (h *HivedAlgorithm) setBadNode(nodeName string) {
	if h.badNodes.Contains(nodeName) {
		return
	}
	h.badNodes.Add(nodeName)
	for _, ccl := range h.fullCellList {
		for _, gpu := range ccl[1] {
			pGpu := gpu.(*PhysicalCell)
			nodes, _ := pGpu.GetPhysicalPlacement()
			if nodes[0] == nodeName {
				h.setBadCell(pGpu)
			}
		}
	}
}

// setBadNode marks a node and the cells in it as healthy.
func (h *HivedAlgorithm) setHealthyNode(nodeName string) {
	if !h.badNodes.Contains(nodeName) {
		return
	}
	h.badNodes.Delete(nodeName)
	for _, ccl := range h.fullCellList {
		for _, gpu := range ccl[1] {
			pGpu := gpu.(*PhysicalCell)
			nodes, _ := pGpu.GetPhysicalPlacement()
			if nodes[0] == nodeName {
				h.setHealthyCell(pGpu)
			}
		}
	}
}

// setBadCell marks a physical cell (and also the virtual cell it is bound to) as bad,
// and recursively for its parent, guaranteeing that a cell is bad if all of its children are bad.
func (h *HivedAlgorithm) setBadCell(c *PhysicalCell) {
	c.SetHealthiness(api.CellBad)
	if inFreeCellList(c) {
		h.incrementBadFreeCell(c.GetChain(), c.GetLevel(), 1)
	}
	if parent := c.GetParent(); parent != nil {
		for _, buddy := range parent.GetChildren() {
			if buddy.(*PhysicalCell).GetAPIStatus().CellHealthiness != api.CellBad {
				return
			}
		}
		h.setBadCell(parent.(*PhysicalCell))
	}
}

// setHealthyCell marks a physical cell (and also the virtual cell it is bound to) as healthy,
// and recursively for its parent, guaranteeing that a cell is healthy if any of its children is healthy.
func (h *HivedAlgorithm) setHealthyCell(c *PhysicalCell) {
	c.SetHealthiness(api.CellHealthy)
	if inFreeCellList(c) {
		h.decrementBadFreeCell(c.GetChain(), c.GetLevel(), 1)
	}
	if parent := c.GetParent(); parent != nil {
		if pp := parent.(*PhysicalCell); pp.GetAPIStatus().CellHealthiness == api.CellBad {
			h.setHealthyCell(pp)
		}
	}
}

// incrementBadFreeCell increments the number of bad free cells, and checks if the healthy cells
// in the physical cluster can satisfy the free cells in all the VCs.
func (h *HivedAlgorithm) incrementBadFreeCell(c CellChain, l CellLevel, n int32) {
	h.badFreeCellNum[c][l] += n
	if h.allVCFreeCellNum[c][l] > h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l] {
		klog.Warningf("Cell type %v (chain %v level %v) now has fewer healthy cells (healthy %v, bad %v) "+
			"than the total free cells of all the VCs (%v). Certain VCs' cells may be doomed to be bad.",
			h.cellTypes[c][l], c, l, h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l],
			h.badFreeCellNum[c][l], h.allVCFreeCellNum[c][l])
		h.checkVCDoomedBadCells(c, l)
	}
}

// decrementBadFreeCell decrements the number of bad free cells, and checks if the healthy cells
// in the physical cluster can satisfy the free cells in all the VCs.
func (h *HivedAlgorithm) decrementBadFreeCell(c CellChain, l CellLevel, n int32) {
	h.badFreeCellNum[c][l] -= n
	if h.allVCFreeCellNum[c][l] >= h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l] {
		if h.allVCFreeCellNum[c][l] == h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l] {
			klog.Infof("Cell type %v (chain %v level %v) now has sufficient healthy cells (healthy %v, bad %v) "+
				"for allocating the total free cells of all the VCs (%v).",
				h.cellTypes[c][l], c, l, h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l],
				h.badFreeCellNum[c][l], h.allVCFreeCellNum[c][l])
		}
		h.checkVCDoomedBadCells(c, l)
	}
}

// checkVCDoomedBadCells checks if the healthy cells in the physical cluster can satisfy the free cells
// in each VC. If not, we will mark some of the virtual cells in the VC as bad (i.e., doomed to be bad).
func (h *HivedAlgorithm) checkVCDoomedBadCells(c CellChain, l CellLevel) {
	for vcName, vcFreeCellNum := range h.vcFreeCellNum {
		if _, ok := vcFreeCellNum[c][l]; !ok {
			continue
		}
		prevDoomedBadNum := h.vcDoomedBadCellNum[vcName][c][l]
		h.vcDoomedBadCellNum[vcName][c][l] = vcFreeCellNum[c][l] - h.totalLeftCellNum[c][l] + h.badFreeCellNum[c][l]
		if h.vcDoomedBadCellNum[vcName][c][l] <= 0 {
			h.vcDoomedBadCellNum[vcName][c][l] = 0
			if prevDoomedBadNum > 0 {
				klog.Infof("Cell type %v (chain %v level %v) now has sufficient healthy cells (healthy %v, bad %v) "+
					"for allocating the free cells of the VC %v (%v).",
					h.cellTypes[c][l], c, l,
					h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l], h.badFreeCellNum[c][l],
					vcName, vcFreeCellNum[c][l])
			}
		} else {
			klog.Warningf("Cell type %v (chain %v level %v) now has fewer healthy cells (healthy %v, bad %v) "+
				"than the free cells of the VC %v (%v). Certain cells in the VC are doomed to be bad.",
				h.cellTypes[c][l], c, l,
				h.totalLeftCellNum[c][l]-h.badFreeCellNum[c][l], h.badFreeCellNum[c][l],
				vcName, vcFreeCellNum[c][l])
		}
		if prevDoomedBadNum > h.vcDoomedBadCellNum[vcName][c][l] {
			numToReduce := prevDoomedBadNum - h.vcDoomedBadCellNum[vcName][c][l]
			n := int32(0)
			for _, vc := range h.vcSchedulers[vcName].getNonReservedFreeCellList()[c][l] {
				virtualCell := vc.(*VirtualCell)
				if virtualCell.GetPhysicalCell() == nil && virtualCell.GetAPIStatus().CellHealthiness == api.CellBad {
					virtualCell.GetAPIStatus().CellHealthiness = api.CellHealthy
					n++
					if n >= numToReduce {
						break
					}
				}
			}
		} else if prevDoomedBadNum < h.vcDoomedBadCellNum[vcName][c][l] {
			numToIncrease := h.vcDoomedBadCellNum[vcName][c][l] - prevDoomedBadNum
			n := int32(0)
			for _, vc := range h.vcSchedulers[vcName].getNonReservedFreeCellList()[c][l] {
				virtualCell := vc.(*VirtualCell)
				if virtualCell.GetPhysicalCell() == nil && virtualCell.GetAPIStatus().CellHealthiness != api.CellBad {
					virtualCell.GetAPIStatus().CellHealthiness = api.CellBad
					n++
					if n >= numToIncrease {
						break
					}
				}
			}
		}
	}
}

// scheduleNewAffinityGroup schedules each pod of a new affinity group to a set of GPUs
// (in both the physical cluster and the VC). This is the entrance of a new scheduling attempt.
func (h *HivedAlgorithm) scheduleNewAffinityGroup(
	pod *core.Pod,
	s *api.PodSchedulingSpec,
	suggestedNodes common.Set) (physicalPlacement groupPhysicalPlacement, virtualPlacement groupVirtualPlacement) {

	priority := CellPriority(s.Priority)
	sr := schedulingRequest{
		vc:                   s.VirtualCluster,
		reservationId:        s.ReservationId,
		priority:             priority,
		affinityGroupName:    s.AffinityGroup.Name,
		affinityGroupPodNums: map[int32]int32{},
	}
	for _, m := range s.AffinityGroup.Members {
		// we will merge group members with same GPU number
		sr.affinityGroupPodNums[m.GpuNumber] += m.PodNumber
	}
	h.validateSchedulingRequest(sr, pod)
	if sr.reservationId != "" {
		klog.Infof("Use reservation %v", s.ReservationId)
		physicalPlacement, virtualPlacement = h.processSchedulingRequest(sr, suggestedNodes)
	} else {
		physicalPlacement, virtualPlacement = h.scheduleAffinityGroupForGpuType(sr, s.GpuType, pod, suggestedNodes)
	}
	if physicalPlacement != nil {
		klog.Infof("Succeeded in scheduling group %v", s.AffinityGroup.Name)
	} else {
		klog.Infof("Failed to schedule group %v", s.AffinityGroup.Name)
	}
	return physicalPlacement, virtualPlacement
}

// scheduleAffinityGroupForGpuType schedules an affinity group in a certain cell chain.
// If a GPU type is specified, it will be scheduled to a chain that contains this GPU type.
// Otherwise any GPU type will be tried (the first one that succeeds will be picked).
func (h *HivedAlgorithm) scheduleAffinityGroupForGpuType(
	sr schedulingRequest,
	gpuType string,
	pod *core.Pod,
	suggestedNodes common.Set) (physicalPlacement groupPhysicalPlacement, virtualPlacement groupVirtualPlacement) {

	if gpuType != "" {
		if chains := h.chains[gpuType]; chains == nil {
			panic(internal.NewBadRequestError(fmt.Sprintf(
				"[%v]: pod requesting GPU type %v which the whole cluster does not have",
				internal.Key(pod), gpuType)))
		} else {
			vcHasType := false
			for _, chain := range chains {
				if h.vcSchedulers[sr.vc].getNonReservedFullCellList()[chain] != nil {
					vcHasType = true
				}
				sr.chain = chain
				if physicalPlacement, virtualPlacement = h.processSchedulingRequest(sr, suggestedNodes); physicalPlacement != nil {
					return physicalPlacement, virtualPlacement
				}
			}
			if sr.priority >= minGuaranteedPriority && !vcHasType {
				panic(internal.NewBadRequestError(fmt.Sprintf(
					"[%v]: pod requesting GPU type %v which VC %v does not have",
					internal.Key(pod), gpuType, sr.vc)))
			}
		}
	} else {
		for _, chains := range h.chains {
			for _, chain := range chains {
				sr.chain = chain
				if physicalPlacement, virtualPlacement = h.processSchedulingRequest(sr, suggestedNodes); physicalPlacement != nil {
					return physicalPlacement, virtualPlacement
				}
			}
		}
	}
	return nil, nil
}

// validateSchedulingRequest checks the existence of VC and reservation ID, and the legality of priority.
func (h *HivedAlgorithm) validateSchedulingRequest(sr schedulingRequest, pod *core.Pod) {
	var message string
	if h.vcSchedulers[sr.vc] == nil {
		message = fmt.Sprintf("VC %v does not exists!", sr.vc)
	} else if sr.reservationId != "" {
		if h.vcSchedulers[sr.vc].getReservedCellList()[sr.reservationId] == nil {
			message = fmt.Sprintf("VC %v does not have reservation %v", sr.vc, sr.reservationId)
		} else if sr.priority == opportunisticPriority {
			message = fmt.Sprintf("opportunistic pod not supported to use reservation %v", sr.reservationId)
		}
	}
	if message != "" {
		panic(internal.NewBadRequestError(fmt.Sprintf("[%v]: %v", internal.Key(pod), message)))
	}
}

// processSchedulingRequest feeds a request to a VC scheduler or the opportunistic scheduler depending on its priority.
func (h *HivedAlgorithm) processSchedulingRequest(
	sr schedulingRequest,
	suggestedNodes common.Set) (groupPhysicalPlacement, groupVirtualPlacement) {

	if sr.priority >= minGuaranteedPriority {
		return h.scheduleGuaranteedAffinityGroup(sr, suggestedNodes)
	} else {
		return h.scheduleOpportunisticAffinityGroup(sr, suggestedNodes), nil
	}
}

// scheduleGuaranteedAffinityGroup schedules an affinity group in its VC,
// and then maps the placement in VC to the physical cluster.
func (h *HivedAlgorithm) scheduleGuaranteedAffinityGroup(
	sr schedulingRequest,
	suggestedNodes common.Set) (groupPhysicalPlacement, groupVirtualPlacement) {

	// schedule in VC
	virtualPlacement := h.vcSchedulers[sr.vc].schedule(sr)
	if virtualPlacement == nil {
		return nil, nil
	}
	// map the vc placement to the physical cluster
	return h.mapVirtualPlacementToPhysical(virtualPlacement, sr, suggestedNodes), virtualPlacement
}

// mapVirtualPlacementToPhysical maps a VC placement to the physical cluster,
// by mapping each virtual GPU cell to a physical GPU cell.
func (h *HivedAlgorithm) mapVirtualPlacementToPhysical(
	virtualPlacement groupVirtualPlacement,
	sr schedulingRequest,
	suggestedNodes common.Set) groupPhysicalPlacement {

	gpuNums := common.Int32MapKeys(sr.affinityGroupPodNums)
	common.SortInt32(gpuNums)
	physicalPlacement := groupPhysicalPlacement{}
	for _, podGpuNum := range gpuNums {
		podPlacements := virtualPlacement[podGpuNum]
		physicalPlacement[podGpuNum] = make([]CellList, len(podPlacements))
		for i, podGpus := range podPlacements {
			physicalPlacement[podGpuNum][i] = make(CellList, len(podGpus))
			for j, gpu := range podGpus {
				vGpu := gpu.(*VirtualCell)
				if pGpu := vGpu.GetPhysicalCell(); pGpu != nil {
					// Two possible cases of finding the virtual cell has been bound to a physical cell:
					// 1. A group of this VC is running on this physical cell (then the cell will be in CellUsed state).
					// We can either lazy-preempt this group and try to use another physical cell, or just preempt the group.
					// 2. A group of this VC is preempting another group on this cell (CellAcquiring or CellAcquired).
					// We will cancel the ongoing preemption and delete the preemptor group.
					if pGpu.GetState() == cellUsed {
						if groupToPreempt := vGpu.GetPhysicalCell().GetUsingGroup(); groupToPreempt.lazyPreemptionEnable {
							h.lazyPreemptAffinityGroup(groupToPreempt, sr.affinityGroupName)
						}
					} else { // cellAcquiring or cellAcquired
						h.deletePreemptorAffinityGroup(pGpu.GetAcquiringGroup(), sr.affinityGroupName)
					}
				}
				physicalPlacement[podGpuNum][i][j] = mapVirtualCellToPhysical(vGpu, h.freeCellList[sr.chain], suggestedNodes)
			}
		}
	}
	// the cell bindings are temporary, and should be cleared after the scheduling is done
	clearPreBindings(virtualPlacement)
	return physicalPlacement
}

// scheduleOpportunisticAffinityGroup calls the opportunistic pod scheduler to schedule an affinity group.
func (h *HivedAlgorithm) scheduleOpportunisticAffinityGroup(
	sr schedulingRequest,
	suggestedNodes common.Set) (physicalPlacement groupPhysicalPlacement) {

	physicalPlacement = h.opportunisticSchedulers[sr.chain].Schedule(
		sr.affinityGroupPodNums, opportunisticPriority, suggestedNodes)
	if physicalPlacement == nil {
		klog.Infof("Insufficient capacity in PC for scheduling request: GPU numbers %v, priority %v, chain %v",
			sr.affinityGroupPodNums, sr.priority, sr.chain)
	} else {
		klog.Infof("Succeeded in scheduling in PC for scheduling request: GPU numbers %v, priority %v, chain %v",
			sr.affinityGroupPodNums, sr.priority, sr.chain)
	}
	return physicalPlacement
}

// createAllocatedAffinityGroup creates a new affinity group and allocate the resources.
func (h *HivedAlgorithm) createAllocatedAffinityGroup(s *api.PodSchedulingSpec, info *api.PodBindInfo, pod *core.Pod) {
	klog.Infof("[%v]: Creating new allocated affinity group: %v", internal.Key(pod), s.AffinityGroup.Name)
	newGroup := newAlgoAffinityGroup(
		s.AffinityGroup, s.VirtualCluster, s.GangReleaseEnable, s.LazyPreemptionEnable, s.Priority, groupAllocated)
	shouldLazyPreempt := false
	for _, gms := range info.AffinityGroupBindInfo {
		gpuNumber := int32(len(gms.PodPlacements[0].PhysicalGpuIndices))
		for podIndex := int32(0); podIndex < int32(len(gms.PodPlacements)); podIndex++ {
			node := gms.PodPlacements[podIndex].PhysicalNode
			for gpuIndex := int32(0); gpuIndex < int32(
				len(gms.PodPlacements[podIndex].PhysicalGpuIndices)); gpuIndex++ {
				pGpu, vGpu, lazyPreempt := h.findAllocatedGpu(
					gpuIndex,
					gms.PodPlacements[podIndex].PhysicalGpuIndices,
					gms.PodPlacements[podIndex].PreassignedCellTypes,
					CellChain(info.CellChain), node, shouldLazyPreempt, s, newGroup, pod)
				if pGpu == nil {
					// pGpu not being found means that this GPU address does not exist in the spec.
					// we simply ignore this GPU, and let the job run normally
					// (but we cannot ignore the other GPUs of this pod that are still in the spec,
					// otherwise it may cause resource conflicts)
					continue
				} else {
					newGroup.physicalGpuPlacement[gpuNumber][podIndex][gpuIndex] = pGpu
					if lazyPreempt == nil {
						newGroup.virtualGpuPlacement = nil
					} else if vGpu != nil {
						newGroup.virtualGpuPlacement[gpuNumber][podIndex][gpuIndex] = vGpu
						if vGpu.GetPhysicalCell() != nil {
							groupToPreempt := vGpu.GetPhysicalCell().GetUsingGroup()
							h.lazyPreemptAffinityGroup(groupToPreempt, newGroup.name)
						}
					} else {
						shouldLazyPreempt = shouldLazyPreempt || *lazyPreempt
					}
					// Even if we have successfully found the vGpu and pGpu, there is still one possibility
					// that we should not bind them: allocating the physical cell may lead to broken safety.
					// Such case won't happen by design as buddy alloc guarantees safety; but this could
					// happen due to inconsistency of VC assignments for reasons like reconfiguration.
					// In this case, we will lazy preempt this affinity group.
					success, message := h.allocateGpu(pGpu, vGpu, CellPriority(s.Priority), newGroup.vc)
					pGpu.AddUsingGroup(newGroup)
					setState(pGpu, cellUsed)
					if !success {
						shouldLazyPreempt = true
						klog.Warningf("[%v]: %v", internal.Key(pod), message)
					}
				}
			}
		}
	}
	if shouldLazyPreempt {
		h.lazyPreemptAffinityGroup(newGroup, newGroup.name)
	}
	h.allocatedAffinityGroups[s.AffinityGroup.Name] = newGroup
	klog.Infof("[%v]: New allocated affinity group created: %v", internal.Key(pod), s.AffinityGroup.Name)
}

// deleteAllocatedAffinityGroup deletes a new affinity group and release the resources (that are not
// allocated to a preemptor group).
func (h HivedAlgorithm) deleteAllocatedAffinityGroup(g *AlgoAffinityGroup, pod *core.Pod) {
	klog.Infof("[%v]: All pods complete, deleting affinity group: %v",
		internal.Key(pod), g.name)
	for _, podPlacements := range g.physicalGpuPlacement {
		for _, podPlacement := range podPlacements {
			for _, gpu := range podPlacement {
				if gpu == nil {
					continue
				}
				pGpu := gpu.(*PhysicalCell)
				pGpu.DeleteUsingGroup(g)
				// state of pGpu can be either Used or Acquiring
				if pGpu.GetState() == cellUsed {
					h.releaseGpu(pGpu, g.vc)
					setState(pGpu, cellFree)
				} else { // cellAcquiring
					// When pGpu is in cellAcquiring state, we shouldn't call h.releaseGpu
					// because it must have been allocated to the acquiring group before
					setState(pGpu, cellAcquired)
				}
			}
		}
	}
	delete(h.allocatedAffinityGroups, g.name)
	klog.Infof("[%v]: Allocated affinity group deleted: %v", internal.Key(pod), g.name)
}

// createPreemptorAffinityGroup creates a new affinity group that is preempting some other groups.
// Its resources are immediately allocated to the group (even if the preemption victims have not yet been deleted),
// so that other groups will not be scheduled to the same placement (unless they have higher priorities).
// This avoids the case where multiple groups preempt the same victims simultaneously, which may cause resource dead lock.
func (h *HivedAlgorithm) createPreemptorAffinityGroup(
	s *api.PodSchedulingSpec,
	physicalPlacement groupPhysicalPlacement,
	virtualPlacement groupVirtualPlacement,
	pod *core.Pod) {

	klog.Infof("[%v]: Creating new preemptor affinity group: %v", internal.Key(pod), s.AffinityGroup.Name)
	newGroup := newAlgoAffinityGroup(
		s.AffinityGroup, s.VirtualCluster, s.GangReleaseEnable, s.LazyPreemptionEnable, s.Priority, groupPreempting)
	newGroup.physicalGpuPlacement = physicalPlacement
	newGroup.virtualGpuPlacement = virtualPlacement
	for gpuNum := range physicalPlacement {
		for podIndex := range physicalPlacement[gpuNum] {
			for gpuIndex, gpu := range physicalPlacement[gpuNum][podIndex] {
				pGpu := gpu.(*PhysicalCell)
				vGpu := virtualPlacement[gpuNum][podIndex][gpuIndex].(*VirtualCell)
				if pGpu.GetState() == cellUsed {
					usingGroup := pGpu.GetUsingGroup()
					h.releaseGpu(pGpu, usingGroup.vc)
					usingGroup.state = groupBeingPreempted
				}
				h.allocateGpu(pGpu, vGpu, CellPriority(s.Priority), newGroup.vc)
				pGpu.AddAcquiringGroup(newGroup)
				// state of pGpu can be either Used or Free
				if pGpu.GetState() == cellUsed {
					setState(pGpu, cellAcquiring)
				} else { // cellFree
					setState(pGpu, cellAcquired)
				}
			}
		}
	}
	newGroup.preemptorPods[internal.Key(pod)] = pod
	h.preemptorAffinityGroups[s.AffinityGroup.Name] = newGroup
	klog.Infof("[%v]: New preemptor affinity group created: %v", internal.Key(pod), newGroup.name)
}

// deletePreemptorAffinityGroup revokes a preemption and deletes the affinity group that is
// still waiting for the completion of the preemption.
func (h *HivedAlgorithm) deletePreemptorAffinityGroup(g *AlgoAffinityGroup, newGroupName string) {
	if newGroupName == "" {
		klog.Infof("All preemptor pods deleted, deleting preemptor affinity group %v", g.name)
	} else {
		klog.Infof("Preemption canceled by a higher-priority group %v, deleting preemptor affinity group %v",
			newGroupName, g.name)
	}
	for gpuNum := range g.physicalGpuPlacement {
		for podIndex := range g.physicalGpuPlacement[gpuNum] {
			for _, gpu := range g.physicalGpuPlacement[gpuNum][podIndex] {
				pGpu := gpu.(*PhysicalCell)
				h.releaseGpu(pGpu, g.vc)
				pGpu.DeleteAcquiringGroup(pGpu.GetAcquiringGroup())
				// state of pGpu can be either Acquiring or Acquired
				if pGpu.GetState() == cellAcquiring {
					setState(pGpu, cellUsed)
					// return the cell to the group being preempted
					beingPreemptedGroup := pGpu.GetUsingGroup()
					var beingPreemptedVGpu *VirtualCell
					if beingPreemptedGroup.virtualGpuPlacement != nil {
						beingPreemptedVGpu = retrieveVirtualCell(
							beingPreemptedGroup.physicalGpuPlacement,
							beingPreemptedGroup.virtualGpuPlacement, pGpu)
					}
					h.allocateGpu(
						pGpu, beingPreemptedVGpu, CellPriority(beingPreemptedGroup.priority), beingPreemptedGroup.vc)
				} else { // cellAcquired
					setState(pGpu, cellFree)
				}
			}
		}
	}
	delete(h.preemptorAffinityGroups, g.name)
	klog.Infof("Preemptor affinity group %v deleted", g.name)
}

// preemptorAffinityGroupToAllocated let a preemptor affinity group whose preemption has completed
// transit to allocated state.
func (h *HivedAlgorithm) preemptorAffinityGroupToAllocated(g *AlgoAffinityGroup, pod *core.Pod) {
	for gpuNum := range g.physicalGpuPlacement {
		for podIndex := range g.physicalGpuPlacement[gpuNum] {
			for _, gpu := range g.physicalGpuPlacement[gpuNum][podIndex] {
				pGpu := gpu.(*PhysicalCell)
				pGpu.DeleteAcquiringGroup(g)
				pGpu.AddUsingGroup(g)
				setState(pGpu, cellUsed)
			}
		}
	}
	g.state = groupAllocated
	g.preemptorPods = nil
	h.allocatedAffinityGroups[g.name] = g
	delete(h.preemptorAffinityGroups, g.name)
	klog.Infof("[%v]: Preemptor affinity group %v transits to allocated", internal.Key(pod), g.name)
}

// lazyPreemptAffinityGroup removes an affinity group from its VC, clears it virtual placement,
// and exposes this decision.
func (h *HivedAlgorithm) lazyPreemptAffinityGroup(victim *AlgoAffinityGroup, preemptor string) {
	for _, podVirtualPlacements := range victim.virtualGpuPlacement {
		for _, podVirtualPlacement := range podVirtualPlacements {
			for _, gpu := range podVirtualPlacement {
				if gpu != nil {
					vGpu := gpu.(*VirtualCell)
					pGpu := vGpu.GetPhysicalCell()
					h.releaseGpu(pGpu, victim.vc)
					h.allocateGpu(pGpu, nil, opportunisticPriority, victim.vc)
				}
			}
		}
	}
	victim.virtualGpuPlacement = nil
	victim.lazyPreemptionStatus = &api.LazyPreemptionStatus{
		Preemptor:      preemptor,
		PreemptionTime: meta.Now(),
	}

	klog.Infof("Affinity group %v is lazy preempted from VC by %v", victim.name, preemptor)
}

// findAllocatedGpu finds the physical and virtual GPUs in the full cell lists for an allocate pod.
// The boolean return value indicates whether the affinity group should be lazy-preempted.
// The bool being nil means the group is OT and has no virtual placement.
func (h *HivedAlgorithm) findAllocatedGpu(
	index int32,
	physicalGpuIndices []int32,
	preassignedCellTypes []api.CellType,
	chain CellChain,
	node string,
	lazyPreempted bool,
	s *api.PodSchedulingSpec,
	group *AlgoAffinityGroup,
	pod *core.Pod) (*PhysicalCell, *VirtualCell, *bool) {

	priority := CellPriority(s.Priority)
	physicalGpuIndex := physicalGpuIndices[index]
	if pGpu := findPhysicalGpu(h.fullCellList, chain, node, physicalGpuIndex); pGpu == nil {
		klog.Warningf(
			"[%v]: cannot find GPU %v on node %v: not found in the spec. pod ignored",
			internal.Key(pod), physicalGpuIndex, node)
		return nil, nil, common.PtrBool(false)
	} else {
		var vGpu *VirtualCell
		if preassignedCellTypes == nil {
			klog.Warningf("[%v]: cannot find virtual cell: preassigned cell not found in pod bind info", internal.Key(pod))
			return pGpu, nil, common.PtrBool(true)
		}
		if group.virtualGpuPlacement != nil && !lazyPreempted {
			preassignedType := preassignedCellTypes[index]
			if preassignedType != "" {
				var preassignedLevel CellLevel
				typeFound := false
				for l, t := range h.cellTypes[pGpu.GetChain()] {
					if t == preassignedType {
						preassignedLevel = l
						typeFound = true
					}
				}
				var message string
				if !typeFound {
					message = fmt.Sprintf("preassigned cell type %v not found in chain %v", preassignedType, pGpu.GetChain())
				} else if vcs := h.vcSchedulers[s.VirtualCluster]; vcs == nil {
					message = fmt.Sprintf("VC %v not found", s.VirtualCluster)
				} else {
					vccl := vcs.getNonReservedFreeCellList()[pGpu.GetChain()]
					str := string(pGpu.GetChain())
					if s.ReservationId != "" {
						vccl = vcs.getReservedCellList()[s.ReservationId]
						str = string(s.ReservationId)
					}
					if vccl == nil {
						message = fmt.Sprintf("VC %v has no cell for %v", s.VirtualCluster, str)
					} else {
						vGpu, message = mapPhysicalCellToVirtual(pGpu, vccl, preassignedLevel, priority)
					}
				}
				if vGpu == nil {
					klog.Warningf("[%v]: cannot find virtual cell: %v", internal.Key(pod), message)
					return pGpu, nil, common.PtrBool(true)
				} else {
					return pGpu, vGpu, common.PtrBool(false)
				}
			} else {
				return pGpu, nil, nil
			}
		} else {
			return pGpu, nil, common.PtrBool(false)
		}
	}
}

// allocateGpu creates the cell bindings, removes the physical cell from the free list
// (if necessary), and sets the priority.
func (h *HivedAlgorithm) allocateGpu(
	pGpu *PhysicalCell,
	vGpu *VirtualCell,
	p CellPriority,
	vcn api.VirtualClusterName) (success bool, message string) {

	success = true
	if vGpu != nil {
		setPriority(vGpu, p)
		updateUsedGpuNumAtPriority(vGpu, p, true)
		setPriority(pGpu, p)
		updateUsedGpuNumAtPriority(pGpu, p, true)
		pac := vGpu.GetPreAssignedCell()
		preassignedNewlyBound := pac.GetPhysicalCell() == nil
		preassignedBad := pac.GetAPIStatus().CellHealthiness == api.CellBad
		bindCell(pGpu, vGpu)
		if preassignedNewlyBound {
			c := pac.GetChain()
			l := pac.GetLevel()
			h.vcFreeCellNum[vcn][c][l]--
			h.allVCFreeCellNum[c][l]--
			if preassignedBad {
				// this means the preassigned cell was counted as a doomed bad cell
				h.vcDoomedBadCellNum[vcn][c][l]--
			}
			// remove the allocated cell from the free list (possibly splitting cells)
			success, message = h.removeCellFromFreeList(vGpu.GetPreAssignedCell().GetPhysicalCell())
		}
	} else {
		setPriority(pGpu, opportunisticPriority)
		updateUsedGpuNumAtPriority(pGpu, opportunisticPriority, true)
		pGpu.GetAPIStatus().VC = vcn
		h.apiClusterStatus.VirtualClusters[vcn] = append(
			h.apiClusterStatus.VirtualClusters[vcn], generateOpporVirtualCell(pGpu.GetAPIStatus()))
	}
	return success, message
}

// releaseGpu destroys the cell bindings, adds the physical cell back to the free list
// (if necessary), and resets the priority.
func (h *HivedAlgorithm) releaseGpu(pGpu *PhysicalCell, vcn api.VirtualClusterName) {
	if vGpu := pGpu.GetVirtualCell(); vGpu != nil {
		preassignedPhysical := vGpu.GetPreAssignedCell().GetPhysicalCell()
		unbindCell(pGpu)
		if vGpu.GetPreAssignedCell().GetPhysicalCell() == nil {
			h.vcFreeCellNum[vcn][vGpu.GetChain()][vGpu.GetPreAssignedCell().GetLevel()]++
			h.allVCFreeCellNum[vGpu.GetChain()][vGpu.GetPreAssignedCell().GetLevel()]++
			// add the released cell back to the free list (possibly merging cells)
			h.addCellToFreeList(preassignedPhysical)
		}
		updateUsedGpuNumAtPriority(vGpu, vGpu.GetPriority(), false)
		setPriority(vGpu, freePriority)
	} else {
		pGpu.GetAPIStatus().VC = ""
		h.apiClusterStatus.VirtualClusters[vcn] = deleteOpporVirtualCell(
			h.apiClusterStatus.VirtualClusters[vcn], pGpu.GetAddress())
	}
	updateUsedGpuNumAtPriority(pGpu, pGpu.GetPriority(), false)
	setPriority(pGpu, freePriority)
}

// removeCellFromFreeList removes a cell from the free cell list and splits its parent recursively if needed.
func (h *HivedAlgorithm) removeCellFromFreeList(c *PhysicalCell) (success bool, message string) {
	chain := c.GetChain()
	success = true
	// reduce the left cell numbers for the lower levels and do safety check
	numToRemove := int32(len(h.fullCellList[chain][c.GetLevel()][0].GetChildren()))
	isBad := c.GetAPIStatus().CellHealthiness == api.CellBad
	for l := c.GetLevel() - 1; l >= lowestLevel; l-- {
		h.totalLeftCellNum[chain][l] -= numToRemove
		if h.totalLeftCellNum[chain][l] < h.allVCFreeCellNum[chain][l] {
			success = false
			message = fmt.Sprintf("Adding pod would lead to broken safety: cell type %v, %v left, %v free cells in all VCs",
				h.cellTypes[chain][l], h.totalLeftCellNum[chain][l], h.allVCFreeCellNum[chain][l])
		}
		// if the used cell is bad, we should exclude it from the bad free cells (the same below)
		if isBad {
			h.decrementBadFreeCell(chain, l, numToRemove)
		} else {
			h.checkVCDoomedBadCells(chain, l)
		}
		numToRemove *= int32(len(h.fullCellList[chain][l][0].GetChildren()))
	}
	for terminate := false; ; {
		l := c.GetLevel()
		parent := c.GetParent()
		if parent != nil {
			pp := parent.(*PhysicalCell)
			if pp.IsSplit() {
				terminate = true
			} else {
				h.freeCellList[chain][l] = append(h.freeCellList[chain][l], pp.GetChildren()...)
				pp.SetSplit(true)
			}
		} else {
			terminate = true
		}
		h.freeCellList[chain].remove(c, l)
		h.totalLeftCellNum[chain][l]--
		if h.totalLeftCellNum[chain][l] < h.allVCFreeCellNum[chain][l] {
			success = false
			message = fmt.Sprintf("Adding pod would lead to broken safety: cell type %v, %v left, %v free cells in all VCs",
				h.cellTypes[chain][l], h.totalLeftCellNum[chain][l], h.allVCFreeCellNum[chain][l])
		}
		if c.GetAPIStatus().CellHealthiness == api.CellBad {
			h.decrementBadFreeCell(chain, l, 1)
		} else {
			h.checkVCDoomedBadCells(chain, l)
		}
		if terminate {
			break
		} else {
			c = parent.(*PhysicalCell)
		}
	}
	return success, message
}

// addCellToFreeList adds a cell to the free cell list and merges its buddies recursively if needed.
func (h *HivedAlgorithm) addCellToFreeList(c *PhysicalCell) {
	chain := c.GetChain()
	// increase the left cell numbers for the lower levels
	numToAdd := int32(len(h.fullCellList[chain][c.GetLevel()][0].GetChildren()))
	isBad := c.GetAPIStatus().CellHealthiness == api.CellBad
	for l := c.GetLevel() - 1; l >= lowestLevel; l-- {
		h.totalLeftCellNum[chain][l] += numToAdd
		// if the freed cell is bad, we should add it to the bad free cells (the same below)
		if isBad {
			h.incrementBadFreeCell(chain, l, numToAdd)
		} else {
			h.checkVCDoomedBadCells(chain, l)
		}
		numToAdd *= int32(len(h.fullCellList[chain][l][0].GetChildren()))
	}
	for terminate := false; ; {
		l := c.GetLevel()
		parent := c.GetParent()
		if parent != nil {
			allBuddyFree := true
			for _, buddy := range parent.GetChildren() {
				if buddy.(*PhysicalCell).GetVirtualCell() != nil {
					allBuddyFree = false
					break
				}
			}
			if !allBuddyFree {
				terminate = true
			} else {
				for _, buddy := range parent.GetChildren() {
					if !CellEqual(buddy, c) {
						h.freeCellList[chain].remove(buddy, l)
					}
				}
				parent.(*PhysicalCell).SetSplit(false)
			}
		} else {
			terminate = true
		}
		h.totalLeftCellNum[chain][l]++
		if c.GetAPIStatus().CellHealthiness == api.CellBad {
			h.incrementBadFreeCell(chain, l, 1)
		} else {
			h.checkVCDoomedBadCells(chain, l)
		}
		if terminate {
			h.freeCellList[chain][l] = append(h.freeCellList[chain][l], c)
			break
		} else {
			c = parent.(*PhysicalCell)
		}
	}
}
