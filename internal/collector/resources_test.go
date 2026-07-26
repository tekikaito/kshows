package collector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func rl(cpu, mem string) corev1.ResourceList {
	out := corev1.ResourceList{}
	if cpu != "" {
		out[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		out[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return out
}

func TestEffectiveResources(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways

	tests := []struct {
		name    string
		spec    corev1.PodSpec
		wantCPU int64
		wantMem int64
	}{
		{
			name: "sums regular containers",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				{Resources: corev1.ResourceRequirements{Requests: rl("500m", "512Mi")}},
				{Resources: corev1.ResourceRequirements{Requests: rl("250m", "256Mi")}},
			}},
			wantCPU: 750, wantMem: 768 << 20,
		},
		{
			name: "large init container dominates",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Resources: corev1.ResourceRequirements{Requests: rl("100m", "128Mi")}},
				},
				InitContainers: []corev1.Container{
					{Resources: corev1.ResourceRequirements{Requests: rl("2", "1Gi")}},
				},
			},
			wantCPU: 2000, wantMem: 1 << 30,
		},
		{
			name: "sidecar init containers add to the sum",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Resources: corev1.ResourceRequirements{Requests: rl("400m", "256Mi")}},
				},
				InitContainers: []corev1.Container{
					{RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: rl("100m", "64Mi")}},
					{Resources: corev1.ResourceRequirements{Requests: rl("300m", "128Mi")}},
				},
			},
			// max(400+100, 300+100) CPU; max(256+64, 128+64) memory
			wantCPU: 500, wantMem: 320 << 20,
		},
		{
			name:    "no resources at all",
			spec:    corev1.PodSpec{Containers: []corev1.Container{{}}},
			wantCPU: 0, wantMem: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := effectiveResources(&tt.spec)
			if req.CPUMillis != tt.wantCPU {
				t.Errorf("cpu = %d, want %d", req.CPUMillis, tt.wantCPU)
			}
			if req.MemBytes != tt.wantMem {
				t.Errorf("mem = %d, want %d", req.MemBytes, tt.wantMem)
			}
		})
	}
}
