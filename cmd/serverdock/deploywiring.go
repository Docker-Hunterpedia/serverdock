package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// loadInstallID reads meta.install_id, which migration 0004 creates (16 random
// bytes in lowercase hex). The agent sets serverdock.instance to it and names the
// app network after it, so two installs on one Docker host never touch each
// other's containers.
func loadInstallID(ctx context.Context, st *store.Store) (string, error) {
	var id string
	err := st.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'install_id'`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("meta.install_id is missing: the database migration did not run")
	}
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		return "", fmt.Errorf("meta.install_id is not 32 lowercase hex characters")
	}
	return id, nil
}

// localOps builds the handlers of the local-only agent ops for the in-process
// local agent. The remote agent process never gets them: its handler table holds
// RemoteOK ops only, whatever Config.Ops says. CheckCreate is policy.CheckCreateAgent:
// default-deny on the exact bytes sent to Docker.
func localOps(docker *dockerapi.Client, instance string, allowLoopbackRegistry bool, log *slog.Logger) (map[string]agent.Handler, error) {
	return agentops.Handlers(agentops.Deps{Docker: docker, Instance: instance, CheckCreate: policy.CheckCreateAgent, AllowLoopbackRegistry: allowLoopbackRegistry, Log: log})
}

// recordAppSubnet picks (first start) or reads (later starts) the app subnet and
// returns the AppNet that checks it later. It can block startup for up to 10 s
// when Docker is down. A failure is logged and not fatal (without Docker nothing
// can be deployed anyway, and the choice is retried the next time something asks
// for it), except an explicit --app-subnet that overlaps the host or a Docker
// network: that refuses to start and records nothing.
func recordAppSubnet(ctx context.Context, st *store.Store, docker *dockerapi.Client, instance, flag string, log *slog.Logger) (*deploy.AppNet, error) {
	an, err := deploy.NewAppNet(deploy.AppNetOptions{Store: st, Instance: instance, Flag: flag, Docker: docker, Log: log})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := an.Subnet(ctx); err != nil {
		var oe *deploy.OverlapError
		if errors.As(err, &oe) && oe.FirstStart {
			return nil, err // an explicit --app-subnet that clashes: refuse to start, save nothing
		}
		log.Warn("app subnet not recorded yet", "err", err)
	}
	return an, nil
}
