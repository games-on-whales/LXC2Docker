package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
)

func TestUserDefinedNetworksPersistForPortainer(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		execs:      newExecStore(),
		events:     newEventBroker(),
	}

	body := []byte(`{
		"Name":"frontend",
		"Driver":"bridge",
		"Internal":true,
		"Attachable":true,
		"EnableIPv6":true,
		"Labels":{"com.docker.compose.project":"demo"},
		"Options":{"com.docker.network.bridge.name":"gowbr1"},
		"IPAM":{"Driver":"default","Config":[{"Subnet":"10.42.0.0/24","Gateway":"10.42.0.1"}]}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/networks/create", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.createNetwork(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected create response to include network id")
	}

	rec := st.GetNetwork("frontend")
	if rec == nil {
		t.Fatal("expected created network to persist in store")
	}
	if rec.Subnet != "10.42.0.0/24" || rec.Gateway != "10.42.0.1" {
		t.Fatalf("expected ipam to persist, got subnet=%q gateway=%q", rec.Subnet, rec.Gateway)
	}
	if !rec.Internal || !rec.Attachable || !rec.EnableIPv6 {
		t.Fatalf("expected boolean network flags to persist, got %#v", rec)
	}

	listRR := httptest.NewRecorder()
	h.listNetworks(listRR, httptest.NewRequest(http.MethodGet, "/networks", nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("expected 200 from list, got %d body=%s", listRR.Code, listRR.Body.String())
	}
	var listed []map[string]any
	if err := json.NewDecoder(listRR.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	found := false
	for _, n := range listed {
		if n["Name"] == "frontend" {
			found = true
			if n["Driver"] != "bridge" {
				t.Fatalf("expected bridge driver, got %#v", n["Driver"])
			}
			if n["Internal"] != true || n["Attachable"] != true || n["EnableIPv6"] != true {
				t.Fatalf("expected user-defined network metadata in list response, got %#v", n)
			}
		}
	}
	if !found {
		t.Fatalf("expected frontend network in list response: %#v", listed)
	}

	inspectReq := httptest.NewRequest(http.MethodGet, "/networks/"+created.ID, nil)
	inspectReq = mux.SetURLVars(inspectReq, map[string]string{"id": created.ID})
	inspectRR := httptest.NewRecorder()
	h.inspectNetwork(inspectRR, inspectReq)
	if inspectRR.Code != http.StatusOK {
		t.Fatalf("expected 200 from inspect, got %d body=%s", inspectRR.Code, inspectRR.Body.String())
	}
	var inspected map[string]any
	if err := json.NewDecoder(inspectRR.Body).Decode(&inspected); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	if inspected["Name"] != "frontend" {
		t.Fatalf("expected inspect response to resolve stored network, got %#v", inspected)
	}
}

func TestNetworkConnectDisconnectAndRemovePersistAttachments(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.AddNetwork(&store.NetworkRecord{
		ID:         "net-frontend",
		Name:       "frontend",
		Driver:     "bridge",
		Scope:      "local",
		CreatedAt:  mustParseTime(t, "2026-01-01T00:00:00Z"),
		Attachable: true,
		Gateway:    "10.42.0.1",
		Subnet:     "10.42.0.0/24",
	}); err != nil {
		t.Fatalf("add network: %v", err)
	}
	const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if err := st.AddContainer(&store.ContainerRecord{
		ID:        containerID,
		Name:      "web",
		Image:     "docker.io/library/nginx:latest",
		IPAddress: "10.100.0.10",
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		execs:      newExecStore(),
		events:     newEventBroker(),
	}

	connectBody := []byte(`{
		"Container":"` + containerID + `",
		"EndpointConfig":{
			"Aliases":["web","frontend-web"],
			"MacAddress":"02:42:ac:11:00:02",
			"DriverOpts":{"com.example.mode":"fast"},
			"IPAMConfig":{"IPv4Address":"10.42.0.25"}
		}
	}`)
	connectReq := httptest.NewRequest(http.MethodPost, "/networks/frontend/connect", bytes.NewReader(connectBody))
	connectReq = mux.SetURLVars(connectReq, map[string]string{"id": "frontend"})
	connectRR := httptest.NewRecorder()
	h.connectNetwork(connectRR, connectReq)
	if connectRR.Code != http.StatusOK {
		t.Fatalf("expected 200 from connect, got %d body=%s", connectRR.Code, connectRR.Body.String())
	}

	rec := st.GetContainer(containerID)
	attached, ok := rec.Networks["frontend"]
	if !ok {
		t.Fatalf("expected frontend attachment to persist, got %#v", rec.Networks)
	}
	if attached.NetworkID != "net-frontend" || attached.IPAddress != "10.42.0.25" {
		t.Fatalf("expected persisted network attachment metadata, got %#v", attached)
	}
	if len(attached.Aliases) != 2 || attached.DriverOpts["com.example.mode"] != "fast" {
		t.Fatalf("expected endpoint config to persist, got %#v", attached)
	}

	inspectReq := httptest.NewRequest(http.MethodGet, "/networks/frontend", nil)
	inspectReq = mux.SetURLVars(inspectReq, map[string]string{"id": "frontend"})
	inspectRR := httptest.NewRecorder()
	h.inspectNetwork(inspectRR, inspectReq)
	if inspectRR.Code != http.StatusOK {
		t.Fatalf("expected 200 from inspect, got %d body=%s", inspectRR.Code, inspectRR.Body.String())
	}
	var inspected map[string]any
	if err := json.NewDecoder(inspectRR.Body).Decode(&inspected); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	containers, ok := inspected["Containers"].(map[string]any)
	if !ok || containers[containerID] == nil {
		t.Fatalf("expected inspect response to include attached container, got %#v", inspected["Containers"])
	}

	removeReq := httptest.NewRequest(http.MethodDelete, "/networks/frontend", nil)
	removeReq = mux.SetURLVars(removeReq, map[string]string{"id": "frontend"})
	removeRR := httptest.NewRecorder()
	h.removeNetwork(removeRR, removeReq)
	if removeRR.Code != http.StatusConflict {
		t.Fatalf("expected 409 removing attached network, got %d body=%s", removeRR.Code, removeRR.Body.String())
	}

	disconnectBody := []byte(`{"Container":"` + containerID + `"}`)
	disconnectReq := httptest.NewRequest(http.MethodPost, "/networks/frontend/disconnect", bytes.NewReader(disconnectBody))
	disconnectReq = mux.SetURLVars(disconnectReq, map[string]string{"id": "frontend"})
	disconnectRR := httptest.NewRecorder()
	h.disconnectNetwork(disconnectRR, disconnectReq)
	if disconnectRR.Code != http.StatusOK {
		t.Fatalf("expected 200 from disconnect, got %d body=%s", disconnectRR.Code, disconnectRR.Body.String())
	}
	rec = st.GetContainer(containerID)
	if _, ok := rec.Networks["frontend"]; ok {
		t.Fatalf("expected frontend attachment to be removed, got %#v", rec.Networks)
	}

	removeRR = httptest.NewRecorder()
	h.removeNetwork(removeRR, removeReq)
	if removeRR.Code != http.StatusNoContent {
		t.Fatalf("expected 204 removing detached network, got %d body=%s", removeRR.Code, removeRR.Body.String())
	}
	if st.GetNetwork("frontend") != nil {
		t.Fatal("expected removed network to disappear from store")
	}
}

func TestContainerNetworkViewsUsePersistedAttachments(t *testing.T) {
	t.Parallel()

	rec := &store.ContainerRecord{
		ID:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
		Name:      "web",
		IPAddress: "10.100.0.10",
		Networks: map[string]store.NetworkAttachment{
			"gow": {
				NetworkID:  "gow",
				IPAddress:  "10.100.0.10",
				Gateway:    "10.100.0.1",
				EndpointID: "ep-gow",
			},
			"frontend": {
				NetworkID:  "net-frontend",
				IPAddress:  "10.42.0.25",
				Gateway:    "10.42.0.1",
				EndpointID: "ep-frontend",
				Aliases:    []string{"web", "frontend-web"},
			},
		},
	}

	if got := networkModeFor(rec); got != "frontend" {
		t.Fatalf("expected custom network to surface as network mode, got %q", got)
	}
	if !containerOnNetwork(rec, []string{"frontend"}) {
		t.Fatal("expected containerOnNetwork to match attached network name")
	}
	if !containerOnNetwork(rec, []string{"net-frontend"}) {
		t.Fatal("expected containerOnNetwork to match attached network id")
	}

	endpoints := networkSettingsFor(rec)
	frontend, ok := endpoints["frontend"]
	if !ok {
		t.Fatalf("expected frontend endpoint settings, got %#v", endpoints)
	}
	if frontend.NetworkID != "net-frontend" || frontend.IPAddress != "10.42.0.25" {
		t.Fatalf("expected persisted endpoint details in inspect/list views, got %#v", frontend)
	}
	if len(frontend.Aliases) != 2 {
		t.Fatalf("expected aliases to round-trip through network settings, got %#v", frontend)
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}
