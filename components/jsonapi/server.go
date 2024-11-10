package jsonapi

import "github.com/gin-gonic/gin"

type Server struct {
}

func NewServer(apis []API[]) *Server {
	r := gin.New()
	r.Handle()
}
