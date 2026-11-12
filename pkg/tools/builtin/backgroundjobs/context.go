package backgroundjobs

import "context"

type noBackgroundJobsKey struct{}

// WithoutBackgroundJobs prevents job launches in hosts that stop their toolsets after each call.
func WithoutBackgroundJobs(ctx context.Context) context.Context {
	return context.WithValue(ctx, noBackgroundJobsKey{}, true)
}
