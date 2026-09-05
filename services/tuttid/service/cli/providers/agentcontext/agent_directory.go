package agentcontext

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agenttargetbiz "github.com/tutti-os/tutti/services/tuttid/biz/agenttarget"
	workspaceagentbiz "github.com/tutti-os/tutti/services/tuttid/biz/workspaceagent"
	agentservice "github.com/tutti-os/tutti/services/tuttid/service/agent"
	cliservice "github.com/tutti-os/tutti/services/tuttid/service/cli"
	workspaceagentservice "github.com/tutti-os/tutti/services/tuttid/service/workspaceagent"
)

// Selection identity is distinct from the real target that supplies the runtime.
type agentSelection struct {
	ID            string
	Name          string
	Provider      string
	Source        string
	HarnessTarget agenttargetbiz.Target
}

// convert global agenttargetbiz.Target into agentSelection
func globalAgentSelection(target agenttargetbiz.Target) agentSelection {
	return agentSelection{ID: target.ID, Name: target.Name, Provider: target.Provider, Source: "global", HarnessTarget: target}
}

// convert workspace agenttargetbiz.Target into agentSelection
func resolvedAgentSelection(resolved workspaceagentbiz.Resolved) agentSelection {
	return agentSelection{
		ID: resolved.Agent.ID, Name: resolved.Agent.Name, Provider: resolved.HarnessTarget.Provider,
		Source: "workspace-agent", HarnessTarget: resolved.HarnessTarget,
	}
}

func (p Provider) resolveAgent(ctx context.Context, workspaceID, agentID string) (agentSelection, error) {
	agentID = strings.TrimSpace(agentID)
	// without workspace prefix
	if !strings.HasPrefix(agentID, workspaceagentbiz.IDPrefix) {
		target, err := p.resolveEnabledAgentTarget(ctx, agentID)
		return globalAgentSelection(target), err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return agentSelection{}, fmt.Errorf("%w:  missing workspaceID for resolving workspaceAgent", cliservice.ErrInvalidInput)
	}
	if p.workspaceAgents == nil {
		return agentSelection{}, cliservice.ServiceUnavailableError(
			"workspace_agent_directory_unavailable",
			errors.New("workspace agent directory is unavailable"),
		)
	}
	resolved, err := p.workspaceAgents.Resolve(ctx, workspaceID, agentID)
	if err != nil {
		return agentSelection{}, err
	}
	return resolvedAgentSelection(resolved), nil
}

// return value can be Null
func (p Provider) workspaceAgentCatalog(ctx context.Context, workspaceID string) ([]agentCatalogItem, error) {
	if strings.TrimSpace(workspaceID) == "" || p.workspaceAgents == nil {
		return nil, nil
	}
	views, err := p.workspaceAgents.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	items := make([]agentCatalogItem, 0, len(views))
	for _, view := range views {
		resolved, err := p.workspaceAgents.Resolve(ctx, workspaceID, view.Agent.ID)
		if err == nil {
			items = append(items, agentCatalogItem{agentSelection: resolvedAgentSelection(resolved)})
			continue
		}
		availability, known := workspaceAgentUnavailable(view.Harness.Provider, err)
		if !known {
			return nil, err
		}
		items = append(items, agentCatalogItem{
			agentSelection: agentSelection{ID: view.Agent.ID, Name: view.Agent.Name, Provider: view.Harness.Provider, Source: "workspace-agent"},
			Availability:   availability,
		})
	}
	return items, nil
}

func workspaceAgentUnavailable(provider string, err error) (agentservice.ProviderAvailability, bool) {
	// Only configuration failures become listable unavailable entries. Do not
	// hide storage failures or serialize runtime configuration/credential errors.
	for _, candidate := range []struct {
		err  error
		code string
	}{
		{workspaceagentservice.ErrHarnessUnavailable, "workspace_agent_harness_unavailable"},
		{workspaceagentservice.ErrHarnessDisabled, "workspace_agent_harness_disabled"},
		{workspaceagentservice.ErrPlanNotUsable, "workspace_agent_plan_not_usable"},
		{workspaceagentservice.ErrModelNotInPlan, "workspace_agent_model_not_in_plan"},
		{workspaceagentservice.ErrHarnessPlanProtocolMismatch, "workspace_agent_harness_plan_protocol_mismatch"},
	} {
		if errors.Is(err, candidate.err) {
			return agentservice.ProviderAvailability{
				Provider: provider, Status: agentservice.ProviderAvailabilityUnavailable,
				LastError: &agentservice.ProviderAvailabilityError{Code: candidate.code, Message: candidate.err.Error()},
			}, true
		}
	}
	return agentservice.ProviderAvailability{}, false
}
