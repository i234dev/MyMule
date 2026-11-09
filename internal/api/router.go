package api

import (
	"strings"

	"mymule/internal/portmap"
)

// RouterInfo says which of our ports MyMule opened on the router (UPnP
// or NAT-PMP). Method empty and no error: still trying.
type RouterInfo struct {
	Method string   `json:"method,omitempty"` // "UPnP" or "NAT-PMP"
	Ports  []string `json:"ports,omitempty"`  // "TCP 4662", "UDP 4672"
	Error  string   `json:"error,omitempty"`  // why nothing could be opened
}

// RouterInfoOf converts the mapper's state (nil: upnp is off).
func RouterInfoOf(s *portmap.State) *RouterInfo {
	if s == nil {
		return nil
	}
	r := &RouterInfo{Method: s.Method, Error: s.Error}
	for _, p := range s.Ports {
		r.Ports = append(r.Ports, p.String())
	}
	return r
}

// Summary is a one-line description: "TCP 4662 and UDP 4672 open (UPnP)".
func (r *RouterInfo) Summary() string {
	switch {
	case r == nil:
		return "off (upnp = false: forward the ports by hand)"
	case r.Method != "":
		return strings.Join(r.Ports, " and ") + " open (" + r.Method + ")"
	case r.Error != "":
		return "ports not opened automatically: " + r.Error
	}
	return "trying UPnP / NAT-PMP"
}
