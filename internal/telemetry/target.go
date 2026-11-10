package telemetry

import (
	"log/slog"
)

// Target is the fixed set of Kubernetes object coordinates an operator can copy
// into kubectl. Only known values are set; unknown fields stay empty and are
// omitted from the log. Building a Target never queries Kubernetes.
type Target struct {
	ClusterID  string `json:"cluster_id,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	Container  string `json:"container,omitempty"`
}

// Empty reports whether no coordinate is known.
func (t Target) Empty() bool {
	return t.ClusterID == "" && t.APIVersion == "" && t.Kind == "" && t.Namespace == "" && t.Name == "" && t.UID == "" && t.Container == ""
}

// Attr renders the target as a grouped object attribute, or nothing when unknown.
func (t Target) Attr() []slog.Attr {
	if t.Empty() {
		return nil
	}
	fields := make([]slog.Attr, 0, 7)
	for _, f := range []struct{ key, value string }{
		{"cluster_id", t.ClusterID},
		{"api_version", t.APIVersion},
		{"kind", t.Kind},
		{"namespace", t.Namespace},
		{"name", t.Name},
		{"uid", t.UID},
		{"container", t.Container},
	} {
		if f.value != "" {
			fields = append(fields, slog.String(f.key, f.value))
		}
	}
	return []slog.Attr{slog.Group("target", attrsToAny(fields)...)}
}

// KV renders the target as a slog attribute suitable for slog.Logger(...) calls.
// slog treats a slog.Attr passed in keyvals as a structured value, producing the
// required nested object rather than a flat key.
func (t Target) KV() []any {
	attrs := t.Attr()
	if len(attrs) == 0 {
		return nil
	}
	return []any{attrs[0]}
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
	}
	return out
}
