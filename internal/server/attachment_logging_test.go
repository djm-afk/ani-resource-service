package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestAttachmentLogsProviderAndPersistFailures(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	o := &Observability{attachmentLogger: logger}
	work := biz.AttachmentWork{Attachment: biz.Attachment{ID: "att-1", SubmissionID: "sub-1", TenantID: "t-1"}}
	progress := biz.AttachmentProgress{
		ProviderError: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.New("apiserver unreachable")},
		Reason:        biz.ProviderUnavailable,
	}
	o.logAttachmentFailure(logger, context.Background(), work, progress, errors.New("write failed: SQLSTATE 23505"))

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("bad JSON %v: %s", err, line)
		}
		records = append(records, record)
	}
	find := func(event string) map[string]any {
		for _, record := range records {
			if record["event"] == event {
				return record
			}
		}
		return nil
	}

	providerFailure := find("step.failed")
	if providerFailure == nil {
		t.Fatalf("missing step.failed: %v", records)
	}
	if providerFailure["dependency"] != "kubernetes" || providerFailure["attachment_id"] != "att-1" {
		t.Errorf("provider failure fields = %v", providerFailure)
	}
	if msg, _ := providerFailure["error"].(string); !strings.Contains(msg, "apiserver unreachable") {
		t.Errorf("provider failure lost its cause: %v", providerFailure["error"])
	}

	persistFailure := find("state.persist_failed")
	if persistFailure == nil {
		t.Fatalf("missing state.persist_failed: %v", records)
	}
	if persistFailure["dependency"] != "postgresql" || persistFailure["attachment_id"] != "att-1" {
		t.Errorf("persist failure fields = %v", persistFailure)
	}
	if msg, _ := persistFailure["error"].(string); !strings.Contains(msg, "23505") {
		t.Errorf("persist failure lost the SQLSTATE: %v", persistFailure["error"])
	}
}
