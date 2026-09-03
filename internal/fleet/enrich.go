package fleet

import (
	"slices"
	"strings"
	"time"
)

// ProjectForObserver replaces the observer's aggregate entries with fresh
// local authority, then derives all observer-relative IDs and permissions.
// The neutral input is copied and never mutated.
func ProjectForObserver(
	aggregate NeutralSnapshot,
	local RawSnapshot,
	observer Observer,
) Snapshot {
	view := withFreshObserverState(aggregate, local, observer)
	if observer.Role == RoleSpoke {
		view = localSpokeProjection(view, string(observer.NodeID))
	}
	active := ActivePlatformHost(view.Projects)
	identity := DefaultIdentity()
	resp := Snapshot{
		ProtocolVersion:       view.ProtocolVersion,
		Generation:            view.Generation,
		PlatformAuthenticated: view.PlatformAuthenticated,
		ActivePlatformHost:    active,
	}
	resp.Hosts = buildHosts(view.Hosts, observer, identity)
	resp.Projects = buildProjects(view.Projects, active, identity)
	resp.ProjectMap = buildProjectMap(resp.Projects)
	worktrees, filteredKeys := buildWorktrees(view.Worktrees, identity)
	resp.Worktrees = worktrees
	resp.Sessions = buildSessions(view.Sessions, filteredKeys, identity)
	resp.Workspaces = buildWorkspaceSummaries(view.Workspaces, view.Hosts, observer.NodeID)
	return resp
}

// localSpokeProjection keeps the fleet host directory available for direct
// navigation while limiting a spoke's workspace surface to state it can act
// on locally. The hub remains the one place that projects the full fleet.
func localSpokeProjection(view NeutralSnapshot, observer string) NeutralSnapshot {
	view.Projects = slices.DeleteFunc(slices.Clone(view.Projects), func(project RawProject) bool {
		return project.HostKey != observer
	})
	view.Worktrees = slices.DeleteFunc(slices.Clone(view.Worktrees), func(worktree RawWorktree) bool {
		return worktree.HostKey != observer
	})
	view.Sessions = slices.DeleteFunc(slices.Clone(view.Sessions), func(session RawSession) bool {
		return session.HostKey != observer
	})
	view.Workspaces = slices.DeleteFunc(slices.Clone(view.Workspaces), func(workspace RawWorkspace) bool {
		return workspace.HostKey != observer
	})
	return view
}

func withFreshObserverState(
	aggregate NeutralSnapshot,
	local RawSnapshot,
	observer Observer,
) NeutralSnapshot {
	view := aggregate
	if view.ProtocolVersion == 0 {
		view.ProtocolVersion = local.ProtocolVersion
	}
	if len(view.Hosts) == 0 {
		view.Generation = local.Generation
		view.PlatformAuthenticated = local.PlatformAuthenticated
	}
	view.Hosts = replaceObserverHost(
		aggregate.Hosts, neutralLocalHost(local, observer.Role), observer.NodeID,
	)
	key := string(observer.NodeID)
	view.Projects = replaceObserverProjects(aggregate.Projects, local.Projects, key)
	view.Worktrees = replaceObserverWorktrees(aggregate.Worktrees, local.Worktrees, key)
	view.Sessions = replaceObserverSessions(aggregate.Sessions, local.Sessions, key)
	view.Workspaces = replaceObserverWorkspaces(aggregate.Workspaces, local.Workspaces, key)
	return view
}

func replaceObserverHost(hosts []NeutralHost, local NeutralHost, observer NodeID) []NeutralHost {
	out := make([]NeutralHost, 0, len(hosts)+1)
	found := false
	for _, host := range hosts {
		if host.NodeID == observer {
			if !found {
				local.NodeID = observer
				out = append(out, local)
				found = true
			}
			continue
		}
		out = append(out, host)
	}
	if !found {
		local.NodeID = observer
		out = append(out, local)
	}
	return out
}

