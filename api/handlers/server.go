package handlers

import "github.com/centrys-labs/stagezero/gen"

type Server struct{}

var _ gen.ServerInterface = (*Server)(nil)
