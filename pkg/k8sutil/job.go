package k8sutil

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// RunJob runs the provided job and awaits until it finishes or the timeout is reached.
// returns the job's pod logs (indexed by container name) and the state of each of the
// containers (also indexed by container name).
func RunJob(
	ctx context.Context,
	cli kubernetes.Interface,
	logger *log.Logger,
	job *batchv1.Job,
) (map[string][]byte, map[string]corev1.ContainerState, error) {
	job, err := cli.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create job: %w", err)
	}

	defer func() {
		propagation := metav1.DeletePropagationForeground
		delopts := metav1.DeleteOptions{PropagationPolicy: &propagation}
		if err = cli.BatchV1().Jobs(job.Namespace).Delete(context.Background(), job.Name, delopts); err != nil {
			logger.Printf("failed to delete job: %s", err)
		}
	}()

	var hasTimedOut, hasFailed bool
	for {
		var gotJob *batchv1.Job
		if gotJob, err = cli.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err != nil {
			return nil, nil, fmt.Errorf("failed getting job: %w", err)
		}

		if gotJob.Status.Failed > 0 {
			hasFailed = true
			break
		} else if gotJob.Status.Succeeded > 0 {
			break
		}

		time.Sleep(time.Second)
	}

	listOptions := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(job.Spec.Selector.MatchLabels).String(),
	}

	var jobPod corev1.Pod
	if pods, err := cli.CoreV1().Pods(job.Namespace).List(ctx, listOptions); err != nil {
		return nil, nil, fmt.Errorf("failed to list pods for job: %w", err)
	} else if len(pods.Items) == 0 {
		return nil, nil, fmt.Errorf("pod for job not found")
	} else {
		jobPod = pods.Items[0]
	}

	lastContainerStatuses := map[string]corev1.ContainerState{}
	for _, status := range jobPod.Status.ContainerStatuses {
		lastContainerStatuses[status.Name] = status.State
	}

	logs := map[string][]byte{}
	for _, container := range jobPod.Spec.Containers {
		options := &corev1.PodLogOptions{Container: container.Name}
		podlogs, err := cli.CoreV1().Pods(jobPod.Namespace).GetLogs(jobPod.Name, options).Stream(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get pod log stream: %w", err)
		}

		defer func(stream io.ReadCloser) {
			if err := stream.Close(); err != nil {
				logger.Printf("failed to close pod log stream: %s", err)
			}
		}(podlogs)

		output, err := io.ReadAll(podlogs)
		if err != nil {
			return nil, lastContainerStatuses, fmt.Errorf("failed to read pod logs: %w", err)
		}

		logs[container.Name] = output
	}

	if hasTimedOut {
		return logs, lastContainerStatuses, fmt.Errorf("timeout waiting for the pod")
	}

	if hasFailed {
		return logs, lastContainerStatuses, fmt.Errorf("pod failed")
	}

	return logs, lastContainerStatuses, nil
}
