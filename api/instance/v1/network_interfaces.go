package instancev1

import (
	"encoding/json"
	"github.com/google/uuid"
	"regexp"
)

func CanonicalNetworkInterfaceIntent(id, slot, mode, vnicID string, version int64) ([]byte, error) {
	u, e := uuid.Parse(id)
	if e != nil || u == uuid.Nil || u.String() != id || version < 1 || slot != "primary" || (mode != "automatic" && mode != "manual") || (mode == "automatic" && vnicID != "") || (mode == "manual" && !regexp.MustCompile(`^vnic_[0-9a-f]{32}$`).MatchString(vnicID)) {
		return nil, ErrInvalidBusiness
	}
	return json.Marshal([]any{id, version, "set-network-interface", slot, mode, vnicID})
}
