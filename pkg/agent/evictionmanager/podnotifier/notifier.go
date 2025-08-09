package podnotifier

import (
	"context"
	v1 "k8s.io/api/core/v1"
)

// Notifier implements pod notify logic.
type Notifier interface {
	// Name returns name as identifier for a specific Notifier.
	Name() string

	// Notify for given pods and corresponding graceful period seconds.
	Notify(ctx context.Context, pod *v1.Pod, reason, plugin string) error
}
