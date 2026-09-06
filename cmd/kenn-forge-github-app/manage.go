package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/forge/githubapp"
	"go.kenn.io/forge/internal/config"
)

func runInstall(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app install", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	host := fs.String("host", "", "GitHub host of the app to install")
	owner := fs.String("owner", "", "GitHub account that owns the app or installation")
	appID := fs.Int64("app-id", 0, "GitHub App ID to select when host or owner is ambiguous")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the installation")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	app, err := selectApp(cfg, *host, *owner, *appID)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return env.runInstallFlow(ctx, cfg, app, !*noBrowser, *timeout)
}

func runUninstall(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app uninstall", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	host := fs.String("host", "", "GitHub host of the app to uninstall")
	owner := fs.String("owner", "", "GitHub account that owns the app or installation")
	appID := fs.Int64("app-id", 0, "GitHub App ID to select when host or owner is ambiguous")
	yes := fs.Bool("yes", false, "confirm uninstalling without prompting")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	app, err := selectApp(cfg, *host, *owner, *appID)
	if err != nil {
		return err
	}
	if app.InstallationID == 0 {
		return fmt.Errorf("app %q has no recorded installation", app.Slug)
	}
	if !*yes {
		return fmt.Errorf(
			"uninstalling removes the app's access to %s; re-run with --yes to confirm",
			app.InstallationAccount,
		)
	}
	ctx := context.Background()
	jwt, err := appJWT(app, env.now())
	if err != nil {
		return err
	}
	err = env.apiClient(app.Host).DeleteInstallation(ctx, jwt, app.InstallationID)
	// A 404 means the installation is already gone on GitHub's side;
	// still clear the stale local record.
	if err != nil && !githubapp.IsStatus(err, http.StatusNotFound) {
		return err
	}
	fmt.Fprintf(env.stdout,
		"Uninstalled app %q from %s. kenn-forge falls back to PAT tokens for %s.\n",
		app.Slug, app.InstallationAccount, app.Host,
	)
	oldApp := app
	app.InstallationID = 0
	app.InstallationAccount = ""
	return updateAppSlotInConfig(cfg, env.configPath, oldApp, app)
}

func runDelete(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app delete", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	host := fs.String("host", "", "GitHub host of the app to delete")
	owner := fs.String("owner", "", "GitHub account that owns the app or installation")
	appID := fs.Int64("app-id", 0, "GitHub App ID to select when host or owner is ambiguous")
	yes := fs.Bool("yes", false, "confirm deletion without prompting")
	localOnly := fs.Bool("local-only", false,
		"only remove the local config entry and key (app already deleted on GitHub)")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the deletion")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	app, err := selectApp(cfg, *host, *owner, *appID)
	if err != nil {
		return err
	}
	if !*yes {
		return fmt.Errorf(
			"deleting app %q removes its credentials permanently; re-run with --yes to confirm",
			app.Slug,
		)
	}
	configuredApp := app

	if !*localOnly {
		// Deletion is confirmed by the app's credentials dying, so the
		// credentials must demonstrably work first. Otherwise a stale
		// or rotated key would read as "deleted" and local state would
		// be removed while the app keeps its access on GitHub.
		ctxCheck := context.Background()
		client := env.apiClient(app.Host)
		app, err = env.refreshAppMetadata(ctxCheck, client, app)
		if err != nil {
			if githubapp.IsStatus(err, http.StatusUnauthorized) ||
				githubapp.IsStatus(err, http.StatusNotFound) {
				return fmt.Errorf(
					"cannot verify deletion, GitHub rejected the app credentials: %w\n"+
						"if the app is already deleted, re-run with --local-only", err,
				)
			}
			return fmt.Errorf(
				"cannot verify deletion, app credentials are unusable: %w\n"+
					"delete the app in GitHub settings yourself, then re-run with --local-only", err,
			)
		}

		// GitHub has no API for deleting an app; the user confirms it
		// in browser settings while we poll for the credentials to die.
		url := settingsURL(env.webBaseFor(app.Host), app) + "/advanced"
		fmt.Fprintf(env.stdout,
			"Delete the app in GitHub settings (Danger Zone -> Delete GitHub App):\n  %s\n", url,
		)
		if !*noBrowser {
			if err := env.openBrowser(url); err != nil {
				fmt.Fprintf(env.stdout, "could not open browser: %v\n", err)
			}
		}
		fmt.Fprintln(env.stdout, "Waiting for the app to disappear...")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		err = env.pollUntil(ctx, *timeout, func(ctx context.Context) (bool, error) {
			jwt, err := appJWT(app, env.now())
			if err != nil {
				return false, err
			}
			_, err = client.GetApp(ctx, jwt)
			if err == nil {
				return false, nil
			}
			if githubapp.IsStatus(err, http.StatusUnauthorized) ||
				githubapp.IsStatus(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			return fmt.Errorf(
				"app still exists on %s: %w\n"+
					"re-run with --local-only after deleting it in the browser", app.Host, err,
			)
		}
	}

	if err := removeAppFromConfig(cfg, env.configPath, app); err != nil {
		return err
	}
	if appPrivateKeyOwnedByCLI(env.configPath, configuredApp, app.Slug) {
		if err := os.Remove(app.PrivateKeyPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(env.stdout, "could not remove private key %s: %v\n", app.PrivateKeyPath, err)
		} else {
			fmt.Fprintf(env.stdout, "Removed private key %s\n", app.PrivateKeyPath)
		}
	} else {
		fmt.Fprintf(env.stdout, "Preserved external private key %s\n", app.PrivateKeyPath)
	}
	fmt.Fprintf(env.stdout, "Deleted app %q for host %s from config.\n", app.Slug, app.Host)
	return nil
}

func appPrivateKeyOwnedByCLI(
	configPath string, app config.GitHubAppConfig, alternateSlugs ...string,
) bool {
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return false
	}
	keyPath, err := filepath.Abs(app.PrivateKeyPath)
	if err != nil {
		return false
	}
	if filepath.Dir(keyPath) != configDir {
		return false
	}
	base := filepath.Base(keyPath)
	prefix := "github-app-" + strings.ReplaceAll(app.Host, ":", "_") + "-"
	slugs := append([]string{app.Slug}, alternateSlugs...)
	for _, slug := range slugs {
		if slug == "" {
			continue
		}
		if base == prefix+slug+".pem" {
			return true
		}
	}
	return false
}

func runOpen(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app open", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	host := fs.String("host", "", "GitHub host of the app to open")
	owner := fs.String("owner", "", "GitHub account that owns the app or installation")
	appID := fs.Int64("app-id", 0, "GitHub App ID to select when host or owner is ambiguous")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	app, err := selectApp(cfg, *host, *owner, *appID)
	if err != nil {
		return err
	}
	ctx := context.Background()
	client := env.apiClient(app.Host)
	app, err = env.refreshAppMetadata(ctx, client, app)
	if err != nil {
		return err
	}
	url := settingsURL(env.webBaseFor(app.Host), app)
	fmt.Fprintf(env.stdout, "App settings: %s\n", url)
	return env.openBrowser(url)
}
