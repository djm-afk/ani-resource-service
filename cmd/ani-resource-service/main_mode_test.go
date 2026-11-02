package main

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

func runtimeModeConfig(t *testing.T, dsn string) string {
	t.Helper()
	c := &conf.Bootstrap{Network: &conf.Network{DatabaseDsn: dsn, ClusterId: "11111111-1111-4111-8111-111111111111", CursorSigningKey: testenv.SigningKey(), Worker: &conf.Worker{Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond)}}, Server: &conf.Server{Grpc: &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)}, Admin: &conf.Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)}, ShutdownTimeout: durationpb.New(time.Second)}}
	raw, err := protojson.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestFullInstanceModeReachesTrustedCompositionGuards(t *testing.T) {
	original := flagconf
	t.Cleanup(func() { flagconf = original })
	flagconf = runtimeModeConfig(t, "postgres://unused.invalid/network")
	t.Setenv("ANI_INSTANCE_CONFIG_FILE", filepath.Join(t.TempDir(), "instance.json"))
	for _, entry := range []struct{ mode, want string }{{"full", "read governance CA"}, {"", "read governance CA"}, {"invalid-mode", "unknown ANI_NETWORK_MODE"}} {
		t.Run(entry.mode, func(t *testing.T) {
			t.Setenv("ANI_NETWORK_MODE", entry.mode)
			t.Setenv("ANI_NETWORK_CLIENT_CA", "")
			t.Setenv("ANI_NETWORK_TLS_CERT", "")
			t.Setenv("ANI_NETWORK_TLS_KEY", "")
			err := run(newRuntimeLogger(io.Discard))
			if err == nil || !strings.Contains(err.Error(), entry.want) {
				t.Fatal("composition mode did not retain its trusted transport/mode guard", err)
			}
		})
	}
}
func TestFullInstanceModeRequiresRealImageRuntime(t *testing.T) {
	original := flagconf
	t.Cleanup(func() { flagconf = original })
	fixture := testenv.NewDatabase(t)
	tlsFixture := testenv.NewServiceTLS(t)
	// The fixture repository is not the composition under test. Close its own
	// runtime pool before checking for connections left by a failed app.
	fixture.Repository.Close()
	flagconf = runtimeModeConfig(t, fixture.RuntimeDSN)
	t.Setenv("ANI_NETWORK_MODE", "full")
	t.Setenv("ANI_INSTANCE_CONFIG_FILE", filepath.Join(t.TempDir(), "instance.json"))
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	err := run(newRuntimeLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "instance requires enabled Image runtime") {
		t.Fatal("full mode did not reach the production Instance dependency guard", err)
	}
	// Observe actual runtime sessions, not a synthetic cleanup callback.
	runtimeURL, err := url.Parse(fixture.RuntimeDSN)
	if err != nil {
		t.Fatal("invalid test fixture runtime identity")
	}
	var sessions int
	err = fixture.Owner.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1", runtimeURL.User.Username()).Scan(&sessions)
	if err != nil || sessions != 0 {
		t.Fatal("failed Instance composition retained Network runtime sessions", sessions, err)
	}
}
