package agentcontext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agenttargetbiz "github.com/tutti-os/tutti/services/tuttid/biz/agenttarget"
	workspacebiz "github.com/tutti-os/tutti/services/tuttid/biz/workspace"
	workspaceagentbiz "github.com/tutti-os/tutti/services/tuttid/biz/workspaceagent"
	workspacedata "github.com/tutti-os/tutti/services/tuttid/data/workspace"
	agentservice "github.com/tutti-os/tutti/services/tuttid/service/agent"
	agentextensionservice "github.com/tutti-os/tutti/services/tuttid/service/agentextension"
	cliservice "github.com/tutti-os/tutti/services/tuttid/service/cli"
	workspaceagentservice "github.com/tutti-os/tutti/services/tuttid/service/workspaceagent"
)

// Unused store operations fail closed via the nil embedded interface. The real
// WorkspaceAgent service owns scope lookup and strict harness validation here.
type directoryStore struct {
	workspaceagentservice.Store
	agents []workspaceagentbiz.Agent
	err    error
}

func (s directoryStore) ListWorkspaceAgents(_ context.Context, workspaceID string) ([]workspaceagentbiz.Agent, error) {
	if s.err != nil {
		return nil, s.err
	}
	var result []workspaceagentbiz.Agent
	for _, agent := range s.agents {
		if agent.WorkspaceID == workspaceID {
			result = append(result, agent)
		}
	}
	return result, nil
}

func (s directoryStore) GetWorkspaceAgent(ctx context.Context, workspaceID, id string) (workspaceagentbiz.Agent, error) {
	agents, err := s.ListWorkspaceAgents(ctx, workspaceID)
	if err != nil {
		return workspaceagentbiz.Agent{}, err
	}
	for _, agent := range agents {
		if agent.ID == id {
			return agent, nil
		}
	}
	return workspaceagentbiz.Agent{}, workspacedata.ErrWorkspaceAgentNotFound
}

type directoryTargets struct{ fakeAgentTargetList }

func (d directoryTargets) GetAgentTarget(_ context.Context, id string) (agenttargetbiz.Target, error) {
	for _, target := range d.targets {
		if target.ID == id {
			return target, nil
		}
	}
	return agenttargetbiz.Target{}, workspacedata.ErrAgentTargetNotFound
}

func workspaceDirectoryProvider(store directoryStore, targets []agenttargetbiz.Target) (Provider, *fakeAgentSessions) {
	sessions := &fakeAgentSessions{availability: []agentservice.ProviderAvailability{availableProvider("codex")}}
	directory := directoryTargets{fakeAgentTargetList{targets: targets}}
	provider := NewProviderWithAgentTargets(
		fakeWorkspaceCatalog{startup: workspacebiz.Summary{ID: "ws-a"}}, sessions, nil, directory,
	).WithWorkspaceAgents(&workspaceagentservice.Service{Store: store, Targets: directory})
	return provider, sessions
}