func replaceObserverProjects(all, local []RawProject, observer string) []RawProject {
	out := filterProjectsForOtherNodes(all, observer)
	return append(out, stampedProjects(local, observer)...)
}

func filterProjectsForOtherNodes(all []RawProject, observer string) []RawProject {
	out := make([]RawProject, 0, len(all))
	for _, project := range all {
		if project.HostKey != observer {
			out = append(out, project)
		}
	}
	return out
}

func replaceObserverWorktrees(all, local []RawWorktree, observer string) []RawWorktree {
	provider := make(map[string]RawWorktree)
	out := make([]RawWorktree, 0, len(all)+len(local))
	for _, worktree := range all {
		if worktree.HostKey == observer {
			provider[worktree.ScopedKey] = worktree
			continue
		}
		out = append(out, worktree)
	}
	local = stampedWorktrees(local, observer)
	for index := range local {
		if enriched, ok := provider[local[index].ScopedKey]; ok {
			copyProviderWorktreeFields(&local[index], enriched)
		}
	}
	return append(out, local...)
}

func copyProviderWorktreeFields(target *RawWorktree, source RawWorktree) {
	target.PRState = source.PRState
	target.PRTitle = source.PRTitle
	target.ChecksStatus = source.ChecksStatus
	target.PRReviewDecision = source.PRReviewDecision
	target.PRMergeable = source.PRMergeable
	target.PRAdditions = source.PRAdditions
	target.PRDeletions = source.PRDeletions
	target.PRCommentCount = source.PRCommentCount
	target.PRURL = source.PRURL
	target.PRUpdatedAt = source.PRUpdatedAt
	target.ChecksDetail = source.ChecksDetail
	target.LastPolledAt = source.LastPolledAt
}

func replaceObserverSessions(all, local []RawSession, observer string) []RawSession {
	out := make([]RawSession, 0, len(all)+len(local))
	for _, session := range all {
		if session.HostKey != observer {
			out = append(out, session)
		}
	}
	return append(out, stampedSessions(local, observer)...)
}

func replaceObserverWorkspaces(all, local []RawWorkspace, observer string) []RawWorkspace {
	provider := make(map[string]RawWorkspace)
	out := make([]RawWorkspace, 0, len(all)+len(local))
	for _, workspace := range all {
		if workspace.HostKey == observer {
			provider[workspace.ID] = workspace
			continue
		}
		out = append(out, workspace)
	}
	local = stampedWorkspaces(local, observer)
	for index := range local {
		if enriched, ok := provider[local[index].ID]; ok {
			copyProviderWorkspaceFields(&local[index], enriched)
		}
	}
	return append(out, local...)
}

func copyProviderWorkspaceFields(target *RawWorkspace, source RawWorkspace) {
	target.ItemLastActivityAt = source.ItemLastActivityAt
	target.MRTitle = source.MRTitle
	target.MRState = source.MRState
	target.MRIsDraft = source.MRIsDraft
	target.MRCIStatus = source.MRCIStatus
	target.MRReviewDecision = source.MRReviewDecision
	target.MRAdditions = source.MRAdditions
	target.MRDeletions = source.MRDeletions
}

// MapConnectionState maps internal connection states to the protocol enum
// (connecting, online, degraded, offline). Returns nil for unknown states.
func MapConnectionState(internalState string) *string {
	var mapped string
	switch internalState {
	case "connecting":
		mapped = "connecting"
	case "connected":
		mapped = "online"
	case "probe_failed":
		mapped = "degraded"
	case "disconnected", "error":
		mapped = "offline"
	default:
		return nil
	}
	return &mapped
}

func buildHosts(hosts []NeutralHost, observer Observer, identity Identity) []HostSummary {
	out := make([]HostSummary, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, buildHost(host, observer, identity))
	}
	return out
}

