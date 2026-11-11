package main

import (
	"context"
	"log/slog"
	"strings"
)

// runtimeEvent is the handler-owned event name for framework records. Business
// code must never pass it explicitly to stand in for a registered event.
const runtimeEvent = "runtime.log"

// contractHandler keeps every record on the public contract before it reaches
// the JSON encoder:
//   - records without an event get the framework fallback event;
//   - empty string attributes are dropped, because the schema requires every
//     string field to carry at least one character;
//   - an empty message is replaced with the facts the record does carry.
//
// It wraps the Kratos handler so a single place normalises all producers,
// including the standard library and client-go output routed through klog.
type contractHandler struct {
	inner    slog.Handler
	hasEvent bool
}

func newContractHandler(inner slog.Handler) slog.Handler {
	return &contractHandler{inner: inner}
}

func (h *contractHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contractHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept, hasEvent := filterAttrs(attrs)
	next := &contractHandler{inner: h.inner.WithAttrs(kept), hasEvent: h.hasEvent || hasEvent}
	return next
}

func (h *contractHandler) WithGroup(name string) slog.Handler {
	return &contractHandler{inner: h.inner.WithGroup(name), hasEvent: h.hasEvent}
}

func (h *contractHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, r.NumAttrs()+1)
	hasEvent := h.hasEvent
	event := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "event" && a.Value.Kind() == slog.KindString && a.Value.String() != "" {
			hasEvent = true
			event = a.Value.String()
		}
		if kept, ok := filterAttr(a); ok {
			attrs = append(attrs, kept)
		}
		return true
	})

	if !hasEvent {
		event = runtimeEvent
		attrs = append(attrs, slog.String("event", event))
	}

	msg := r.Message
	if msg == "" {
		msg = fallbackMessage(event, attrs)
	}

	out := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	out.AddAttrs(attrs...)
	return h.inner.Handle(ctx, out)
}

// filterAttrs drops empty strings and reports whether a usable event was bound
// through With.
func filterAttrs(attrs []slog.Attr) ([]slog.Attr, bool) {
	kept := make([]slog.Attr, 0, len(attrs))
	hasEvent := false
	for _, a := range attrs {
		if a.Key == "event" && a.Value.Kind() == slog.KindString && a.Value.String() != "" {
			hasEvent = true
		}
		if v, ok := filterAttr(a); ok {
			kept = append(kept, v)
		}
	}
	return kept, hasEvent
}

// filterAttr recursively removes empty string attributes, including inside
// groups, so the encoder never emits a value the schema rejects.
func filterAttr(a slog.Attr) (slog.Attr, bool) {
	if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
		return a, false
	}
	if a.Value.Kind() != slog.KindGroup {
		return a, true
	}
	group := a.Value.Group()
	kept := make([]any, 0, len(group))
	for _, child := range group {
		if v, ok := filterAttr(child); ok {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 {
		return a, false
	}
	return slog.Group(a.Key, kept...), true
}

// fallbackMessage composes a readable message from stable identifying facts so
// records without a message are still triageable.
func fallbackMessage(event string, attrs []slog.Attr) string {
	value := func(key string) string {
		for _, a := range attrs {
			if a.Key == key && a.Value.Kind() == slog.KindString {
				return a.Value.String()
			}
		}
		return ""
	}
	parts := make([]string, 0, 3)
	for _, key := range []string{"component", "operation", "code"} {
		if v := value(key); v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if event != "" {
		return event
	}
	return "framework log"
}
