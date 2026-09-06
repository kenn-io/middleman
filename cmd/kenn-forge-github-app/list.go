package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/forge/internal/config"
)

type appStatus struct {
	Host                string `json:"host"`
	Role                string `json:"role"`
	AppID               int64  `json:"app_id"`
	Slug                string `json:"slug"`
	Owner               string `json:"owner,omitempty"`
	OwnerType           string `json:"owner_type,omitempty"`
	InstallationID      int64  `json:"installation_id,omitempty"`
	InstallationAccount string `json:"installation_account,omitempty"`
	RateLimit           int    `json:"rate_limit,omitempty"`
	RateRemaining       int    `json:"rate_remaining,omitempty"`
	RateResetsAt        string `json:"rate_resets_at,omitempty"`
	Error               string `json:"error,omitempty"`
}

func runList(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app list", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.GitHubApps) == 0 {
		fmt.Fprintln(env.stdout,
			"no github apps configured; run \"kenn-forge-github-app create\" to add one")
		return nil
	}

	ctx := context.Background()
	statuses := make([]appStatus, 0, len(cfg.GitHubApps))
	for _, app := range cfg.GitHubApps {
		status := appStatus{
			Host:                app.Host,
			Role:                app.Role,
			AppID:               app.AppID,
			Slug:                app.Slug,
			Owner:               app.Owner,
			OwnerType:           app.OwnerType,
			InstallationID:      app.InstallationID,
			InstallationAccount: app.InstallationAccount,
		}
		if status.Role == "" {
			status.Role = config.GitHubAppRoleSync
		}
		if err := env.fillLiveStatus(ctx, cfg, app, &status); err != nil {
			status.Error = err.Error()
		}
		statuses = append(statuses, status)
	}

	if *asJSON {
		enc := json.NewEncoder(env.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(statuses)
	}
	w := tabwriter.NewWriter(env.stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tROLE\tAPP ID\tSLUG\tOWNER\tINSTALLATION\tACCOUNT\tRATE (CORE)\tSTATUS")
	for _, s := range statuses {
		rate, owner, install, account := "-", "-", "-", "-"
		if s.Owner != "" {
			owner = s.Owner
		}
		if s.InstallationID != 0 {
			install = fmt.Sprintf("%d", s.InstallationID)
		}
		if s.InstallationAccount != "" {
			account = s.InstallationAccount
		}
		if s.RateLimit > 0 {
			rate = fmt.Sprintf("%d/%d resets %s", s.RateRemaining, s.RateLimit, s.RateResetsAt)
		}
		status := "ok"
		if s.Error != "" {
			status = s.Error
		} else if s.InstallationID == 0 {
			status = "not installed"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Host, s.Role, s.AppID, s.Slug, owner, install, account, rate, status)
	}
	return w.Flush()
}

// fillLiveStatus queries GitHub for the app's current installation
// and, when installed, the rate budget of a freshly minted token.
// Sync installations limited to selected repositories are checked against
// the configured repos: a manually edited or restored config can carry an
// installation the CLI never verified, and surfacing the gap here beats
// discovering it as sync-time 404s. Archive installations intentionally allow
// partial coverage and use normal routes elsewhere.
func (env *appEnv) fillLiveStatus(
	ctx context.Context, cfg *config.Config, app config.GitHubAppConfig, status *appStatus,
) error {
	client := env.apiClient(app.Host)
	jwt, err := appJWT(app, env.now())
	if err != nil {
		return err
	}
	if _, err := client.GetApp(ctx, jwt); err != nil {
		return err
	}
	if app.InstallationID == 0 {
		return nil
	}
	token, err := client.CreateInstallationToken(ctx, jwt, app.InstallationID)
	if err != nil {
		return err
	}
	rate, err := client.CoreRateLimit(ctx, token.Token)
	if err != nil {
		return err
	}
	status.RateLimit = rate.Limit
	status.RateRemaining = rate.Remaining
	status.RateResetsAt = time.Unix(rate.Reset, 0).UTC().Format(time.RFC3339)

	installs, err := client.ListInstallations(ctx, jwt)
	if err != nil {
		return err
	}
	for _, install := range installs {
		if install.ID != app.InstallationID ||
			strings.EqualFold(install.RepositorySelection, "all") {
			continue
		}
		accessible, err := client.ListInstallationRepositories(ctx, token.Token)
		if err != nil {
			return err
		}
		if app.Role != config.GitHubAppRoleArchive {
			if missing := missingSelectedRepos(
				cfg, app.Host, install.Account.Login, accessible,
			); len(missing) > 0 {
				return fmt.Errorf(
					"installation does not cover %s; edit its repository access on GitHub",
					strings.Join(missing, ", "),
				)
			}
		}
	}
	return nil
}