func buildHost(host NeutralHost, observer Observer, identity Identity) HostSummary {
	isSelf := host.NodeID == observer.NodeID
	kind := "remote"
	transport := "http"
	policy := AvailabilityPolicy(RealCapabilityPolicy{})
	if isSelf {
		kind = "self"
		transport = "local"
	} else if observer.Role == RoleSpoke {
		policy = SummaryOnlyPolicy{}
	}
	h := HostSummary{
		ID:                    identity.HostID(string(host.NodeID)),
		ConfigKey:             string(host.NodeID),
		NodeID:                string(host.NodeID),
		Name:                  host.Name,
		Kind:                  kind,
		FederationRole:        host.FederationRole,
		BaseURL:               host.BaseURL,
		Platform:              strings.ToLower(host.Platform),
		Reachable:             host.Reachable,
		PreferredTransport:    transport,
		Capabilities:          host.Capabilities,
		Diagnostics:           []HostDiagnostic{},
		OperationAvailability: map[string]HostOperationAvailability{},
	}
	if host.Hostname != "" {
		h.Hostname = &host.Hostname
	}
	if host.LastSeenAt != "" {
		ts := normalizeDateValue(host.LastSeenAt)
		h.LastSeenAt = &ts
	}
	if host.TmuxLastPolledAt != "" {
		ts := normalizeDateValue(host.TmuxLastPolledAt)
		h.TmuxLastPolledAt = &ts
	}
	h.TmuxProbeError = host.TmuxProbeError
	h.TmuxMetricsError = host.TmuxMetricsError
	diags := tmuxProbeDiagnostics(
		host.TmuxProbeError,
		host.TmuxMetricsError,
	)
	if host.Capabilities != nil {
		diags = append(
			DiagnosticsFromCapabilities(*host.Capabilities),
			diags...,
		)
	}
	if diags == nil {
		diags = []HostDiagnostic{}
	}
	h.Diagnostics = diags
	if host.Capabilities != nil {
		h.OperationAvailability = OperationAvailabilityFromState(
			diags, host.Capabilities.Commands, host.Reachable, policy,
		)
	} else {
		h.OperationAvailability = OperationAvailabilityFromState(
			nil, CommandCapabilities{}, host.Reachable, policy,
		)
	}
	h.Error = host.Error
	if host.Version != "" {
		h.Version = &host.Version
	}
	h.TmuxSessions = tmuxOrEmpty(host.TmuxSessions)
	return h
}

func tmuxProbeDiagnostics(
	probeError string,
	metricsError string,
) []HostDiagnostic {
	var out []HostDiagnostic
	if probeError != "" {
		out = append(out, HostDiagnostic{
			Code:               "tmuxProbeFailed",
			Severity:           "warning",
			Summary:            "Tmux inventory probe failed",
			RecoverySuggestion: "Check that tmux is running and the configured tmux command can list sessions.",
		})
	}
	if metricsError != "" {
		out = append(out, HostDiagnostic{
			Code:               "tmuxMetricsUnavailable",
			Severity:           "warning",
			Summary:            "Tmux process metrics unavailable",
			RecoverySuggestion: "Check that tmux and process table commands are available on the host.",
		})
	}
	return out
}

func buildProjects(raw []RawProject, activeHost *string, identity Identity) []ProjectSummary {
	out := make([]ProjectSummary, 0, len(raw))
	for _, p := range raw {
		proj := ProjectSummary{
			ID:             identity.EntityID(p.HostKey, p.ScopedKey),
			HostID:         identity.HostID(p.HostKey),
			RegistryID:     p.RegistryID,
			ScopedKey:      p.ScopedKey,
			Name:           p.Name,
			RootPath:       p.RootPath,
			DefaultBranch:  p.DefaultBranch,
			RepositoryKind: p.RepositoryKind,
			Platform:       p.Platform,
			IsStale:        p.IsStale,
			IsSynthesized:  p.IsSynthesized,
		}
		if p.PlatformRepo != "" {
			url := "https://" + effectiveHost(p.PlatformHost) + "/" + p.PlatformRepo
			proj.PlatformURL = &url
		}
		proj.PlatformCoverage = PlatformCoverage(p, activeHost)
		out = append(out, proj)
	}
	return out
}

