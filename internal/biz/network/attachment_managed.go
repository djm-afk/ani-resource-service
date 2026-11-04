package biz

type WorkloadReference struct{ APIVersion, Kind, Namespace, Name, UID string }
type PodReference struct{ Reference, Owner WorkloadReference }
type SnapshotComparison int

const (
	SnapshotAccepted SnapshotComparison = iota
	SnapshotStale
	SnapshotConflict
)

func consumerScopeMatches(a Attachment, c ConsumerSubmission) bool {
	return c.TenantID == a.TenantID && c.InstanceID == a.InstanceID && c.SubmissionID == a.SubmissionID &&
		c.Generation == a.Generation && c.AttachmentID == a.ID && c.ClusterID == a.ClusterID && c.Namespace == a.Namespace
}

func referenceValid(r WorkloadReference, api, kind, namespace string) bool {
	return r.APIVersion == api && r.Kind == kind && r.Namespace == namespace && namespaceName.MatchString(r.Name) && r.UID != "" && len(r.UID) <= 128
}

func validConsumerState(a Attachment, c ConsumerSubmission) bool {
	if a.FinalizationID != "" && a.FinalizationID != c.FinalizationID {
		return false
	}
	switch c.State {
	case "open":
		return c.FinalizationID == ""
	case "closing", "closed":
		return UUIDFinalization(c.FinalizationID)
	default:
		return false
	}
}

func validCurrentConsumer(a Attachment, c ConsumerSubmission, protocol int32) bool {
	if c.ProtocolVersion != protocol || c.SnapshotRevision < 1 || !consumerScopeMatches(a, c) || len(c.CurrentPods) > 1 || !validConsumerState(a, c) {
		return false
	}
	if c.DesiredRunState != "running" && c.DesiredRunState != "stopped" && c.DesiredRunState != "deleted" {
		return false
	}
	for _, pod := range c.CurrentPods {
		if !referenceValid(pod.Reference, "v1", "Pod", a.Namespace) {
			return false
		}
	}
	return true
}

func validManagedConsumer(a Attachment, c ConsumerSubmission) bool {
	return validCurrentConsumer(a, c, 2)
}

func UUIDFinalization(value string) bool {
	parsed, err := ParseTenant(value)
	return err == nil && parsed == value
}

// CompareConsumerSnapshot only orders the owning service's current management
// revisions. Historical controllers, retired Pods and provider objects are not
// part of the consumer contract.
func CompareConsumerSnapshot(a Attachment, c ConsumerSubmission) SnapshotComparison {
	protocol := int32(2)
	if a.WorkloadMode == "vm_managed" {
		protocol = 3
	}
	if !validCurrentConsumer(a, c, protocol) {
		return SnapshotConflict
	}
	if c.SnapshotRevision < a.SnapshotRevision {
		return SnapshotStale
	}
	if a.ConsumerSnapshot != nil {
		old := a.ConsumerSnapshot
		if old.FinalizationID != "" && old.FinalizationID != c.FinalizationID {
			return SnapshotConflict
		}
		rank := map[string]int{"open": 0, "closing": 1, "closed": 2}
		if rank[c.State] < rank[old.State] {
			return SnapshotConflict
		}
	}
	return SnapshotAccepted
}
