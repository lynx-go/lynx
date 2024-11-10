package jsonapi

import "context"

type EndPoint struct {
	Handler       HandlerFunc
	RequestBinder func() interface{}
}

type HandlerFunc func(ctx context.Context, req interface{}) (interface{}, error)
type Binder func() interface{}

func NewEndPoint(h HandlerFunc, req Binder) *EndPoint {

}
