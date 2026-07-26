package collector

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/tekikaito/kshows/internal/model"
)

// effectiveResources computes a pod's effective requests and limits the way
// the scheduler does: regular containers are summed, restartable (sidecar)
// init containers add to that sum, and each remaining init container is
// compared as a max against the running total (init containers run one at a
// time, so only the largest matters).
func effectiveResources(spec *corev1.PodSpec) (requests, limits model.Resources) {
	var reqCPU, reqMem, limCPU, limMem int64
	for i := range spec.Containers {
		r := &spec.Containers[i].Resources
		reqCPU += cpuMillis(r.Requests)
		reqMem += memBytes(r.Requests)
		limCPU += cpuMillis(r.Limits)
		limMem += memBytes(r.Limits)
	}
	var sideReqCPU, sideReqMem, sideLimCPU, sideLimMem int64
	var initReqCPU, initReqMem, initLimCPU, initLimMem int64
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sideReqCPU += cpuMillis(c.Resources.Requests)
			sideReqMem += memBytes(c.Resources.Requests)
			sideLimCPU += cpuMillis(c.Resources.Limits)
			sideLimMem += memBytes(c.Resources.Limits)
			continue
		}
		initReqCPU = max(initReqCPU, cpuMillis(c.Resources.Requests))
		initReqMem = max(initReqMem, memBytes(c.Resources.Requests))
		initLimCPU = max(initLimCPU, cpuMillis(c.Resources.Limits))
		initLimMem = max(initLimMem, memBytes(c.Resources.Limits))
	}
	requests = model.Resources{
		CPUMillis: max(reqCPU+sideReqCPU, initReqCPU+sideReqCPU),
		MemBytes:  max(reqMem+sideReqMem, initReqMem+sideReqMem),
	}
	limits = model.Resources{
		CPUMillis: max(limCPU+sideLimCPU, initLimCPU+sideLimCPU),
		MemBytes:  max(limMem+sideLimMem, initLimMem+sideLimMem),
	}
	return requests, limits
}

func cpuMillis(rl corev1.ResourceList) int64 {
	if q, ok := rl[corev1.ResourceCPU]; ok {
		return q.MilliValue()
	}
	return 0
}

func memBytes(rl corev1.ResourceList) int64 {
	if q, ok := rl[corev1.ResourceMemory]; ok {
		return q.Value()
	}
	return 0
}

func quantityValue(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.Value()
	}
	return 0
}
