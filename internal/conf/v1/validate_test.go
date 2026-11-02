package conf

import (
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestLoadBalancerRuntimeImageIdentityFormats(t *testing.T) {
	for _, id := range []string{"sha256:" + strings.Repeat("a", 64), "registry/image@sha256:" + strings.Repeat("b", 64)} {
		cfg := validConfig()
		cfg.Network.LoadBalancer = &LoadBalancer{InstallationFingerprint: strings.Repeat("c", 64), ControllerImageId: id, EnvoyImageId: id, ShutdownImageId: id, KcImageId: id}
		if err := cfg.Validate(); err != nil {
			t.Fatal("fixed runtime image ID rejected", id, err)
		}
		for _, invalid := range []string{"registry/image:latest", "sha256:abc", "registry/image@sha256:" + strings.Repeat("b", 63), "prefix sha256:" + strings.Repeat("b", 64)} {
			cfg.Network.LoadBalancer.KcImageId = invalid
			if err := cfg.Validate(); err == nil {
				t.Fatal("unfixed or malformed image ID accepted", invalid)
			}
		}
	}
}

func validConfig() *Bootstrap {
	return &Bootstrap{Network: &Network{DatabaseDsn: "postgres://runtime@127.0.0.1/network", ClusterId: "test", CursorSigningKey: testenv.SigningKey(), Worker: &Worker{
		Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond),
	}}, Server: &Server{
		Grpc:            &Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)},
		Admin:           &Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(5 * time.Second),
	}}
}

func TestBootstrapValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Bootstrap)
		ok     bool
	}{
		{name: "loopback listeners", ok: true},
		{name: "unspecified pod listeners", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "0.0.0.0:19090"
			c.Server.Admin.Addr = "[::]:19091"
		}, ok: true},
		{name: "IPv6 loopback listeners", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "[::1]:19090"
			c.Server.Admin.Addr = "[::1]:19091"
		}, ok: true},
		{name: "invalid grpc network", mutate: func(c *Bootstrap) { c.Server.Grpc.Network = "udp" }},
		{name: "invalid admin network", mutate: func(c *Bootstrap) { c.Server.Admin.Network = "unix" }},
		{name: "hostname is not a literal boundary", mutate: func(c *Bootstrap) { c.Server.Admin.Addr = "localhost:19091" }},
		{name: "specific external address", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "192.0.2.1:19090" }},
		{name: "malformed address", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1" }},
		{name: "invalid port", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1:70000" }},
		{name: "zero port", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1:0" }},
		{name: "duplicate fixed address", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "127.0.0.1:19090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "wildcard conflicts with loopback port", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "0.0.0.0:19090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "equivalent numeric ports", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "127.0.0.1:019090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "missing grpc timeout", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = nil }},
		{name: "zero grpc timeout", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(0) }},
		{name: "negative admin timeout", mutate: func(c *Bootstrap) { c.Server.Admin.Timeout = durationpb.New(-time.Second) }},
		{name: "grpc timeout above maximum", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(31 * time.Second) }},
		{name: "invalid protobuf timeout", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Timeout = &durationpb.Duration{Nanos: 1_000_000_000}
		}},
		{name: "missing shutdown timeout", mutate: func(c *Bootstrap) { c.Server.ShutdownTimeout = nil }},
		{name: "zero shutdown timeout", mutate: func(c *Bootstrap) { c.Server.ShutdownTimeout = durationpb.New(0) }},
		{name: "shutdown timeout above maximum", mutate: func(c *Bootstrap) {
			c.Server.ShutdownTimeout = durationpb.New(31 * time.Second)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := proto.Clone(validConfig()).(*Bootstrap)
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			err := cfg.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestBootstrapValidateRequiresCompleteServerConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Bootstrap
	}{
		{name: "nil bootstrap"},
		{name: "missing server", cfg: &Bootstrap{}},
		{name: "missing grpc", cfg: &Bootstrap{Server: &Server{Admin: validConfig().Server.Admin}}},
		{name: "missing admin", cfg: &Bootstrap{Server: &Server{Grpc: validConfig().Server.Grpc}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestBootstrapRequiresVPCDependenciesAndBoundedWorkerTiming(t *testing.T) {
	cfg := validConfig()
	cfg.Network = &Network{DatabaseDsn: "postgres://runtime@127.0.0.1/network", ClusterId: "test", CursorSigningKey: testenv.SigningKey(), Worker: &Worker{
		Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond),
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Network.Worker.Lease = durationpb.New(time.Second)
	if err := cfg.Validate(); err == nil {
		t.Fatal("lease shorter than external request budget accepted")
	}
	cfg.Network.Worker.Lease = durationpb.New(20 * time.Second)
	cfg.Network.CursorSigningKey = "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("short cursor signing key accepted")
	}
	cfg.Network = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing business dependencies accepted")
	}
}

func TestNET05AObservationBudgetsAreBounded(t *testing.T) {
	baseline := validConfig()
	baseline.Network.Observation = &Observation{AuditInterval: durationpb.New(30 * time.Second), AuditJitter: durationpb.New(2 * time.Second), AuditTimeout: durationpb.New(10 * time.Second), FlushInterval: durationpb.New(100 * time.Millisecond), QueueCapacity: 4096, WorkersPerKind: 2, RequestQps: 5, RequestBurst: 10}
	if e := baseline.Validate(); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*Observation){
		"missing interval": func(o *Observation) { o.AuditInterval = nil },
		"zero flush":       func(o *Observation) { o.FlushInterval = durationpb.New(0) },
		"unbounded jitter": func(o *Observation) { o.AuditJitter = durationpb.New(6 * time.Second) },
		"no freshness headroom": func(o *Observation) {
			o.AuditInterval = durationpb.New(40 * time.Second)
			o.AuditJitter = durationpb.New(5 * time.Second)
			o.AuditTimeout = durationpb.New(15 * time.Second)
		},
		"unbounded memory":  func(o *Observation) { o.QueueCapacity = 65537 },
		"unbounded workers": func(o *Observation) { o.WorkersPerKind = 5 },
		"zero qps":          func(o *Observation) { o.RequestQps = 0 },
		"unbounded burst":   func(o *Observation) { o.RequestBurst = 201 },
	} {
		t.Run(name, func(t *testing.T) {
			c := proto.Clone(baseline).(*Bootstrap)
			mutate(c.Network.Observation)
			if e := c.Validate(); e == nil {
				t.Fatal("unsafe observation budget accepted")
			}
		})
	}
}
