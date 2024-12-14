package grpc

import (
	"context"
	"github.com/lynx-go/lynx"
)

type Server struct {
}

func (s *Server) Name() string {
	//TODO implement me
	panic("implement me")
}

func (s *Server) Start(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

func (s *Server) Stop(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

var _ lynx.Service = new(Server)
