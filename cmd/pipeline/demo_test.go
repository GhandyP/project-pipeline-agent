package main

import (
	"testing"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

func TestConfiguredProjectOwnerDoesNotFallBackToTeamOrder(t *testing.T) {
	snapshot := domain.ProjectSnapshot{
		OwnerID: "u-sofia",
		Team: []domain.Owner{
			{ID: "u-ana", Name: "Ana Ruiz"},
			{ID: "u-sofia", Name: "Sofia Marin"},
		},
	}

	got, ok := configuredProjectOwner(snapshot)
	if !ok || got != "Sofia Marin" {
		t.Fatalf("configuredProjectOwner = %q, %t; want Sofia Marin, true", got, ok)
	}
}

func TestConfiguredProjectOwnerFailsClosedWithoutRosterBackedOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ownerID string
	}{
		{name: "missing", ownerID: ""},
		{name: "not on roster", ownerID: "u-absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := domain.ProjectSnapshot{
				OwnerID: tc.ownerID,
				Team:    []domain.Owner{{ID: "u-ana", Name: "Ana Ruiz"}},
			}
			if got, ok := configuredProjectOwner(snapshot); ok || got != "" {
				t.Fatalf("configuredProjectOwner = %q, %t; want empty, false", got, ok)
			}
		})
	}
}