type worktreeKey struct {
	hostKey   string
	scopedKey string
}

func buildWorktrees(raw []RawWorktree, identity Identity) ([]WorktreeSummary, map[worktreeKey]bool) {
	out := make([]WorktreeSummary, 0, len(raw))
	filtered := make(map[worktreeKey]bool)
	for _, wt := range raw {
		if wt.IsStale && !wt.IsPrimary {
			filtered[worktreeKey{wt.HostKey, wt.ScopedKey}] = true
			continue
		}
		out = append(out, buildWorktree(wt, identity))
	}
	return out, filtered
}

func buildWorktree(wt RawWorktree, identity Identity) WorktreeSummary {
	w := WorktreeSummary{
		ID:                 identity.EntityID(wt.HostKey, wt.ScopedKey),
		HostID:             identity.HostID(wt.HostKey),
		ProjectID:          identity.EntityID(wt.HostKey, wt.ProjectKey),
		RegistryID:         wt.RegistryID,
		ScopedKey:          wt.ScopedKey,
		Name:               wt.Name,
		Path:               wt.Path,
		Branch:             wt.Branch,
		IsPrimary:          wt.IsPrimary,
		IsHidden:           wt.IsHidden,
		IsStale:            wt.IsStale,
		DiffAdded:          wt.DiffAdded,
		DiffRemoved:        wt.DiffRemoved,
		SyncAhead:          wt.SyncAhead,
		SyncBehind:         wt.SyncBehind,
		LinkedPRNumber:     wt.LinkedPRNumber,
		PRURL:              wt.PRURL,
		PRTitle:            wt.PRTitle,
		PRUpdatedAt:        normalizeDateStrPtr(wt.PRUpdatedAt),
		ChecksDetail:       lowerChecks(wt.ChecksDetail),
		LastPolledAt:       normalizeDateStrPtr(wt.LastPolledAt),
		SessionBackend:     sessionBackendOrDefault(wt.SessionBackend),
		LinkedIssueNumbers: intsOrEmpty(wt.LinkedIssueNumbers),
	}
	w.PRState = lowerPtr(wt.PRState)
	w.ChecksStatus = lowerPtr(wt.ChecksStatus)
	// PRReviewDecision and PRMergeable are provider enum vocabularies, lowercased
	// like PRState/ChecksStatus so a peer's casing can't leak through. The diff
	// size and comment count pass straight through; the raw producer already
	// omitted zero/empty values so an undetailed PR carries no misleading state.
	w.PRReviewDecision = lowerPtr(wt.PRReviewDecision)
	w.PRMergeable = lowerPtr(wt.PRMergeable)
	w.PRAdditions = wt.PRAdditions
	w.PRDeletions = wt.PRDeletions
	w.PRCommentCount = wt.PRCommentCount
	return w
}

