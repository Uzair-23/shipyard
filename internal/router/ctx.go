package router

import "context"

type routeKey struct{}

func withRoute(ctx context.Context, r Route) context.Context {
	return context.WithValue(ctx, routeKey{}, r)
}

func routeFrom(ctx context.Context) (Route, bool) {
	r, ok := ctx.Value(routeKey{}).(Route)
	return r, ok
}
