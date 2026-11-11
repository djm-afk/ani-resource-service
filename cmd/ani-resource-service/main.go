package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/go-kratos/kratos/v3/log"
	"github.com/go-logr/logr"
	"go.uber.org/automaxprocs/maxprocs"
	"k8s.io/klog/v2"

	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// Name and Version can be overridden with -ldflags at build time.
var (
	Name                = "ani-resource-service"
	Version             = "dev"
	flagconf            string
	flagMigrate         bool
	flagImageMigrate    bool
	flagInstanceMigrate bool
	flagSandboxMigrate  bool
	flagComputeRestore  bool
	flagVMMigrate       bool
	id, _               = os.Hostname()
)

func init() {
	flag.StringVar(&flagconf, "conf", "configs", "config path, for example -conf configs/config.yaml")
	flag.BoolVar(&flagMigrate, "migrate", false, "apply Network migrations using explicit owner environment")
	flag.BoolVar(&flagImageMigrate, "image-migrate", false, "apply Image migrations using explicit owner secret file")
	flag.BoolVar(&flagInstanceMigrate, "instance-migrate", false, "apply Instance migrations using explicit owner secret file")
	flag.BoolVar(&flagSandboxMigrate, "sandbox-migrate", false, "apply Sandbox migrations using explicit owner secret file")
	flag.BoolVar(&flagVMMigrate, "vm-migrate", false, "apply VM migrations using explicit owner secret file")
	flag.BoolVar(&flagComputeRestore, "compute-restore", false, "restore pre-compute metadata with all readers stopped using explicit owner secret file")
}

func main() {
	flag.Parse()
	logOutput := os.Stdout
	if imageAdminAction != "" || vmCloneReleaseMode {
		// Keep the operator's single JSON result readable on stdout.
		logOutput = os.Stderr
	}
	// Validate the level before anything else so an invalid value fails fast and
	// visibly instead of silently clamping the process to a different level.
	level, levelErr := parseRuntimeLevel(os.Getenv("ANI_LOG_LEVEL"))
	logger := newRuntimeLoggerAtLevel(logOutput, level)
	log.SetDefault(logger)
	slog.SetDefault(logger)
	// Route client-go/klog through the same JSON handler; otherwise its
	// warnings are printed as plain text and break the stdout contract.
	klog.SetLogger(logr.FromSlogHandler(logger.Handler()))
	if levelErr != nil {
		attrs := append([]any{"event", "process.start_failed", "step", "logger", "log_format", "json"},
			logfields.ErrorFields(levelErr)...)
		logger.Error("invalid log configuration", attrs...)
		os.Exit(1)
	}
	execute := func() error {
		if vmCloneReleaseMode {
			if sandboxReviewMode || flagVMMigrate || flagComputeRestore || flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("clone release is exclusive with other command modes")
			}
			return runVMCloneRelease()
		}
		if sandboxReviewMode {
			if flagVMMigrate || flagComputeRestore || flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("sandbox diagnostic is exclusive with other command modes")
			}
			return runSandboxUnknownReview()
		}
		if flagVMMigrate {
			if flagComputeRestore || flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("VM migration is exclusive with other command modes")
			}
			return runVMMigration()
		}
		if flagComputeRestore {
			if flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("compute recovery is exclusive with other command modes")
			}
			return runComputeRestore()
		}
		if flagInstanceMigrate {
			if flagSandboxMigrate || flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("instance migration is exclusive with other command modes")
			}
			return runInstanceMigration()
		}
		if flagSandboxMigrate {
			if flagMigrate || flagImageMigrate || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("sandbox migration is exclusive with other command modes")
			}
			return runSandboxMigration()
		}
		if err := validateImageAdminModes(); err != nil {
			return err
		}
		if imageAdminAction != "" {
			return runImageAdmin()
		}
		if flagImageMigrate {
			if flagMigrate || baseConnectivityAction != "" {
				return fmt.Errorf("Image migration, Network migration and base connectivity modes are exclusive")
			}
			return runImageMigration()
		}
		if baseConnectivityAction != "" {
			if flagMigrate {
				return fmt.Errorf("base connectivity and migration modes are exclusive")
			}
			return runBaseConnectivityCommand()
		}
		if flagMigrate {
			return runMigration()
		}
		return run(logger)
	}
	if err := execute(); err != nil {
		// The terminal error keeps its real cause; the event name distinguishes a
		// failure after readiness from one during startup.
		attrs := append([]any{"event", "process.failed"}, logfields.ErrorFields(err)...)
		logger.Error("service terminated", attrs...)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	logger.Info("service starting",
		"event", "process.starting",
		"log_format", "json",
		"log_level", resLevelName(runtimeLevel(os.Getenv("ANI_LOG_LEVEL"))))
	undoMaxProcs, err := maxprocs.Set(maxprocs.Logger(func(format string, args ...interface{}) {
		log.Info("runtime CPU quota", "detail", fmt.Sprintf(format, args...))
	}))
	if err != nil {
		return fmt.Errorf("configure runtime CPU quota: %w", err)
	}
	defer undoMaxProcs()

	c := config.New(config.WithSource(
		file.NewSource(flagconf),
		env.NewSource("ANI"),
	))
	defer c.Close()
	if err := c.Load(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if os.Getenv("ANI_NETWORK_NAMESPACE_PREFIX") != "" || os.Getenv("NETWORK_NAMESPACE_PREFIX") != "" {
		return fmt.Errorf("namespace prefix configuration has been removed")
	}
	if old, err := c.Value("network.namespace_prefix").String(); err == nil && old != "" {
		return fmt.Errorf("network.namespace_prefix has been removed")
	}
	if old, err := c.Value("network.namespacePrefix").String(); err == nil && old != "" {
		return fmt.Errorf("network.namespacePrefix has been removed")
	}
	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		return fmt.Errorf("scan config: %w", err)
	}
	switch mode := os.Getenv("ANI_NETWORK_MODE"); mode {
	case "", "full":
		// The full composition uses the same trusted mTLS listener and method
		// authorization as governance and wires the local Instance consumer.
	case "vpc-read":
		return runVPCRead(&bc, logger)
	case "governance":
		return runGovernance(&bc, logger)
	case "compute":
		return runComputeOnly(&bc, logger)
	default:
		return fmt.Errorf("unknown ANI_NETWORK_MODE %q", mode)
	}
	app, err := buildApp(&bc, logger)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	if err := app.Run(); err != nil {
		return fmt.Errorf("run app: %w", err)
	}
	return nil
}

