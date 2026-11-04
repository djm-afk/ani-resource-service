package data

import (
	"context"
	"fmt"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type InstanceConsumer struct {
	client networkv1.InstanceNetworkConsumerServiceClient
	conn   *grpc.ClientConn
}

func NewInstanceConsumer(endpoint string) (*InstanceConsumer, error) {
	c := &InstanceConsumer{}
	if endpoint == "" {
		return c, nil
	}
	conn, e := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		return nil, fmt.Errorf("invalid instance consumer target")
	}
	c.conn = conn
	c.client = networkv1.NewInstanceNetworkConsumerServiceClient(conn)
	return c, nil
}
func (c *InstanceConsumer) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
func (c *InstanceConsumer) GetSubmission(ctx context.Context, a biz.Attachment) (biz.ConsumerSubmission, error) {
	if c.client == nil {
		return biz.ConsumerSubmission{}, fmt.Errorf("instance consumer unavailable")
	}
	version := int32(1)
	if a.WorkloadMode == "controller_managed" {
		version = 2
	}
	r, e := c.client.GetSubmission(ctx, &networkv1.GetSubmissionRequest{ProtocolVersion: version, TenantId: a.TenantID, InstanceId: a.InstanceID, SubmissionId: a.SubmissionID, Generation: a.Generation, AttachmentId: a.ID, ExpectedFinalizationId: a.FinalizationID})
	if e != nil {
		return biz.ConsumerSubmission{}, e
	}
	states := map[networkv1.SubmissionState]string{networkv1.SubmissionState_SUBMISSION_STATE_OPEN: "open", networkv1.SubmissionState_SUBMISSION_STATE_CLOSING: "closing", networkv1.SubmissionState_SUBMISSION_STATE_CLOSED: "closed"}
	value := biz.ConsumerSubmission{ProtocolVersion: r.GetProtocolVersion(), TenantID: r.GetTenantId(), InstanceID: r.GetInstanceId(), SubmissionID: r.GetSubmissionId(), AttachmentID: r.GetAttachmentId(), ClusterID: r.GetClusterId(), Namespace: r.GetNamespace(), Generation: r.GetGeneration(), State: states[r.GetState()], FinalizationID: r.GetFinalizationId(), PodUIDs: r.GetPodUids()}
	value.SnapshotRevision = r.GetSnapshotRevision()
	value.DesiredRunState = r.GetDesiredRunState()
	for _, pod := range r.GetCurrentPods() {
		value.CurrentPods = append(value.CurrentPods, biz.PodReference{Reference: consumerWorkload(pod.GetReference()), Owner: consumerWorkload(pod.GetOwner())})
	}
	if r.ClosedAt != nil {
		if e := r.ClosedAt.CheckValid(); e != nil {
			return biz.ConsumerSubmission{}, e
		}
		t := r.ClosedAt.AsTime()
		value.ClosedAt = &t
	}
	return value, nil
}

func consumerWorkload(r *networkv1.WorkloadReference) biz.WorkloadReference {
	if r == nil {
		return biz.WorkloadReference{}
	}
	return biz.WorkloadReference{APIVersion: r.GetApiVersion(), Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName(), UID: r.GetUid()}
}
