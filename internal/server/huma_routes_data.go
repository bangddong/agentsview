package server

import (
	"context"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerDataRoutes() {
	group := newRouteGroup(s.api, "/api/v1/data", "Data")
	get(s, group, "/projects", "Get project inventory", s.humaDataProjects)
	get(s, group, "/project-rules", "List project rules", s.humaDataProjectRules)
	get(s, group, "/project-reclassification/candidates",
		"List archive-wide reclassification candidates",
		s.humaDataCandidates)
}

type dataProjectRulesInput struct {
	Machine string `query:"machine" doc:"Machine to list rules for"`
}

type dataCandidatesInput struct {
	ProjectLabel string `query:"project_label" doc:"Project display label"`
	ProjectKey   string `query:"project_key" doc:"Opaque project identity key"`
}

type dataCandidatesResponse struct {
	Candidates []db.WorktreeReclassificationCandidate `json:"candidates"`
}

func (s *Server) humaDataProjects(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[db.ProjectInventory], error) {
	inv, err := s.db.GetProjectInventory(ctx)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("get project inventory error", err)
	}
	return &jsonOutput[db.ProjectInventory]{Body: inv}, nil
}

func (s *Server) humaDataProjectRules(
	ctx context.Context, in *dataProjectRulesInput,
) (*jsonOutput[db.ProjectRules], error) {
	rules, err := s.db.ListProjectRules(ctx, in.Machine)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("list project rules error", err)
	}
	return &jsonOutput[db.ProjectRules]{Body: rules}, nil
}

func (s *Server) humaDataCandidates(
	ctx context.Context, in *dataCandidatesInput,
) (*jsonOutput[dataCandidatesResponse], error) {
	if strings.TrimSpace(in.ProjectKey) == "" {
		return nil, apiError(http.StatusBadRequest, "project_key is required")
	}
	candidates, err := s.db.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: in.ProjectLabel,
			ProjectKey:   in.ProjectKey,
		})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("list archive-wide reclassification candidates error", err)
	}
	return &jsonOutput[dataCandidatesResponse]{
		Body: dataCandidatesResponse{Candidates: candidates},
	}, nil
}
