package handlers

import (
	"errors"
	"github.com/centrys-labs/stagezero/telemetry"
	"net/http"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/respond"
	"github.com/centrys-labs/stagezero/ssh"
	"github.com/jackc/pgx/v5"
)

// The proxy is one container per server with a fixed name, set by proxy.EnsureProxy.
const proxyContainerName = "stagezero-proxy"

// GetProxyLogs streams the reverse proxy's output for one server over SSE.
//
// Per server, not per service, because that is what the proxy is: one Traefik
// answers for every service on the machine, and splitting its log per service
// would mean one SSH session each for the same lines. Which service a request
// hit is in the line itself, so the panel's filter box is the split.
func (s *Server) GetProxyLogs(w http.ResponseWriter, r *http.Request, id string, params gen.GetProxyLogsParams) {
	sc, _ := auth.GetSessionContext(r)

	sinceFlag, ok := logSinceFlag((*string)(params.Since))
	if !ok {
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: "unknown time range: " + string(*params.Since)})
		return
	}

	srv, err := db.GetServerWithKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respond.JSON(w, http.StatusNotFound, gen.ErrorResponse{Error: "server not found"})
		} else {
			telemetry.Printf("GetServerWithKey id=%s error: %v", id, err)
			respond.JSON(w, http.StatusInternalServerError, gen.ErrorResponse{Error: "failed to look up server"})
		}
		return
	}

	// 404 rather than 403 across workspaces, so the response does not confirm
	// that the server exists.
	if srv.WorkspaceID != sc.WorkspaceID {
		respond.JSON(w, http.StatusNotFound, gen.ErrorResponse{Error: "server not found"})
		return
	}

	// No proxy_status gate: a proxy that failed to come up is exactly when its
	// log is worth reading, and `docker logs` on a stopped container still has
	// the output that explains why it stopped.
	streamContainerLogs(w, r, ssh.ServerConfig{
		Host:       srv.Host,
		Port:       int(srv.Port),
		User:       srv.SSHUser,
		PrivateKey: srv.PrivateKey,
	}, proxyContainerName, sinceFlag, "proxy logs server="+id)
}
