package biz

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type ListVPCs struct {
	TenantID, Name, State, Cursor string
	Limit                         int
	Page, PageSize                *uint32
}

type VPCPage struct {
	Total      int64
	Items      []VPC
	NextCursor string
}

type VPCFilter struct {
	Name, State, AfterID string
	AfterCreatedAt       time.Time
	Limit                int32
	Offset               int64
}

type vpcCursor struct {
	Version     int
	Kind        string
	TenantID    string
	Name, State string
	VPCID       string
	ID          string
	CreatedAt   time.Time
}

func (n *Network) ListVPCs(ctx context.Context, request ListVPCs) (VPCPage, error) {
	tenant, err := ParseTenant(request.TenantID)
	if err != nil {
		return VPCPage{}, err
	}
	if request.Page != nil || request.PageSize != nil {
		if request.Limit != 0 || request.Cursor != "" {
			return VPCPage{}, Fail(InvalidArgument, "page/pageSize cannot be mixed with limit/cursor")
		}
		window, err := ResolvePage(request.Page, request.PageSize)
		if err != nil {
			return VPCPage{}, err
		}
		filter, err := normalizeList(request.Name, request.State, int(window.Limit))
		if err != nil {
			return VPCPage{}, err
		}
		filter.Limit, filter.Offset = window.Limit, window.Offset
		rows, total, err := n.repository.ListVPCs(ctx, tenant, filter)
		if err != nil {
			return VPCPage{}, err
		}
		page := VPCPage{Total: total, Items: make([]VPC, 0, len(rows))}
		for _, value := range rows {
			page.Items = append(page.Items, value)
		}
		return page, nil
	}
	filter, err := normalizeList(request.Name, request.State, request.Limit)
	if err != nil {
		return VPCPage{}, err
	}
	request.Name, request.Limit = filter.Name, int(filter.Limit)-1
	if request.Cursor != "" {
		cursor, err := n.decodeCursor(request.Cursor)
		if err != nil || cursor.Version != 1 || cursor.Kind != "vpc" ||
			cursor.TenantID != tenant || cursor.Name != request.Name || cursor.State != request.State ||
			!validVPCID(cursor.ID) || cursor.CreatedAt.IsZero() {
			return VPCPage{}, Fail(InvalidCursor, "cursor does not match this query")
		}
		filter.AfterCreatedAt, filter.AfterID = cursor.CreatedAt, cursor.ID
	}
	rows, total, err := n.repository.ListVPCs(ctx, tenant, filter)
	if err != nil {
		return VPCPage{}, err
	}
	page := VPCPage{Total: total, Items: make([]VPC, 0, request.Limit)}
	for index, value := range rows {
		if index == request.Limit {
			last := rows[index-1]
			page.NextCursor = n.encodeCursor(vpcCursor{
				Version: 1, Kind: "vpc", TenantID: tenant, Name: request.Name, State: request.State,
				ID: last.ID, CreatedAt: last.CreatedAt,
			})
			break
		}
		page.Items = append(page.Items, value)
	}
	return page, nil
}

func (n *Network) encodeCursor(cursor vpcCursor) string {
	body, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, n.cursorKey)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (n *Network) decodeCursor(value string) (vpcCursor, error) {
	var cursor vpcCursor
	if len(value) > 2048 {
		return cursor, Fail(InvalidCursor, "cursor too long")
	}
	left, right, ok := strings.Cut(value, ".")
	if !ok {
		return cursor, Fail(InvalidCursor, "invalid cursor")
	}
	body, err := base64.RawURLEncoding.DecodeString(left)
	if err != nil || base64.RawURLEncoding.EncodeToString(body) != left {
		return cursor, Fail(InvalidCursor, "invalid cursor")
	}
	signature, err := base64.RawURLEncoding.DecodeString(right)
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != right {
		return cursor, Fail(InvalidCursor, "invalid cursor")
	}
	mac := hmac.New(sha256.New, n.cursorKey)
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(body, &cursor) != nil {
		return cursor, Fail(InvalidCursor, "invalid cursor")
	}
	return cursor, nil
}

func normalizeList(name, state string, limit int) (VPCFilter, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return VPCFilter{}, Fail(InvalidArgument, "limit must be within 1..100")
	}
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 128 {
		return VPCFilter{}, Fail(InvalidArgument, "invalid name filter")
	}
	for _, value := range name {
		if unicode.IsControl(value) {
			return VPCFilter{}, Fail(InvalidArgument, "invalid name filter")
		}
	}
	switch ResourceState(state) {
	case "", Provisioning, Available, Degraded, Failed, Deleting, Deleted:
	default:
		return VPCFilter{}, Fail(InvalidArgument, "invalid state filter")
	}
	return VPCFilter{Name: name, State: state, Limit: int32(limit + 1)}, nil
}
