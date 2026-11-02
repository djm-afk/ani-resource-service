package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

var baseConnectivityAction string
var baseConnectivityRun string
var baseConnectivityPool string
var baseConnectivityReviewedSHA string
var baseConnectivityInterval time.Duration
var baseConnectivityMax int

func init() {
	flag.StringVar(&baseConnectivityAction, "base-connectivity", "", "admin mode: plan, status, resume, pause, dispatch, rollout, enable-new, disable-new, activate-legacy")
	flag.StringVar(&baseConnectivityRun, "base-run", "", "stable reviewed backfill run UUID")
	flag.StringVar(&baseConnectivityPool, "base-pool", "", "Intranet pool ID for a new plan; empty freezes the current default")
	flag.StringVar(&baseConnectivityReviewedSHA, "base-reviewed-sha256", "", "exact reviewed plan SHA-256 required for resume")
	flag.DurationVar(&baseConnectivityInterval, "base-interval", time.Second, "shared minimum admission interval (100ms..1h), fixed at planning")
	flag.IntVar(&baseConnectivityMax, "base-max", 1, "maximum candidates processed by dispatch (1..10000); accepted operations continue on service workers")
}

// The administrative command uses the restricted runtime role and performs no
// Provider calls. It cannot select owner credentials, kubeconfig or shell SQL.
func runBaseConnectivityCommand() error {
	dsn := os.Getenv("ANI_NETWORK_DATABASE_DSN")
	cluster := os.Getenv("ANI_NETWORK_CLUSTER_ID")
	if os.Getenv("ANI_NETWORK_NAMESPACE_PREFIX") != "" || os.Getenv("NETWORK_NAMESPACE_PREFIX") != "" {
		return fmt.Errorf("namespace prefix configuration has been removed")
	}

	if dsn == "" || cluster == "" {
		return fmt.Errorf("base-connectivity requires ANI_NETWORK_DATABASE_DSN and ANI_NETWORK_CLUSTER_ID")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	repository, err := data.OpenPostgres(ctx, dsn, data.Placement{ClusterID: cluster, NamespacePrefix: "tenant-"})
	if err != nil {
		return err
	}
	defer repository.Close()
	return executeBaseConnectivityCommand(ctx, repository, os.Stdout)
}

func executeBaseConnectivityCommand(ctx context.Context, repository *data.Postgres, output io.Writer) error {
	encoder := json.NewEncoder(output)
	write := func(value any, err error) error {
		if err != nil {
			return err
		}
		return encoder.Encode(value)
	}
	switch baseConnectivityAction {
	case "plan":
		return write(repository.PlanBaseBackfill(ctx, data.BaseBackfillPlanInput{RunID: baseConnectivityRun, PoolID: baseConnectivityPool, Interval: baseConnectivityInterval}))
	case "status":
		return write(repository.GetBaseBackfill(ctx, baseConnectivityRun))
	case "resume":
		return write(repository.SetBaseBackfillPaused(ctx, baseConnectivityRun, false, baseConnectivityReviewedSHA))
	case "pause":
		return write(repository.SetBaseBackfillPaused(ctx, baseConnectivityRun, true, ""))
	case "rollout":
		return write(repository.GetBaseConnectivityRollout(ctx))
	case "enable-new":
		return write(repository.SetNewVPCBaseConnectivity(ctx, true))
	case "disable-new":
		return write(repository.SetNewVPCBaseConnectivity(ctx, false))
	case "activate-legacy":
		return write(repository.ActivateLegacyBaseAggregation(ctx))
	case "dispatch":
		if baseConnectivityMax < 1 || baseConnectivityMax > 10000 {
			return fmt.Errorf("base-max must be within 1..10000")
		}
		for count := 0; count < baseConnectivityMax; {
			result, err := repository.AdmitNextBaseBackfill(ctx, baseConnectivityRun)
			if err != nil {
				return err
			}
			if err = encoder.Encode(result); err != nil {
				return err
			}
			switch result.State {
			case "accepted", "conflict":
				count++
			case "waiting":
				// A concurrent pause is noticed within one second even for long rate limits.
				delay := time.Duration(result.WaitMS) * time.Millisecond
				if delay > time.Second {
					delay = time.Second
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			case "blocked":
				return fmt.Errorf("backfill paused after prerequisite failure; inspect status and resume the same reviewed plan after repair")
			case "paused", "admission_complete":
				return nil
			default:
				return fmt.Errorf("unexpected backfill admission state")
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown base-connectivity action")
	}
}
