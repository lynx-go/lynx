package jsonapi

import "context"

type API[IN any, OUT any] interface {
	Route() string
	HandleFunc(ctx context.Context, in IN) (OUT, error)
}
