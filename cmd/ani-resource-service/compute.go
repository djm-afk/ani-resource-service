package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/compute"
)

type computeSettings struct{ DatabaseDSNFile, ClusterID string }
type computeComponents struct {
	repo     *data.Postgres
	resolver *biz.Resolver
	service  *service.Service
	settings computeSettings
}

func (c *computeComponents) Close() {
	if c != nil && c.repo != nil {
		c.repo.Close()
	}
}

// Compute is independently configured and constructed. No image, Notebook,
// namespace, instance runtime or workload environment is required.
func openCompute(ctx context.Context, expectedCluster ...string) (*computeComponents, error) {
	path := os.Getenv("ANI_COMPUTE_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	body, e := readImagePrivateFile(path)
	if e != nil {
		return nil, e
	}
	defer clear(body)
	var cfg computeSettings
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&cfg); e != nil || decoder.Decode(new(any)) != io.EOF || !filepath.IsAbs(cfg.DatabaseDSNFile) || !biz.UUID(cfg.ClusterID) {
		return nil, fmt.Errorf("invalid compute configuration")
	}
	if len(expectedCluster) > 1 || (len(expectedCluster) == 1 && cfg.ClusterID != expectedCluster[0]) {
		return nil, fmt.Errorf("compute cluster differs from target kube-system UID")
	}
	raw, e := readImagePrivateFile(cfg.DatabaseDSNFile)
	if e != nil {
		return nil, e
	}
	defer clear(raw)
	repo, e := data.OpenPostgres(ctx, strings.TrimSpace(string(raw)))
	if e != nil {
		return nil, e
	}
	resolver, e := biz.NewResolver(repo, cfg.ClusterID)
	if e != nil {
		repo.Close()
		return nil, e
	}
	handler, e := service.NewService(repo, resolver, cfg.ClusterID)
	if e != nil {
		repo.Close()
		return nil, e
	}
	return &computeComponents{repo: repo, resolver: resolver, service: handler, settings: cfg}, nil
}
func requireInstanceCompute(c *computeComponents, cfg instanceSettings) error {
	if c == nil || filepath.Clean(cfg.DatabaseDSNFile) != filepath.Clean(c.settings.DatabaseDSNFile) || cfg.Service.ClusterID != c.settings.ClusterID {
		return fmt.Errorf("instance requires compute configured with the same managed database DSN file and cluster")
	}
	return nil
}
