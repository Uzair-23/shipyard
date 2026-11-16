package router

import "context"

type reqKey struct{}

// reqInfo is what the proxy learned about a request before forwarding it.
type reqInfo struct {
	Route    Route
	Identity *Identity // nil when the request carries no valid _sy_app cookie
}

func withInfo(ctx context.Context, i reqInfo) context.Context {
	return context.WithValue(ctx, reqKey{}, i)
}

func infoFrom(ctx context.Context) (reqInfo, bool) {
	i, ok := ctx.Value(reqKey{}).(reqInfo)
	return i, ok
}
