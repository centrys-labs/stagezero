package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/centrys-labs/stagezero/db"
)

func TestMonitoringChangeErrorDoesNotExposeSecrets(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{errors.New("ssh command failed: token=private-credential"), http.StatusServiceUnavailable},
		{errors.Join(db.ErrMonitoringBusy, errors.New("private-credential")), http.StatusConflict},
	} {
		response := httptest.NewRecorder()
		monitoringChangeError(response, tc.err)
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private-credential") {
			t.Fatalf("status=%d response=%s", response.Code, response.Body.String())
		}
	}
}
