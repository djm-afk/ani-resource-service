package clusterops

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

func (k *Kubernetes) OpenPodLogs(ctx context.Context, q biz.PodLogQuery) (biz.OpenedPodLog, error) {
	pod, err := apiCall(ctx, func(c context.Context) (*corev1.Pod, error) {
		return k.typed.CoreV1().Pods(q.Namespace).Get(c, q.PodName, metav1.GetOptions{})
	})
	if err != nil {
		return biz.OpenedPodLog{}, err
	}
	if q.ExpectedPodUID != "" && q.ExpectedPodUID != string(pod.UID) {
		return biz.OpenedPodLog{}, biz.NewError("POD_CHANGED")
	}
	container := q.Container
	if container == "" {
		if len(pod.Spec.Containers) != 1 {
			return biz.OpenedPodLog{}, biz.NewError("POD_CONTAINER_REQUIRED")
		}
		container = pod.Spec.Containers[0].Name
	}
	found := false
	for _, c := range pod.Spec.Containers {
		found = found || c.Name == container
	}
	for _, c := range pod.Spec.InitContainers {
		found = found || c.Name == container
	}
	for _, c := range pod.Spec.EphemeralContainers {
		found = found || c.Name == container
	}
	if !found {
		return biz.OpenedPodLog{}, biz.NewError("INVALID_ARGUMENT")
	}
	options := &corev1.PodLogOptions{Container: container, Follow: q.Follow, Previous: q.Previous, Timestamps: q.Timestamps, TailLines: q.TailLines, SinceSeconds: q.SinceSeconds, LimitBytes: q.LimitBytes}
	if q.SinceTime != nil {
		v := metav1.NewTime(*q.SinceTime)
		options.SinceTime = &v
	}
	// Bound only opening, keeping the native TLS/auth transport and an
	// independent no-whole-response-timeout client. After headers arrive the
	// same context lives until reader.Close or the caller's read window ends.
	streamCtx, cancel := context.WithCancel(ctx)
	opening := time.AfterFunc(biz.KubernetesTimeout, cancel)
	reader, err := k.logs.Pods(q.Namespace).GetLogs(q.PodName, options).Stream(streamCtx)
	stopped := opening.Stop()
	if ctx.Err() != nil {
		cancel()
		if reader != nil {
			_ = reader.Close()
		}
		return biz.OpenedPodLog{}, ctx.Err()
	}
	if !stopped || streamCtx.Err() != nil {
		if reader != nil {
			_ = reader.Close()
		}
		cancel()
		return biz.OpenedPodLog{}, biz.NewError("KUBERNETES_UNAVAILABLE")
	}
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		cancel()
		return biz.OpenedPodLog{}, kubeError(err, false)
	}
	if reader == nil {
		cancel()
		return biz.OpenedPodLog{}, biz.NewError("INTERNAL_ERROR")
	}
	return biz.OpenedPodLog{Reader: &logReadCloser{ReadCloser: reader, cancel: cancel}, PodUID: string(pod.UID), Container: container}, nil
}

type logReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *logReadCloser) Close() error { r.cancel(); return r.ReadCloser.Close() }
