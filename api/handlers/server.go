package handlers

import (
	"sync"

	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/source"
)

type Server struct {
	// SourceAnalyzer is replaceable in handler tests. Requests always run a
	// server-side analysis, never one supplied by the client.
	SourceAnalyzer source.Analyzer
	SourceLimits   source.AnalysisLimits
	sourceGateOnce sync.Once
	sourceGate     *source.AnalysisGate
}

var _ gen.ServerInterface = (*Server)(nil)
