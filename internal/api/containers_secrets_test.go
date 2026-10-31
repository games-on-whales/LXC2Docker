package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Validation failures must not write credentials to the daemon log. Cmd,
// Entrypoint and labels can carry secrets too, so logging the full request
// with only Env redacted would still expose sensitive user data.
func TestCreateContainerDoesNotLogRequestSecrets(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	request := httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(`{
  "Env":["PBS_ADMIN_PASSWORD=private-environment-value"],
  "Cmd":["private-command-value"],
  "Entrypoint":["private-entrypoint-value"],
  "Labels":{"credential":"private-label-value"}
 }`))
	response := httptest.NewRecorder()
	(&Handler{}).createContainer(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
	for _, secret := range []string{"private-environment-value", "private-command-value", "private-entrypoint-value", "private-label-value"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("container request secret leaked to daemon log")
		}
	}
}
