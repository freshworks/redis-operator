package service

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
)

// evictionCandidate is a co-located slave pod (belonging to some other RedisFailover) that
// could be evicted to free CPU and/or memory on a node ahead of resizing a master or slave
// pod in place.
type evictionCandidate struct {
	Namespace string
	Name      string
	CPU       resource.Quantity
	Memory    resource.Quantity
}

// ComputeRequiredHeadroom returns how much additional CPU and memory must be freed on
// nodeName for rFailover's podName (master or slave) to resize to its currently desired spec
// - either may be zero/negative if that resource already fits. podName's own current
// allocation is deliberately excluded from the "everything else on the node" totals, and
// rFailover.Spec.Redis.Resources is used directly as the desired usage (the same value
// applies to every replica, since they share one StatefulSet template) - this avoids having
// to reconcile the pod's spec (which already shows the new values once a resize is
// requested) against its status (which shows what's actually allocated until the resize
// completes).
func (r *RedisFailoverChecker) ComputeRequiredHeadroom(rFailover *redisfailoverv1.RedisFailover, nodeName, podName string) (resource.Quantity, resource.Quantity, error) {
	node, err := r.k8sService.GetNode(nodeName)
	if err != nil {
		return resource.Quantity{}, resource.Quantity{}, err
	}
	allocatableCPU := node.Status.Allocatable.Cpu()
	allocatableMemory := node.Status.Allocatable.Memory()

	podList, err := r.k8sService.ListPodsOnNode(nodeName, "")
	if err != nil {
		return resource.Quantity{}, resource.Quantity{}, err
	}

	otherCPU := resource.Quantity{}
	otherMemory := resource.Quantity{}
	for _, pod := range podList.Items {
		if pod.Name == podName && pod.Namespace == rFailover.Namespace {
			continue
		}
		for _, container := range pod.Spec.Containers {
			otherCPU.Add(*container.Resources.Requests.Cpu())
			otherMemory.Add(*container.Resources.Requests.Memory())
		}
	}

	requiredCPU := otherCPU.DeepCopy()
	requiredCPU.Add(*rFailover.Spec.Redis.Resources.Requests.Cpu())
	requiredCPU.Sub(*allocatableCPU)

	requiredMemory := otherMemory.DeepCopy()
	requiredMemory.Add(*rFailover.Spec.Redis.Resources.Requests.Memory())
	requiredMemory.Sub(*allocatableMemory)

	return requiredCPU, requiredMemory, nil
}

// FreeResizeHeadroom evicts co-located slave pods belonging to OTHER RedisFailovers on
// nodeName - never rFailover's own slaves - selecting the minimal-pod-count, minimal-waste
// subset that covers requiredCPU and requiredMemory together (either constraint is skipped
// if its quantity is zero/negative), to make room for rFailover's master or slave pod to resize in
// place. Returns an error if the requirement(s) cannot be covered even by evicting every
// candidate; callers should treat that the same as an Infeasible resize.
func (r *RedisFailoverHealer) FreeResizeHeadroom(rFailover *redisfailoverv1.RedisFailover, nodeName string, requiredCPU, requiredMemory resource.Quantity) error {
	pods, err := r.k8sService.ListPodsOnNode(nodeName, redisRoleLabelKey+"="+redisRoleLabelSlave)
	if err != nil {
		return err
	}

	pool := buildEvictionPool(pods, rFailover.Name)
	remainingCPU := requiredCPU.DeepCopy()
	remainingMemory := requiredMemory.DeepCopy()

	for remainingCPU.Sign() > 0 || remainingMemory.Sign() > 0 {
		chosen := selectEvictionSet(pool, remainingCPU, remainingMemory)
		if chosen == nil {
			return fmt.Errorf("cannot free %s cpu / %s memory of headroom on node %s: not enough evictable slave pods", requiredCPU.String(), requiredMemory.String(), nodeName)
		}

		for _, c := range chosen {
			pool = removeCandidate(pool, c)
			if err := r.k8sService.EvictPod(c.Namespace, c.Name); err != nil {
				r.logger.WithField("namespace", c.Namespace).WithField("pod", c.Name).Warningf("eviction rejected, skipping and continuing with the rest of this batch: %v", err)
				continue // this candidate is out of the pool; the rest of chosen may still succeed
			}
			if remainingCPU.Sign() > 0 {
				remainingCPU.Sub(c.CPU)
			}
			if remainingMemory.Sign() > 0 {
				remainingMemory.Sub(c.Memory)
			}
		}
	}
	return nil
}

// buildEvictionPool lists eviction candidates from pods, excluding any pod that belongs to
// excludeRedisFailoverName - an endpoint's own slaves are never candidates for freeing room
// for that same endpoint's master.
func buildEvictionPool(pods *corev1.PodList, excludeRedisFailoverName string) []evictionCandidate {
	var pool []evictionCandidate
	for _, pod := range pods.Items {
		if pod.Labels[redisFailoverNameLabelKey] == excludeRedisFailoverName {
			continue
		}
		cpu := resource.Quantity{}
		mem := resource.Quantity{}
		for _, c := range pod.Spec.Containers {
			cpu.Add(*c.Resources.Requests.Cpu())
			mem.Add(*c.Resources.Requests.Memory())
		}
		pool = append(pool, evictionCandidate{Namespace: pod.Namespace, Name: pod.Name, CPU: cpu, Memory: mem})
	}
	return pool
}