func newRuntimeLogger(writer io.Writer) *slog.Logger {
	return newRuntimeLoggerAtLevel(writer, runtimeLevel(os.Getenv("ANI_LOG_LEVEL")))
}

// runtimeLevel parses ANI_LOG_LEVEL. Only info and debug are accepted for the
// runtime process; an unsupported value is a startup error handled by main.
func runtimeLevel(raw string) slog.Level {
	level, err := parseRuntimeLevel(raw)
	if err != nil {
		return slog.LevelInfo
	}
	return level
}

func parseRuntimeLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported ANI_LOG_LEVEL %q: only info or debug are accepted", raw)
	}
}

func newRuntimeLoggerAtLevel(writer io.Writer, level slog.Level) *slog.Logger {
	handler := newContractHandler(log.NewHandler(
		log.WithWriter(writer),
		log.WithFormat(log.FormatJSON),
		log.WithLevel(level),
		log.WithAddSource(true),
		log.WithReplaceAttr(contractReplaceAttr),
		log.WithExtractor(tracing.TraceAttrs, businessAttrs),
		log.WithFilter(log.FilterKey(
			"args",
			"authorization",
			"cookie",
			"credential",
			"password",
			"private_key",
			"request",
			"set-cookie",
			"token",
		)),
	))
	return slog.New(handler).With(
		slog.String("service.name", Name),
		slog.String("service.version", Version),
		slog.String("service.instance.id", id),
	)
}

// contractReplaceAttr maps the framework-owned keys onto the public contract:
// the caller location becomes `source` (file:line string) and the timestamp is
// only emitted once, as UTC RFC3339Nano `time`.
func contractReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	// The tracing extractor appends trace_id/span_id unconditionally; outside a
	// span they are empty strings. The schema requires minLength 1 for every
	// string field, so drop empty values at the encoder boundary.
	if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
		return slog.Attr{}
	}
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String(slog.TimeKey, t.UTC().Format(time.RFC3339Nano))
		}
	case slog.LevelKey:
		if lv, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, resLevelName(lv))
		}
	case slog.SourceKey:
		if src, ok := a.Value.Any().(*slog.Source); ok && src != nil {
			return slog.String("source", filepath.Base(src.File)+":"+strconv.Itoa(src.Line))
		}
	case "service.id":
		return slog.String("service.instance.id", a.Value.String())
	}
	return a
}

func resLevelName(lv slog.Level) string {
	switch {
	case lv < slog.LevelInfo:
		return "DEBUG"
	case lv < slog.LevelWarn:
		return "INFO"
	case lv < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// businessAttrs injects the business identity bound once per operation.
func businessAttrs(ctx context.Context) []slog.Attr {
	return telemetry.BusinessAttrs(ctx)
}
