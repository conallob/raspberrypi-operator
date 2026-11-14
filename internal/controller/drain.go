package controller

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"
)

// Drainer cordons, drains and uncordons nodes.
type Drainer interface {
	Cordon(ctx context.Context, node string) error
	Uncordon(ctx context.Context, node string) error
	Drain(ctx context.Context, node string, timeout time.Duration) error
}

// KubectlDrainer uses the same library as `kubectl drain`.
type KubectlDrainer struct {
	Client kubernetes.Interface
	Out    io.Writer
}

func (d KubectlDrainer) helper(ctx context.Context, timeout time.Duration) *drain.Helper {
	return &drain.Helper{
		Ctx:                 ctx,
		Client:              d.Client,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		Timeout:             timeout,
		Out:                 d.Out,
		ErrOut:              d.Out,
	}
}

func (d KubectlDrainer) node(ctx context.Context, name string) (*corev1.Node, error) {
	return d.Client.CoreV1().Nodes().Get(ctx, name, metav1GetOptions)
}

// Cordon implements Drainer.
func (d KubectlDrainer) Cordon(ctx context.Context, name string) error {
	n, err := d.node(ctx, name)
	if err != nil {
		return err
	}
	return drain.RunCordonOrUncordon(d.helper(ctx, 0), n, true)
}

// Uncordon implements Drainer.
func (d KubectlDrainer) Uncordon(ctx context.Context, name string) error {
	n, err := d.node(ctx, name)
	if err != nil {
		return err
	}
	return drain.RunCordonOrUncordon(d.helper(ctx, 0), n, false)
}

// Drain implements Drainer. It evicts pods (respecting PodDisruptionBudgets)
// and fails rather than forcing if it cannot, so a stuck PDB stops the rollout
// instead of being overridden.
func (d KubectlDrainer) Drain(ctx context.Context, name string, timeout time.Duration) error {
	if err := drain.RunNodeDrain(d.helper(ctx, timeout), name); err != nil {
		return fmt.Errorf("draining %s: %w", name, err)
	}
	return nil
}