func TestWorkspaceAgentDirectoryPreservesIdentityAndScope(t *testing.T) {
	store := directoryStore{agents: []workspaceagentbiz.Agent{
		{ID: "workspace-agent:review", WorkspaceID: "ws-a", Name: "Reviewer", HarnessAgentTargetID: "local:codex", Instructions: "private instructions"},
		{ID: "workspace-agent:docs", WorkspaceID: "ws-a", Name: "Writer", HarnessAgentTargetID: "local:codex"},
		{ID: "workspace-agent:other", WorkspaceID: "ws-b", Name: "Other", HarnessAgentTargetID: "local:codex"},
	}}
	provider, _ := workspaceDirectoryProvider(store, agenttargetbiz.DefaultSystemTargets(1))
	for _, workspaceID := range []string{"", "ws-a", "ws-b"} {
		t.Run("list/"+workspaceID, func(t *testing.T) {
			output, err := provider.newAgentsCommand().Handler(context.Background(), cliservice.InvokeRequest{
				Context: cliservice.InvokeContext{WorkspaceID: workspaceID}, OutputMode: cliservice.OutputModeJSON,
			})
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]map[string]any{}
			for _, value := range output.Value["agents"].([]any) {
				item := value.(map[string]any)
				byID[item["id"].(string)] = item
			}
			if byID["local:codex"] == nil {
				t.Fatal("global agent disappeared")
			}
			for _, agent := range store.agents {
				item, found := byID[agent.ID]
				if found != (workspaceID == agent.WorkspaceID) {
					t.Fatalf("scope %q: agent %s found=%v", workspaceID, agent.ID, found)
				}
				if found && (item["name"] != agent.Name || item["provider"] != "codex" || item["availability"].(map[string]any)["status"] != "available") {
					t.Fatalf("custom agent = %#v", item)
				}
			}
			if strings.HasPrefix(output.Value["defaultAgentTargetId"].(string), "workspace-agent:") {
				t.Fatal("custom agent replaced global default")
			}
			encoded, err := json.Marshal(output.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private instructions") {
				t.Fatal("directory exposed private configuration")
			}
		})
	}
	for _, name := range []string{"list", "composer-options", "start", "skill-bundle"} {
		for _, workspaceID := range []string{"ws-a", "ws-b"} {
			t.Run(name+"/"+workspaceID, func(t *testing.T) {
				provider, sessions := workspaceDirectoryProvider(store, agenttargetbiz.DefaultSystemTargets(1))
				commands := map[string]cliservice.Command{"list": provider.newAgentsCommand(), "composer-options": provider.newComposerOptionsCommand(), "start": provider.newStartCommand(), "skill-bundle": provider.newSkillBundleCommand()}
				id := store.agents[0].ID
				output, err := commands[name].Handler(context.Background(), cliservice.InvokeRequest{
					Context: cliservice.InvokeContext{WorkspaceID: workspaceID}, Input: map[string]any{"agent-id": id, "prompt": "Review"}, OutputMode: cliservice.OutputModeJSON,
				})
				if workspaceID != "ws-a" {
					if err == nil {
						t.Fatal("accepted another workspace's agent")
					}
					if sessions.createCallCount != 0 || sessions.composerInput.AgentTargetID != "" || sessions.skillBundleIn.AgentTargetID != "" {
						t.Fatal("rejected agent reached session service")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				switch name {
				case "list":
					items := output.Value["agents"].([]any)
					if len(items) != 1 || items[0].(map[string]any)["id"] != id {
						t.Fatalf("filtered list = %#v", items)
					}
				case "composer-options":
					if sessions.composerInput.AgentTargetID != id || sessions.composerInput.WorkspaceID != workspaceID || sessions.composerInput.Provider != "codex" {
						t.Fatalf("composer = %#v", sessions.composerInput)
					}
				case "start":
					if sessions.createInput.AgentTargetID != id || sessions.workspaceID != workspaceID || sessions.createInput.Provider != "codex" {
						t.Fatalf("start = %#v", sessions.createInput)
					}
				case "skill-bundle":
					if sessions.skillBundleIn.AgentTargetID != id || sessions.workspaceID != workspaceID {
						t.Fatalf("bundle = %#v", sessions.skillBundleIn)
					}
				}
			})
		}
	}
	// Two workspace roles sharing Codex must not make deprecated --provider ambiguous.
	_, err := provider.newComposerOptionsCommand().Handler(context.Background(), cliservice.InvokeRequest{
		Context: cliservice.InvokeContext{WorkspaceID: "ws-a"}, Input: map[string]any{"provider": "codex"}, OutputMode: cliservice.OutputModeJSON,
	})
	if err != nil {
		t.Fatalf("legacy provider: %v", err)
	}
}

func TestWorkspaceAgentDirectoryListsUnavailableConfigurationAndPropagatesStorageFailure(t *testing.T) {
	store := directoryStore{agents: []workspaceagentbiz.Agent{{ID: "workspace-agent:broken", WorkspaceID: "ws-a", Name: "Broken", HarnessAgentTargetID: "local:missing"}}}
	provider, sessions := workspaceDirectoryProvider(store, agenttargetbiz.DefaultSystemTargets(1))
	request := cliservice.InvokeRequest{Context: cliservice.InvokeContext{WorkspaceID: "ws-a"}, Input: map[string]any{"agent-id": store.agents[0].ID, "prompt": "Review"}, OutputMode: cliservice.OutputModeJSON}
	output, err := provider.newAgentsCommand().Handler(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	item := output.Value["agents"].([]any)[0].(map[string]any)
	if item["availability"].(map[string]any)["reasonCode"] != "workspace_agent_harness_unavailable" {
		t.Fatalf("availability = %#v", item)
	}
	if _, err := provider.newStartCommand().Handler(context.Background(), request); !errors.Is(err, workspaceagentservice.ErrHarnessUnavailable) {
		t.Fatalf("start error = %v", err)
	}
	if sessions.createCallCount != 0 {
		t.Fatal("broken agent reached session creation")
	}
	store.err = errors.New("storage offline")
	provider, _ = workspaceDirectoryProvider(store, agenttargetbiz.DefaultSystemTargets(1))
	if _, err := provider.newAgentsCommand().Handler(context.Background(), request); !errors.Is(err, store.err) {
		t.Fatalf("storage error = %v", err)
	}
}

func TestWorkspaceAgentDirectoryUsesHarnessExtensionSetup(t *testing.T) {
	launchRef, err := agenttargetbiz.CanonicalLaunchRefJSON("acp:gemini", agenttargetbiz.LaunchRef{Type: agenttargetbiz.LaunchRefTypeAgentExtension, ExtensionInstallationID: "gemini@1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	target := agenttargetbiz.Target{ID: "extension:gemini", Name: "Gemini", Provider: "acp:gemini", Enabled: true, Source: agenttargetbiz.SourceSystem, LaunchRefJSON: launchRef, AvailabilityStatus: "ready"}
	store := directoryStore{agents: []workspaceagentbiz.Agent{{ID: "workspace-agent:extension", WorkspaceID: "ws-a", Name: "Reviewer", HarnessAgentTargetID: target.ID}}}
	provider, sessions := workspaceDirectoryProvider(store, []agenttargetbiz.Target{target})
	sessions.availabilityErr = errors.New("extension must not use provider probe")
	setup := &fakeAgentTargetSetupReader{snapshots: map[string]agentextensionservice.SetupSnapshot{target.ID: {Status: agentextensionservice.SetupAuthRequired}}}
	provider = provider.WithAgentTargetSetup(setup)
	output, err := provider.newAgentsCommand().Handler(context.Background(), cliservice.InvokeRequest{
		Context: cliservice.InvokeContext{WorkspaceID: "ws-a"}, Input: map[string]any{"agent-id": store.agents[0].ID}, OutputMode: cliservice.OutputModeJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	item := output.Value["agents"].([]any)[0].(map[string]any)
	if item["id"] != store.agents[0].ID || item["availability"].(map[string]any)["reasonCode"] != "auth_required" {
		t.Fatalf("extension role = %#v", item)
	}
	if setup.callCount(target.ID) != 1 || setup.callCount(store.agents[0].ID) != 0 {
		t.Fatal("setup did not use the real harness identity")
	}
}