func buildWorkspaceSummaries(
	raw []RawWorkspace,
	hosts []NeutralHost,
	self NodeID,
) []WorkspaceSummary {
	hostNames := make(map[string]string, len(hosts))
	for _, host := range hosts {
		hostNames[string(host.NodeID)] = host.Name
	}
	out := make([]WorkspaceSummary, 0, len(raw))
	for _, workspace := range raw {
		repository := workspace.Repository
		summary := WorkspaceSummary{
			ID: workspace.ID,
			Repo: WorkspaceRepositorySummary{
				Provider: repository.Provider, PlatformHost: repository.PlatformHost,
				PlatformRepoID: repository.PlatformRepoID,
				RepoPath:       repository.Owner + "/" + repository.Name,
				Owner:          repository.Owner, Name: repository.Name,
			},
			PlatformHost: repository.PlatformHost,
			RepoOwner:    repository.Owner, RepoName: repository.Name,
			ItemType: workspace.ItemType, ItemNumber: workspace.ItemNumber,
			SourceItemVisible: workspace.SourceItemVisible,
			ItemKey:           workspace.ItemKey, GitHeadRef: workspace.GitHeadRef,
			WorktreePath: workspace.WorktreePath, TmuxSession: workspace.TmuxSession,
			TmuxPaneTitle: workspace.TmuxPaneTitle, TmuxWorking: workspace.TmuxWorking,
			TmuxActivitySource:  workspace.TmuxActivitySource,
			TmuxLastOutputAt:    workspace.TmuxLastOutputAt,
			AgentState:          workspace.AgentState,
			AgentStateUpdatedAt: workspace.AgentStateUpdatedAt,
			Status:              workspace.Status, ErrorMessage: workspace.ErrorMessage,
			CreatedAt:          workspace.CreatedAt,
			ItemLastActivityAt: workspace.ItemLastActivityAt,
			MRTitle:            workspace.MRTitle, MRState: lowerPtr(workspace.MRState),
			MRIsDraft: workspace.MRIsDraft, MRCIStatus: lowerPtr(workspace.MRCIStatus),
			MRReviewDecision: lowerPtr(workspace.MRReviewDecision),
			MRAdditions:      workspace.MRAdditions, MRDeletions: workspace.MRDeletions,
			CommitsAhead: workspace.CommitsAhead, CommitsBehind: workspace.CommitsBehind,
			BranchUpstreamMissing: workspace.BranchUpstreamMissing,
			WorktreeDirty:         workspace.WorktreeDirty,
			EnrichmentStatus:      workspace.EnrichmentStatus,
			EnrichmentRefreshedAt: workspace.EnrichmentRefreshedAt,
			EnrichmentError:       workspace.EnrichmentError,
			AssociatedPRNumber:    workspace.AssociatedPRNumber,
			Visible:               true,
		}
		if workspace.Kata != nil {
			summary.Kata = &WorkspaceKataSummary{
				DaemonID:    workspace.Kata.DaemonID,
				ProjectUID:  workspace.Kata.ProjectUID,
				ProjectName: workspace.Kata.ProjectName,
				IssueUID:    workspace.Kata.IssueUID,
				ShortID:     workspace.Kata.ShortID,
				QualifiedID: workspace.Kata.QualifiedID,
				Title:       workspace.Kata.Title,
			}
		}
		if workspace.HostKey != string(self) {
			summary.FleetHostKey = workspace.HostKey
			summary.FleetHostName = hostNames[workspace.HostKey]
		}
		out = append(out, summary)
	}
	return out
}

// sessionBackendOrDefault normalizes a raw worktree session backend onto the
// exported canonical vocabulary (localPTY, localTmux), defaulting
// an empty value to localPTY. A registered worktree with no active session
// carries no backend; emitting an empty string forces strict consumers to
// special-case it, so default it to the local-PTY attach instead. Values
// outside the vocabulary pass through unchanged.
func sessionBackendOrDefault(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SessionBackendLocalPTY
	}
	for _, canonical := range []string{
		SessionBackendLocalPTY,
		SessionBackendLocalTmux,
	} {
		if strings.EqualFold(raw, canonical) {
			return canonical
		}
	}
	return raw
}

// lowerChecks lowercases the status/conclusion of each check, mirroring the
// PR-state/checks-status normalization. Returns nil for an empty input.
func lowerChecks(in []CheckDetail) []CheckDetail {
	if len(in) == 0 {
		return nil
	}
	out := make([]CheckDetail, len(in))
	for i, c := range in {
		c.Status = strings.ToLower(c.Status)
		c.Conclusion = strings.ToLower(c.Conclusion)
		out[i] = c
	}
	return out
}