func removeCandidate(pool []evictionCandidate, target evictionCandidate) []evictionCandidate {
	out := make([]evictionCandidate, 0, len(pool))
	for _, c := range pool {
		if c.Namespace == target.Namespace && c.Name == target.Name {
			continue
		}
		out = append(out, c)
	}
	return out
}

// qualifies reports whether sumCPU/sumMemory cover requiredCPU/requiredMemory. A requirement
// with a zero/negative quantity is treated as already satisfied - this is what makes
// "only CPU increased", "only memory increased", and "both increased" all fall out of the
// same code path instead of needing separate handling.
func qualifies(sumCPU, sumMemory, requiredCPU, requiredMemory resource.Quantity) bool {
	if requiredCPU.Sign() > 0 && sumCPU.Cmp(requiredCPU) < 0 {
		return false
	}
	if requiredMemory.Sign() > 0 && sumMemory.Cmp(requiredMemory) < 0 {
		return false
	}
	return true
}

// wasteScore combines the two resources' over-eviction into one dimensionless ranking value,
// expressed as a fraction of each requirement rather than raw units - CPU (cores) and memory
// (bytes) can't be summed directly, but "20% more CPU than needed" and "20% more memory than
// needed" can. Only resources with a positive requirement contribute.
func wasteScore(sumCPU, sumMemory, requiredCPU, requiredMemory resource.Quantity) float64 {
	var score float64
	if requiredCPU.Sign() > 0 {
		score += (sumCPU.AsApproximateFloat64() - requiredCPU.AsApproximateFloat64()) / requiredCPU.AsApproximateFloat64()
	}
	if requiredMemory.Sign() > 0 {
		score += (sumMemory.AsApproximateFloat64() - requiredMemory.AsApproximateFloat64()) / requiredMemory.AsApproximateFloat64()
	}
	return score
}

// maxExhaustiveCandidates caps the brute-force subset search. Per-node candidate counts are
// expected to be small (tens, not thousands) where 2^n is trivial; beyond this bound we fall
// back to a greedy selection to avoid combinatorial blowup, trading provable optimality for
// bounded runtime.
const maxExhaustiveCandidates = 24

// selectEvictionSet returns the minimal-pod-count, minimal-waste subset of candidates whose
// summed CPU and Memory cover requiredCPU and requiredMemory (each requirement independently
// optional - see qualifies), or nil if no subset (including taking every candidate) does.
func selectEvictionSet(candidates []evictionCandidate, requiredCPU, requiredMemory resource.Quantity) []evictionCandidate {
	if len(candidates) > maxExhaustiveCandidates {
		return selectEvictionSetGreedy(candidates, requiredCPU, requiredMemory)
	}

	n := len(candidates)
	var best []evictionCandidate
	bestWaste := 0.0
	for size := 1; size <= n; size++ {
		found := false
		combinations(n, size, func(idx []int) {
			sumCPU := resource.Quantity{}
			sumMemory := resource.Quantity{}
			for _, i := range idx {
				sumCPU.Add(candidates[i].CPU)
				sumMemory.Add(candidates[i].Memory)
			}
			if !qualifies(sumCPU, sumMemory, requiredCPU, requiredMemory) {
				return
			}
			waste := wasteScore(sumCPU, sumMemory, requiredCPU, requiredMemory)
			if best == nil || waste < bestWaste {
				best = pick(candidates, idx)
				bestWaste = waste
				found = true
			}
		})
		if found {
			return best
		}
	}
	return nil
}

// selectEvictionSetGreedy is the fallback used above maxExhaustiveCandidates: candidates are
// ranked by their combined contribution toward whichever requirements are still outstanding,
// then taken largest-first until both are covered. Not guaranteed minimal-waste, unlike the
// exhaustive search.
func selectEvictionSetGreedy(candidates []evictionCandidate, requiredCPU, requiredMemory resource.Quantity) []evictionCandidate {
	reqCPUf := requiredCPU.AsApproximateFloat64()
	reqMemf := requiredMemory.AsApproximateFloat64()

	contribution := func(c evictionCandidate) float64 {
		var s float64
		if reqCPUf > 0 {
			s += c.CPU.AsApproximateFloat64() / reqCPUf
		}
		if reqMemf > 0 {
			s += c.Memory.AsApproximateFloat64() / reqMemf
		}
		return s
	}

	sorted := append([]evictionCandidate{}, candidates...)
	sort.Slice(sorted, func(i, j int) bool { return contribution(sorted[i]) > contribution(sorted[j]) })

	sumCPU := resource.Quantity{}
	sumMemory := resource.Quantity{}
	var chosen []evictionCandidate
	for _, c := range sorted {
		if qualifies(sumCPU, sumMemory, requiredCPU, requiredMemory) {
			break
		}
		chosen = append(chosen, c)
		sumCPU.Add(c.CPU)
		sumMemory.Add(c.Memory)
	}
	if qualifies(sumCPU, sumMemory, requiredCPU, requiredMemory) {
		return chosen
	}
	return nil
}

// combinations calls fn once for every size-length combination of indices in [0,n).
func combinations(n, size int, fn func(idx []int)) {
	idx := make([]int, size)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == size {
			fn(append([]int{}, idx...))
			return
		}
		for i := start; i < n; i++ {
			idx[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
}

func pick(candidates []evictionCandidate, idx []int) []evictionCandidate {
	out := make([]evictionCandidate, len(idx))
	for i, j := range idx {
		out[i] = candidates[j]
	}
	return out
}
