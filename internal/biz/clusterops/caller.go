package clusterops

import "context"

type Caller struct {
	Actor, DirectCaller, RequestID string
	Platform                       bool
}
type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
func RequirePlatform(ctx context.Context) error {
	c, ok := CallerFromContext(ctx)
	if !ok || !c.Platform || c.Actor == "" || c.DirectCaller != "ani-governance" || c.RequestID == "" {
		return NewError("PLATFORM_SCOPE_REQUIRED")
	}
	return ctx.Err()
}