// intsOrEmpty guarantees a non-nil slice so the enriched JSON emits [] not null.
func intsOrEmpty(in []int) []int {
	if in == nil {
		return []int{}
	}
	return in
}

// tmuxOrEmpty guarantees a non-nil slice so tmuxSessions emits [] not null.
func tmuxOrEmpty(in []TmuxSessionInfo) []TmuxSessionInfo {
	if in == nil {
		return []TmuxSessionInfo{}
	}
	return in
}

func buildSessions(raw []RawSession, filteredWorktreeKeys map[worktreeKey]bool, identity Identity) []SessionSummary {
	out := make([]SessionSummary, 0, len(raw))
	for _, s := range raw {
		if s.WorktreeKey != "" {
			if filteredWorktreeKeys[worktreeKey{s.HostKey, s.WorktreeKey}] {
				continue
			}
		}
		out = append(out, buildSession(s, identity))
	}
	return out
}

func buildSession(sess RawSession, identity Identity) SessionSummary {
	s := SessionSummary{
		ID:             identity.EntityID(sess.HostKey, sess.ScopedKey),
		HostID:         identity.HostID(sess.HostKey),
		ScopedKey:      sess.ScopedKey,
		RuntimeKind:    sess.RuntimeKind,
		Status:         sess.Status,
		SessionKind:    sess.SessionKind,
		Role:           sess.Role,
		ExecutableName: sess.ExecutableName,
		AgentKind:      sess.AgentKind,
		CPUPercent:     sess.CPUPercent,
		ResidentMB:     sess.ResidentMB,
		ProcessCount:   sess.ProcessCount,
		LastActiveAt:   normalizeDateStrPtr(sess.LastActiveAt),
	}
	if sess.WorktreeKey != "" {
		id := identity.EntityID(sess.HostKey, sess.WorktreeKey)
		s.WorktreeID = &id
	}
	if sess.LastOutputAt != nil {
		s.LastOutputAt = normalizeDateStr(*sess.LastOutputAt)
	}
	return s
}

// normalizeDateStrPtr normalizes an optional RFC3339 timestamp pointer.
func normalizeDateStrPtr(s *string) *string {
	if s == nil {
		return nil
	}
	return normalizeDateStr(*s)
}

// buildProjectMap maps "owner/name@host" to project ID for projects that
// have a platform URL.
func buildProjectMap(projects []ProjectSummary) map[string]string {
	m := make(map[string]string)
	for _, p := range projects {
		if p.PlatformURL == nil {
			continue
		}
		owner, name, host, ok := parsePlatformURL(*p.PlatformURL)
		if !ok {
			continue
		}
		m[owner+"/"+name+"@"+host] = p.ID
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func parsePlatformURL(rawURL string) (owner, name, host string, ok bool) {
	u := rawURL
	if idx := strings.Index(u, "://"); idx >= 0 {
		u = u[idx+3:]
	}
	slash := strings.IndexByte(u, '/')
	if slash < 0 {
		return "", "", "", false
	}
	host = u[:slash]
	path := strings.TrimPrefix(u[slash:], "/")
	path = strings.TrimSuffix(path, ".git")
	// The repo name is the final path segment; everything before it is the
	// owner path. Nested groups (e.g. GitLab "group/subgroup/project") keep
	// their full owner so the project key round-trips instead of truncating
	// to the first two segments and losing the repo.
	lastSlash := strings.LastIndexByte(path, '/')
	if lastSlash <= 0 || lastSlash == len(path)-1 {
		return "", "", "", false
	}
	return path[:lastSlash], path[lastSlash+1:], host, true
}

func lowerPtr(s *string) *string {
	if s == nil {
		return nil
	}
	low := strings.ToLower(*s)
	return &low
}

func normalizeDateStr(s string) *string {
	if s == "" {
		return nil
	}
	out := normalizeDateValue(s)
	return &out
}

func normalizeDateValue(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02T15:04:05.000Z07:00")
}
