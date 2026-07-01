package api

import (
	"reflect"
	"testing"
)

func TestExecStoreIDsForContainer(t *testing.T) {
	t.Parallel()
	s := newExecStore()
	s.add(&execRecord{ID: "e2", ContainerID: "c1"})
	s.add(&execRecord{ID: "e1", ContainerID: "c1"})
	s.add(&execRecord{ID: "e3", ContainerID: "c2"})

	// Sorted, only the requested container's execs.
	if got := s.idsForContainer("c1"); !reflect.DeepEqual(got, []string{"e1", "e2"}) {
		t.Errorf("idsForContainer(c1) = %v, want [e1 e2]", got)
	}
	if got := s.idsForContainer("c2"); !reflect.DeepEqual(got, []string{"e3"}) {
		t.Errorf("idsForContainer(c2) = %v, want [e3]", got)
	}
	// No execs → nil, so ContainerJSON.ExecIDs marshals to null like Docker.
	if got := s.idsForContainer("c3"); got != nil {
		t.Errorf("idsForContainer(c3) = %v, want nil", got)
	}
}
