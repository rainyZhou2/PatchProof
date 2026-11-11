package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/patchproof/patchproof/internal/domain"
)

type jobPhase int

const (
	jobPending jobPhase = iota
	jobActive
	jobSucceeded
	jobFailed
)

func jobStateOf(job *batchv1.Job) jobPhase {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return jobSucceeded
		case batchv1.JobFailed:
			return jobFailed
		}
	}
	switch {
	case job.Status.Succeeded > 0:
		return jobSucceeded
	case job.Status.Failed > 0:
		return jobFailed
	case job.Status.Active > 0:
		return jobActive
	default:
		return jobPending
	}
}

func conditionMessage(job *batchv1.Job, complete bool) string {
	want := batchv1.JobFailed
	if complete {
		want = batchv1.JobComplete
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type != want || condition.Status != corev1.ConditionTrue {
			continue
		}
		return truncate(strings.TrimSpace(condition.Reason+" "+condition.Message), maxMessageLength)
	}
	return ""
}

// failureFromPods inspects pods for terminal or stuck conditions and attaches
// the most recent event message when available.
func (e *Executor) failureFromPods(ctx context.Context, namespace string, pods []corev1.Pod) (domain.ErrorClass, string, bool) {
	for i := range pods {
		pod := &pods[i]
		class, reason, ok := podFailure(pod)
		if !ok {
			continue
		}
		message := strings.TrimSpace(pod.Status.Message)
		if eventMessage := e.latestEventMessage(ctx, namespace, pod.Name); eventMessage != "" {
			message = eventMessage
		}
		if message == "" {
			message = "pod " + pod.Name + " did not complete"
		}
		return class, truncate(fmt.Sprintf("%s: %s", reason, message), maxMessageLength), true
	}
	return "", "", false
}

func podFailure(pod *corev1.Pod) (domain.ErrorClass, string, bool) {
	switch pod.Status.Reason {
	case "Evicted":
		return domain.ErrPodEvicted, "pod evicted", true
	case "NodeLost", "NodeShutdown":
		return domain.ErrNodeLost, "node lost", true
	}
	if reason := oomKilledContainer(pod); reason != "" {
		return domain.ErrExecutorError, "container " + reason + " was OOMKilled", true
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if class, reason, ok := waitingFailure(status); ok {
			return class, reason, true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if class, reason, ok := waitingFailure(status); ok {
			return class, reason, true
		}
	}
	return "", "", false
}

func waitingFailure(status corev1.ContainerStatus) (domain.ErrorClass, string, bool) {
	if status.State.Waiting == nil {
		return "", "", false
	}
	switch status.State.Waiting.Reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return domain.ErrImagePullFailed, "container " + status.Name + " image unavailable: " + status.State.Waiting.Reason, true
	case "CreateContainerConfigError", "CreateContainerError":
		return domain.ErrExecutorError, "container " + status.Name + " could not start: " + status.State.Waiting.Reason, true
	default:
		return "", "", false
	}
}

func oomKilledContainer(pod *corev1.Pod) string {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.Reason == "OOMKilled" {
			return status.Name
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.Reason == "OOMKilled" {
			return status.Name
		}
	}
	return ""
}

func classifyJobFailure(job *batchv1.Job, pods []corev1.Pod) (domain.ErrorClass, string) {
	reason, message := "", ""
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			reason, message = condition.Reason, condition.Message
			break
		}
	}
	if message == "" {
		message = "job failed"
	}
	class := domain.ErrExecutorError
	if reason == "DeadlineExceeded" {
		if neverStarted(pods) {
			message = "pod stuck initializing: " + message
		} else {
			class = domain.ErrTimeout
		}
	}
	detail := strings.TrimSpace(reason + " " + message)
	if detail == "" {
		detail = "job failed"
	}
	return class, truncate(detail, maxMessageLength)
}

// neverStarted reports whether at least one pod exists and no container ever
// started, which distinguishes a stuck sandbox from an execution timeout.
func neverStarted(pods []corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for i := range pods {
		pod := &pods[i]
		for _, status := range pod.Status.InitContainerStatuses {
			if status.State.Running != nil || status.State.Terminated != nil {
				return false
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil || status.State.Terminated != nil {
				return false
			}
			if status.Started != nil && *status.Started {
				return false
			}
		}
	}
	return true
}

func (e *Executor) latestEventMessage(ctx context.Context, namespace, podName string) string {
	events, err := e.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + podName,
	})
	if err != nil {
		return ""
	}
	var latest *corev1.Event
	for i := range events.Items {
		event := &events.Items[i]
		if event.InvolvedObject.Name != podName || event.Message == "" {
			continue
		}
		if latest == nil || eventTimestamp(event).After(eventTimestamp(latest)) {
			latest = event
		}
	}
	if latest == nil {
		return ""
	}
	message := strings.TrimSpace(latest.Message)
	if latest.Reason != "" {
		message = latest.Reason + ": " + message
	}
	return truncate(collapseWhitespace(message), maxMessageLength)
}

func eventTimestamp(event *corev1.Event) time.Time {
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
