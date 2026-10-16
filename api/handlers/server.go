package handlers

import (
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/source"
)

type Server struct {
	// SourceAnalyzer is replaceable in handler tests. Requests always run a
	// server-side analysis, never one supplied by the client.
	SourceAnalyzer source.Analyzer
}

var _ gen.ServerInterface = (*Server)(nil)
