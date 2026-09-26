// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAccProtoV6ProviderFactories wires our provider for the acceptance harness.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"omada": providerserver.NewProtocol6WithError(New("test")()),
}

// TestProviderSchema is a pure unit test (always runs) that the provider's
// schema and metadata build without diagnostics.
func TestProviderSchema(t *testing.T) {
	p := New("test")()

	schemaResp := &provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("provider schema has errors: %v", schemaResp.Diagnostics)
	}
	for _, attr := range []string{"url", "username", "password", "skip_tls_verify", "site"} {
		if _, ok := schemaResp.Schema.Attributes[attr]; !ok {
			t.Errorf("provider schema missing attribute %q", attr)
		}
	}

	metaResp := &provider.MetadataResponse{}
	p.Metadata(context.Background(), provider.MetadataRequest{}, metaResp)
	if metaResp.TypeName != "omada" {
		t.Errorf("provider type name = %q, want omada", metaResp.TypeName)
	}
}

// newMockController returns an httptest server that emulates the Omada v6
// info + login handshake plus the sites and networks list endpoints, so
// acceptance tests run in CI without a real controller.
func newMockController(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{
			"omadacId": "abc123", "controllerVer": "6.0.0", "apiVer": "3", "type": 1,
		})
	})

	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})

	requireToken := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Csrf-Token") != "tok-xyz" {
			writeEnvelope(w, -1400, "invalid csrf token", nil)
			return false
		}
		return true
	}

	mux.HandleFunc("/abc123/api/v2/sites", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows": 1, "currentPage": 1, "currentSize": 100,
			"data": []map[string]any{{"id": "site-1", "name": "Default"}},
		})
	})

	// Stateful LAN-networks store, seeded with one network for the data-source
	// tests and mutated by the resource acceptance test (create/update/delete).
	var mu sync.Mutex
	nextID := 1
	networks := map[string]map[string]any{
		"net-1": {
			"id": "net-1", "name": "IoT", "purpose": "interface",
			"vlan": 30, "vlanType": 0, "application": 0,
			"gatewaySubnet": "192.168.30.1/24",
			// per-VLAN switching/security toggles
			"isolation": true, "allLan": false, "portal": false, "rateLimit": false,
			"qosQueueEnable": false, "accessControlRule": true, "arpDetectionEnable": true,
			"igmpSnoopEnable": false, "fastLeaveEnable": false, "mldSnoopEnable": false,
			"dhcpL2RelayEnable":    false,
			"dhcpGuard":            map[string]any{"enable": false},
			"dhcpv6Guard":          map[string]any{"enable": false},
			"lanNetworkIpv6Config": map[string]any{"enable": 0},
			// derived keys the provider must preserve on update
			"ipRangePool": []map[string]any{{"ipaddrStart": "192.168.30.2", "ipaddrEnd": "192.168.30.254"}},
			"totalIpNum":  253,
			"dhcpSettings": map[string]any{
				"enable": true, "ipaddrStart": "192.168.30.2", "ipaddrEnd": "192.168.30.254",
				"leasetime": 120, "dhcpns": "auto",
				"options": []map[string]any{{"code": 138, "type": 1, "value": "10.10.20.50"}},
			},
		},
	}
	const netBase = "/abc123/api/v2/sites/site-1/setting/lan/networks"

	// Collection: GET (list) + POST (create).
	mux.HandleFunc(netBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			// Create behaves differently per purpose, and both halves matter.
			//
			// An "interface" network is refused here: on a site with a gateway
			// the real controller rejects a web-API create, and it only exists
			// on the Open API. A provider that regressed to POSTing interface
			// networks here would pass a permissive mock and fail on hardware.
			//
			// A "vlan" network IS created here. Verified on a gateway-less
			// v6.2.14.11 site: four fields, no gatewaySubnet, no interfaceIds,
			// and the id comes back as a bare JSON string. The controller fills
			// in interfaceIds and allLan itself, which this mock reproduces so
			// that a provider sending either one is caught.
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeEnvelope(w, -1001, "bad body", nil)
				return
			}
			if p, _ := in["purpose"].(string); p != "vlan" {
				writeEnvelope(w, -1005, "operation forbidden", nil)
				return
			}
			if _, ok := in["igmpSnoopEnable"]; !ok {
				writeEnvelope(w, -1001, "Parameter [igmpSnoopEnable] should not be null.", nil)
				return
			}
			vlan, ok := in["vlan"].(float64)
			if !ok || vlan < 1 || vlan > 4094 {
				writeEnvelope(w, -1001, "Value of vlan is from 1 to 4094.", nil)
				return
			}
			for _, forbidden := range []string{"gatewaySubnet", "interfaceIds"} {
				if v, present := in[forbidden]; present && v != nil {
					// The controller owns these on this path. The provider must
					// never send them; failing loudly beats accepting silently.
					writeEnvelope(w, -1001, "Parameter ["+forbidden+"] is not allowed here.", nil)
					return
				}
			}
			nextID++
			id := fmt.Sprintf("vlan-net-%d", nextID)
			in["id"] = id
			in["vlanType"] = float64(0)
			// Server-assigned, exactly as the live controller did.
			in["interfaceIds"] = []any{"2_aaa", "3_bbb"}
			in["allLan"] = true
			in["primary"] = false
			networks[id] = in
			// The id comes back as a bare string, not an object.
			writeEnvelope(w, 0, "Success.", id)
		default: // GET
			data := make([]map[string]any, 0, len(networks))
			for _, n := range networks {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})

	// Network create lives only on the Open API, and only on v2.
	mux.HandleFunc("/openapi/v2/abc123/sites/site-1/lan-networks", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "AccessToken=oa-token" {
			writeEnvelope(w, -44116, "Open API Authorized failed", nil)
			return
		}
		if r.Method != http.MethodPost {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		// The endpoint requires exactly these four and the provider seeds only
		// them; anything else means it started sending web-API shape here.
		for _, k := range []string{"name", "purpose", "vlan", "igmpSnoopEnable", "interfaceIds", "gatewaySubnet"} {
			if _, ok := in[k]; !ok {
				writeEnvelope(w, -1001, "Parameter ["+k+"] should not be empty", nil)
				return
			}
		}
		if len(in) != 6 {
			writeEnvelope(w, -1001, "unexpected fields in create body", nil)
			return
		}
		// purpose crosses the surface boundary as an int here and a string on the
		// web API. Rejecting the string is what makes the translation testable.
		if _, isNum := in["purpose"].(float64); !isNum {
			writeEnvelope(w, -1001, "Invalid request parameters.", nil)
			return
		}
		// A network must be bound to a LAN interface, like the real controller.
		if ifaces, _ := in["interfaceIds"].([]any); len(ifaces) == 0 {
			writeEnvelope(w, -33515, "LAN interfaces could not be none.", nil)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		id := fmt.Sprintf("gen-%d", nextID)
		nextID++
		// Store the *web API's* representation, not the body just received:
		// that is what a later GET on /setting/lan/networks returns, and the
		// two surfaces spell purpose differently. Echoing the Open API body
		// back would hide exactly the mismatch this route exists to catch.
		networks[id] = map[string]any{
			"id": id, "name": in["name"], "purpose": "interface",
			"vlan": in["vlan"], "gatewaySubnet": in["gatewaySubnet"],
			"interfaceIds": in["interfaceIds"], "igmpSnoopEnable": in["igmpSnoopEnable"],
			"vlanType": 0, "application": 0,
			// A freshly created network really does come back with the IPv6
			// block present but missing `proto`, which is what makes the
			// follow-up update fail without the client-side default.
			"lanNetworkIpv6Config": map[string]any{"enable": 0},
		}
		// Create answers without an id, so the provider must find it by name.
		writeEnvelope(w, 0, "Success.", nil)
	})

	// Item: PATCH (update) + DELETE.
	mux.HandleFunc(netBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, netBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(networks, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			// The real controller is asymmetric here: its GET returns
			// lanNetworkIpv6Config as {enable} but its PATCH rejects the object
			// unless `proto` is also present. Emulating that is what keeps the
			// client-side default honest -- without this the provider could
			// stop sending proto and only fail on hardware.
			if v6, ok := in["lanNetworkIpv6Config"].(map[string]any); ok {
				if _, has := v6["proto"]; !has {
					writeEnvelope(w, -1001, "Parameter [proto] should not be null.", nil)
					return
				}
			}
			cur := networks[id]
			if cur == nil {
				cur = map[string]any{}
			}
			for k, v := range in {
				cur[k] = v
			}
			cur["id"] = id
			networks[id] = cur
			writeEnvelope(w, 0, "", cur)
		}
	})

	// Stateful LAN DNS store. Create returns a null result (like the real
	// controller), so the client resolves the new record by name via GET.
	dns := map[string]map[string]any{}
	dnsNext := 1
	const dnsBase = "/abc123/api/v2/sites/site-1/setting/lan/dns"
	mux.HandleFunc(dnsBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("dns-%d", dnsNext)
			dnsNext++
			in["id"] = id
			dns[id] = in
			writeEnvelope(w, 0, "", nil)
		default: // GET
			data := make([]map[string]any, 0, len(dns))
			for _, n := range dns {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(dnsBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, dnsBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(dns, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			cur := dns[id]
			if cur == nil {
				cur = map[string]any{}
			}
			for k, v := range in {
				cur[k] = v
			}
			cur["id"] = id
			dns[id] = cur
			writeEnvelope(w, 0, "", cur)
		}
	})

	// Stateful port-forward store, seeded with one rule so the WAN-port default
	// can be inferred (mirrors a real controller with an existing rule).
	pf := map[string]map[string]any{
		"pf-seed": {"id": "pf-seed", "name": "seed", "status": true, "protocol": 1,
			"externalPort": "80", "forwardIp": "10.10.20.9", "forwardPort": "80",
			"interfaceWanPortId": []any{"wan-1"}, "virtualWanId": []any{}, "dMZ": false},
	}
	pfNext := 1
	const pfBase = "/abc123/api/v2/sites/site-1/setting/transmission/portForwardings"
	mux.HandleFunc(pfBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !validPortForwardProtocol(in["protocol"]) {
				writeEnvelope(w, -1001, "must be between 0 and 2", nil)
				return
			}
			id := fmt.Sprintf("pf-%d", pfNext)
			pfNext++
			in["id"] = id
			pf[id] = in
			writeEnvelope(w, 0, "", nil)
		default: // GET
			data := make([]map[string]any, 0, len(pf))
			for _, n := range pf {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"totalRows": len(data), "data": data})
		}
	})
	mux.HandleFunc(pfBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, pfBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(pf, id)
			writeEnvelope(w, 0, "", nil)
		default: // PUT
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !validPortForwardProtocol(in["protocol"]) {
				writeEnvelope(w, -1001, "must be between 0 and 2", nil)
				return
			}
			in["id"] = id
			pf[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful IP-group store. Item operations use /groups/{type}/{groupId}.
	groups := map[string]map[string]any{}
	grpNext := 1
	const grpBase = "/abc123/api/v2/sites/site-1/setting/profiles/groups"
	mux.HandleFunc(grpBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("grp-%d", grpNext)
			grpNext++
			in["groupId"] = id
			if in["type"] == nil {
				in["type"] = 0
			}
			groups[id] = in
			writeEnvelope(w, 0, "", nil)
		default: // GET
			data := make([]map[string]any, 0, len(groups))
			for _, n := range groups {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(grpBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, grpBase+"/"), "/")
		id := parts[len(parts)-1]
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(groups, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			cur := groups[id]
			if cur == nil {
				cur = map[string]any{}
			}
			for k, v := range in {
				cur[k] = v
			}
			cur["groupId"] = id
			groups[id] = cur
			writeEnvelope(w, 0, "", cur)
		}
	})

	// Stateful firewall-ACL store (POST create, GET ?type=N, PUT /{id}, DELETE).
	// Seeded with one gateway rule so the omada_firewall_acls data source has
	// something to list; the resource tests only assert on rules they create.
	acls := map[string]map[string]any{
		"acl-seed": {"id": "acl-seed", "type": 0 /* gateway */, "name": "seed",
			"status": true, "policy": 1},
	}
	aclNext := 1
	const aclBase = "/abc123/api/v2/sites/site-1/setting/firewall/acls"
	mux.HandleFunc(aclBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("acl-%d", aclNext)
			aclNext++
			in["id"] = id
			acls[id] = in
			writeEnvelope(w, 0, "", nil)
		default: // GET — filter by ?type=N like the real controller
			want := r.URL.Query().Get("type")
			data := make([]map[string]any, 0, len(acls))
			for _, n := range acls {
				if want != "" && fmt.Sprintf("%v", n["type"]) != want {
					continue
				}
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"totalRows": len(data), "data": data})
		}
	})
	mux.HandleFunc(aclBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, aclBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(acls, id)
			writeEnvelope(w, 0, "", nil)
		default: // PUT
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			acls[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful WLAN-group store (POST create, GET list, PATCH /{id}, DELETE).
	wlans := map[string]map[string]any{}
	wlanNext := 1
	const wlanBase = "/abc123/api/v2/sites/site-1/setting/wlans"
	mux.HandleFunc(wlanBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("wlan-%d", wlanNext)
			wlanNext++
			in["id"] = id
			in["primary"] = false
			wlans[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(wlans))
			for _, n := range wlans {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(wlanBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, wlanBase+"/")
		if strings.Contains(id, "/") { // e.g. .../wlans/{id}/ssids — not a group item
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(wlans, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			cur := wlans[id]
			if cur == nil {
				cur = map[string]any{}
			}
			for k, v := range in {
				cur[k] = v
			}
			cur["id"] = id
			wlans[id] = cur
			writeEnvelope(w, 0, "", cur)
		}
	})

	// Stateful mDNS store (POST create, GET list, PUT /{id}, DELETE).
	mdns := map[string]map[string]any{}
	mdnsNext := 1
	const mdnsBase = "/abc123/api/v2/sites/site-1/setting/service/mdns"
	mux.HandleFunc(mdnsBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("mdns-%d", mdnsNext)
			mdnsNext++
			in["id"] = id
			mdns[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(mdns))
			for _, n := range mdns {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"totalRows": len(data), "data": data})
		}
	})
	mux.HandleFunc(mdnsBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, mdnsBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(mdns, id)
			writeEnvelope(w, 0, "", nil)
		default: // PUT
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			mdns[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful port-profile store (POST create, GET list, PATCH /{id}, DELETE).
	profs := map[string]map[string]any{}
	profNext := 1
	const profBase = "/abc123/api/v2/sites/site-1/setting/lan/profiles"
	mux.HandleFunc(profBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("prof-%d", profNext)
			profNext++
			in["id"] = id
			// Simulate controller-owned keys the provider must not clobber.
			in["prohibitModify"] = false
			in["flag"] = 2
			// The controller assigns type 2 to every profile a user creates.
			if _, ok := in["type"]; !ok {
				in["type"] = 2
			}
			if stp, ok := in["spanningTreeSetting"].(map[string]any); ok {
				stp["instances"] = []any{}
			}
			profs[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(profs))
			for _, n := range profs {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(profBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, profBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(profs, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			// Controller quirk (6.3.0.45): a PATCH that sets networkTagsSetting
			// 0 ("tag every network") on a custom profile is stored as 2 ("tag
			// a specific list") with the list emptied. POST keeps 0.
			if nts, ok := in["networkTagsSetting"].(float64); ok && nts == 0 {
				if typ, _ := in["type"].(float64); typ != 0 {
					in["networkTagsSetting"] = 2
					in["tagNetworkIds"] = []any{}
				}
			}
			profs[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful SSID store (nested under /wlans/{gid}/ssids — Go 1.22 wildcards).
	ssids := map[string]map[string]any{}
	ssidNext := 1
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/wlans/{gid}/ssids", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			// The real controller rejects a create whose payload omits any of
			// these sub-objects; mirror that so a regression back to an
			// undefaulted schema attribute fails the test instead of
			// silently passing. rateLimit's message is confirmed live; the
			// other three are the same ssidDeepKeys shape, checked the same
			// way, message unconfirmed (the controller answered a generic
			// "-1001 Invalid request parameters" for those live).
			for _, key := range []string{"rateLimit", "ssidRateLimit", "rateAndBeaconCtrl", "dhcpOption82"} {
				if in[key] == nil {
					msg := "Invalid request parameters."
					if key == "rateLimit" {
						msg = "rateLimit should not be null"
					}
					writeEnvelope(w, -1001, msg, nil)
					return
				}
			}
			// Confirmed live: vlanEnable without vlanSetting (which is where
			// the bound omada_network id lives) fails the same generic way.
			// vlanPoolIds inside customConfig is the same story: present on
			// every existing SSID, missing it fails create the same way.
			if vlanEnable, _ := in["vlanEnable"].(bool); vlanEnable {
				vs, _ := in["vlanSetting"].(map[string]any)
				if vs == nil {
					writeEnvelope(w, -1001, "Invalid request parameters.", nil)
					return
				}
				cc, _ := vs["customConfig"].(map[string]any)
				if cc == nil || cc["vlanPoolIds"] == nil {
					writeEnvelope(w, -1001, "Invalid request parameters.", nil)
					return
				}
			}
			// Confirmed live (6.2.14.11): an SSID created without a VLAN
			// binding reads back with an empty customConfig, no lanNetworkId.
			if in["vlanSetting"] == nil {
				in["vlanSetting"] = map[string]any{"mode": 0, "customConfig": map[string]any{}}
			}
			id := fmt.Sprintf("ssid-%d", ssidNext)
			ssidNext++
			in["id"] = id
			ssids[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(ssids))
			for _, n := range ssids {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/wlans/{gid}/ssids/{sid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := r.PathValue("sid")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(ssids, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			ssids[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful VPN store (POST create, GET list, PUT /{id}, DELETE).
	vpns := map[string]map[string]any{}
	vpnNext := 1
	const vpnBase = "/abc123/api/v2/sites/site-1/setting/vpns"
	mux.HandleFunc(vpnBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("vpn-%d", vpnNext)
			vpnNext++
			in["id"] = id
			vpns[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(vpns))
			for _, n := range vpns {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(vpnBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, vpnBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(vpns, id)
			writeEnvelope(w, 0, "", nil)
		default: // PUT
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			vpns[id] = in
			writeEnvelope(w, 0, "", in)
		}
	})

	// Stateful static-route store (POST create, GET list, PUT /{id}, DELETE).
	routes := map[string]map[string]any{}
	routeNext := 1
	const routeBase = "/abc123/api/v2/sites/site-1/setting/transmission/staticRoutings"
	mux.HandleFunc(routeBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("route-%d", routeNext)
			routeNext++
			in["id"] = id
			routes[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(routes))
			for _, n := range routes {
				data = append(data, n)
			}
			writeEnvelope(w, 0, "", map[string]any{"totalRows": len(data), "data": data})
		}
	})
	mux.HandleFunc(routeBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, routeBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(routes, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			routes[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			// The real controller rejects anything but PUT here; mirror that so a
			// regression back to PATCH fails the test instead of silently passing.
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// WAN settings. The real payload mixes config with a large set of read-only
	// support* capability flags, which the data source must ignore. Stateful
	// (unlike most of this mock's read-only siblings) so omada_wan_ipv6's PATCH
	// can be exercised end to end: the item path replaces wanPortIpv6Setting only,
	// leaving wanPortIpv4Setting/wanPortMacSetting untouched, mirroring
	// UpdateWANIPv6Setting's read-modify-write.
	wanPort := map[string]any{
		"portUuid": "wan-1", "portName": "WAN/LAN1",
		"wanPortIpv4Setting": map[string]any{
			"proto": "dhcp", "protoType": 0, "vlanId": 0, "qosTagEnable": false,
			"ipv4Dhcp": map[string]any{"unicast": false, "mtu": 1500},
		},
		"wanPortIpv6Setting": map[string]any{"portUuid": "wan-1", "enable": 0},
		"wanPortMacSetting":  map[string]any{"method": "recover", "mac": "AA-BB-CC-DD-EE-FF"},
	}
	const wanBase = "/abc123/api/v2/sites/site-1/setting/wan/networks"
	mux.HandleFunc(wanBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		writeEnvelope(w, 0, "", map[string]any{
			"supportPppoe": true, "supportIpv6": true, "portNum": 2,
			"wanPortSettings": []map[string]any{wanPort},
		})
	})
	mux.HandleFunc(wanBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPatch {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		// UpdateWANIPv6Setting sends the WHOLE port document back (read-modify-
		// write done client-side), so this only needs to accept it — no merge.
		wanPort = in
		writeEnvelope(w, 0, "", nil)
	})

	// Stateful captive-portal store. Like the real controller: the list result is
	// a bare JSON array, create returns a null result, update is PATCH with a full
	// read-modify-write payload. `simplePassword` is stored but never returned in
	// the list, mirroring the write-only password.
	portals := map[string]map[string]any{}
	portalNext := 1
	const portalBase = "/abc123/api/v2/sites/site-1/setting/portals"
	// portalPublic strips the write-only password from a stored portal.
	portalPublic := func(p map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range p {
			if k == "simplePassword" {
				continue
			}
			out[k] = v
		}
		return out
	}
	mux.HandleFunc(portalBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("portal-%d", portalNext)
			portalNext++
			in["id"] = id
			// Controller-owned defaults the provider must preserve on update.
			if _, ok := in["portalCustomize"]; !ok {
				in["portalCustomize"] = map[string]any{
					"defaultLanguage": 1, "copyrightEnable": false, "buttonText": "Log In",
				}
			}
			portals[id] = in
			writeEnvelope(w, 0, "", nil)
		default: // GET — bare array
			data := make([]map[string]any, 0, len(portals))
			for _, p := range portals {
				data = append(data, portalPublic(p))
			}
			writeEnvelope(w, 0, "", data)
		}
	})
	mux.HandleFunc(portalBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, portalBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(portals, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			cur := portals[id]
			if cur == nil {
				cur = map[string]any{}
			}
			// The real controller rejects a patch whose portalCustomize lost its
			// required keys — assert the provider sent them back.
			if pc, ok := in["portalCustomize"].(map[string]any); ok {
				if _, has := pc["defaultLanguage"]; !has {
					writeEnvelope(w, -1001, "portalCustomize parameter [defaultLanguage] should not be null", nil)
					return
				}
			}
			for k, v := range in {
				cur[k] = v
			}
			cur["id"] = id
			portals[id] = cur
			writeEnvelope(w, 0, "", nil)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})
	// Unauthenticated debug endpoint so tests can assert the stored password.
	mux.HandleFunc("/debug/portals", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(portals)
	})

	// Devices list — result is a bare JSON array (no pagination envelope), like
	// the real controller. Seeded with one switch and one AP.
	mux.HandleFunc("/abc123/api/v2/sites/site-1/devices", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		writeEnvelope(w, 0, "", []map[string]any{
			{"name": "SW-1", "type": "switch", "model": "ES205GP", "mac": "8C-86-DD-10-50-CA",
				"sn": "225", "ip": "10.10.99.2", "status": 14, "statusCategory": 1,
				"firmwareVersion": "1.0.6 Build 20260329", "version": "1.0.6",
				"needUpgrade": false, "uptimeLong": 5656402, "clientNum": 0, "hwVersion": "1.0"},
			{"name": "AP-1", "type": "ap", "model": "EAP610", "mac": "60-83-E7-4B-1B-40",
				"sn": "226", "ip": "10.10.99.4", "status": 14, "statusCategory": 1,
				"firmwareVersion": "1.2.3", "version": "1.2.3",
				"needUpgrade": true, "uptimeLong": 1000, "clientNum": 7, "hwVersion": "1.0"},
		})
	})

	// Switch ports — the provider's one cross-surface object. The mock keeps
	// both halves honest: the read is served on the web API, and the write is
	// only accepted on the Open API with a bearer token from the grant below.
	// A test that broke that split (say, by writing through the web API) would
	// fail here rather than only against a real controller.
	switchPorts := map[int]map[string]any{}
	for i := 1; i <= 5; i++ {
		switchPorts[i] = map[string]any{
			"port": i, "name": fmt.Sprintf("Port%d", i),
			"profileId": "prof-all", "profileName": "All",
			"profileOverrideEnable": false, "profileVlanOverrideEnable": true,
			"nativeNetworkId": "net-1", "networkTagsSetting": 2, "tagIds": []string{},
			// The controller reports a tag's label alongside its id. Seeded so
			// the provider is shown to surface it rather than leaving the id
			// opaque -- and note it survives even when tagIds is empty, which
			// is exactly the dangling-reference case.
			"tagName": "AP",
			"duplex":  0, "linkSpeed": 0,
			// Read-only keys the provider must ignore rather than echo back.
			"poe": 1, "operation": "switching", "speed": 5, "switchMac": "8C-86-DD-10-50-CA",
		}
	}

	mux.HandleFunc("/abc123/api/v2/sites/site-1/switches/8C-86-DD-10-50-CA/ports", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out := make([]map[string]any, 0, len(switchPorts))
		for i := 1; i <= len(switchPorts); i++ {
			out = append(out, switchPorts[i])
		}
		writeEnvelope(w, 0, "", out)
	})

	mux.HandleFunc("/openapi/authorize/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != "oa-client" || body["client_secret"] != "oa-secret" {
			writeEnvelope(w, -44106, "invalid client", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"accessToken": "oa-token", "tokenType": "bearer", "expiresIn": 7200,
		})
	})

	mux.HandleFunc("/openapi/v1/abc123/sites/site-1/switches/8C-86-DD-10-50-CA/ports/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "AccessToken=oa-token" {
			writeEnvelope(w, -44116, "Open API Authorized failed", nil)
			return
		}
		if r.Method != http.MethodPatch {
			writeEnvelope(w, -1600, "no such path", nil)
			return
		}
		num, err := strconv.Atoi(pathpkg.Base(r.URL.Path))
		if err != nil {
			writeEnvelope(w, -1600, "no such path", nil)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		cur, ok := switchPorts[num]
		if !ok {
			writeEnvelope(w, -1600, "no such path", nil)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeEnvelope(w, -1001, "bad body", nil)
			return
		}
		// The controller only accepts this key set here; anything else means
		// the provider has started echoing the read document back.
		allowed := map[string]bool{
			"duplex": true, "linkSpeed": true, "name": true, "nativeNetworkId": true,
			"networkTagsSetting": true, "profileId": true, "profileOverrideEnable": true,
			"profileVlanOverrideEnable": true, "tagIds": true,
		}
		for k := range body {
			if !allowed[k] {
				writeEnvelope(w, -1001, "unexpected key "+k, nil)
				return
			}
			cur[k] = body[k]
		}
		// profileName is derived, not written: keep it consistent with profileId
		// so the provider's read-back sees what a real controller would return.
		if id, _ := cur["profileId"].(string); id == "prof-main" {
			cur["profileName"] = "MAIN"
		} else {
			cur["profileName"] = "All"
		}
		writeEnvelope(w, 0, "Success.", nil)
	})

	// Site-settings singleton (GET /setting object, PATCH merges top-level groups).
	// deviceAccount is included deliberately: the provider must never send it,
	// so it should survive every update untouched.
	siteSettings := map[string]any{
		"led":                      map[string]any{"enable": true},
		"lldp":                     map[string]any{"enable": true},
		"advancedFeature":          map[string]any{"enable": true},
		"autoUpgrade":              map[string]any{"enable": false},
		"channelLimit":             map[string]any{"enable": false},
		"rememberDevice":           map[string]any{"enable": true},
		"airtimeFairness":          map[string]any{"enable2g": false, "enable5g": false, "enable6g": false},
		"alert":                    map[string]any{"enable": false, "delayEnable": true, "delay": 60},
		"bandSteering":             map[string]any{"enable": false, "connectionThreshold": 30, "differenceThreshold": 4, "maxFailures": 5},
		"bandSteeringForMultiBand": map[string]any{"mode": 1},
		"mesh":                     map[string]any{"meshEnable": true, "autoFailoverEnable": true, "defGatewayEnable": true, "fullSector": true},
		"remoteLog":                map[string]any{"enable": false, "port": 514, "moreClientLog": false},
		"speedTest":                map[string]any{"enable": false, "interval": 120},
		"roaming": map[string]any{
			"fastRoamingEnable": true, "aiRoamingEnable": false, "dualBand11kReportEnable": true,
			"forceDisassociationEnable": false, "nonStickRoamingEnable": false, "nonPingPongRoamingEnable": false,
		},
		"beaconControl": map[string]any{
			"beaconIntvMode2g": 0, "dtimPeriod2g": 1, "rtsThreshold2g": 2347, "fragmentationThreshold2g": 2346,
			"beaconIntvMode5g": 0, "dtimPeriod5g": 1, "rtsThreshold5g": 2347, "fragmentationThreshold5g": 2346,
			"beaconInterval6g": 100, "beaconIntvMode6g": 0, "dtimPeriod6g": 1, "rtsThreshold6g": 2347,
			"fragmentationThreshold6g": 2346,
		},
		// #nosec G101 -- test fixture, not a credential: it exists purely to assert
		// the provider never sends deviceAccount in its patch.
		"deviceAccount": map[string]any{"username": "device-admin", "password": "must-not-be-touched"},
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPatch {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			for k, v := range in {
				siteSettings[k] = v
			}
		}
		writeEnvelope(w, 0, "", siteSettings)
	})

	// Flat singleton settings documents (ALG, attack defense). Three real
	// controller behaviours are emulated here because the provider depends on
	// each of them:
	//
	//   - PATCH is rejected with -1600; these endpoints require PUT.
	//   - reads carry read-only metadata (`resource`, `support*`, `exist*`) that
	//     the provider must strip before writing back — sending any of it is a
	//     hard error here, so a regression fails the test rather than silently
	//     shipping junk to a real gateway.
	//   - `unmodelledKey` stands in for controller keys the provider does not
	//     model; it must survive an update (read-modify-write).
	// Update verb per endpoint — the controller is not consistent, and the
	// provider records the confirmed verb per SettingDoc. The mock enforces it:
	// the wrong verb answers -1600 exactly as the real controller does.
	singletonVerb := map[string]string{
		"transmission/alg":               http.MethodPut,
		"transmission/bandwidthControls": http.MethodPut,
		"accessControl":                  http.MethodPatch,
		"transmission/sessionLimits":     http.MethodPut,
		"firewall/attackdefense":         http.MethodPut,
		"ssh":                            http.MethodPut,
		"dot1x":                          http.MethodPatch,
		"macAuth":                        http.MethodPatch,
		"ips":                            http.MethodPatch,
		"snmp":                           http.MethodPut,
		"upnp":                           http.MethodPut,
		"firewall/macfilter":             http.MethodPut,
		"vpns/greTunnel":                 http.MethodPut,
	}
	singletons := map[string]map[string]any{
		"ssh": {
			"sshEnable": false, "sshServerPort": float64(22), "layer3Access": false,
			"unmodelledKey": "keep-me",
		},
		"dot1x": {
			"enable": false, "authMode": float64(1), "authType": float64(1),
			"macFormat": float64(0), "vlanAssign": false,
			"unmodelledKey": "keep-me",
		},
		// SNMP. Seeded with a v3 password so the test can prove three things:
		// the secret reaches the controller, it survives an update that does
		// not re-supply it, and it never reaches Terraform state even though
		// the controller hands it back in plaintext on every read.
		// nolint below: gosec's G101 sees a "password" key with a literal value
		// and flags a hardcoded credential. This is a mock controller's seed
		// data — the whole point is that a fake secret exists to assert it
		// never reaches Terraform state.
		"snmp": { //nolint:gosec // test fixture, not a real credential
			"snmpV1V2CEnable": false, "snmpV3Enable": true,
			"username": "test", "password": "seeded-v3-password",
			"securityLevel": float64(1), "authMode": float64(1), "privacyMode": float64(1),
			"unmodelledKey": "keep-me",
		},
		"firewall/macfilter": {"enable": false, "unmodelledKey": "keep-me"},
		"vpns/greTunnel": {
			"greEnable": false, "relatedSsidList": []any{},
			"unmodelledKey": "keep-me",
		},
		"upnp": {"enable": false, "unmodelledKey": "keep-me"},
		"macAuth": {
			"enable": false, "authType": float64(0), "ssids": []any{},
			"unmodelledKey": "keep-me",
		},
		// Gateway bandwidth control. The read nests settings under
		// `bandwidthControl` while the write takes them flat, so the handler
		// rejects a write containing the nested object — that asymmetry is the
		// whole reason the scaffold grew dotted keys.
		"transmission/bandwidthControls": {
			"bandwidthControl": map[string]any{
				"bandwidthControlEnable": false, "thresholdControlEnable": false,
				"thresholdValue": float64(80),
			},
			"table":         map[string]any{"totalRows": float64(0), "data": []any{}},
			"unmodelledKey": "keep-me",
		},
		// Portal access control. The policy lists must survive an update.
		"accessControl": {
			"preAuthAccessEnable": false, "preAuthAccessPolicies": []any{},
			"freeAuthClientEnable": false, "freeAuthClientPolicies": []any{},
			"unmodelledKey": "keep-me",
		},
		// Session limits. The `table` of per-host rules is controller-owned as
		// far as this resource is concerned: it must survive an update and
		// must never be written back, so the mock rejects a write containing it.
		"transmission/sessionLimits": {
			"sessionLimitEnable": false, "sessionLimitMaxSize": float64(128),
			"ipSessionEnable": true,
			"table":           map[string]any{"totalRows": float64(0), "data": []any{}},
			"unmodelledKey":   "keep-me",
		},
		// IPS. The *Categories lists are controller-owned reference data: the
		// provider must report them but never send them, so this handler
		// rejects a write that includes one.
		"ips": {
			"enable": true, "ipsMode": float64(1), "geoEnable": true, "dpLevel": float64(3),
			"customCategories": []any{float64(1), float64(2)},
			"lowCategories":    []any{float64(2), float64(3)},
			"mediumCategories": []any{float64(1), float64(2), float64(3)},
			"highCategories":   []any{float64(1), float64(2), float64(3), float64(4)},
			"allCategories":    []any{float64(1), float64(2), float64(3), float64(4)},
			"unmodelledKey":    "keep-me",
		},
		"transmission/alg": {
			"ftp": true, "ftpPorts": []any{float64(21)},
			"h323": true, "pptp": true, "ipSec": true,
			"sip": false, "sipTcp": true, "sipUdp": true,
			"sipPorts":           []any{float64(5060), float64(5061)},
			"sipDirectSignaling": true, "sipDirectMedia": false,
			"sipTimeout": false, "sipSignalingTimeout": float64(3600), "sipMediaTimeout": float64(180),
			"unmodelledKey": "keep-me",
		},
		"firewall/attackdefense": {
			"tcpConnEnable": false, "udpConnEnable": false, "icmpConnEnable": false,
			"tcpSrcEnable": false, "udpSrcEnable": false, "icmpSrcEnable": false,
			"tcpNoflagEnable": true, "tcpScanReject": true,
			"tcpWinnukeEnable": true, "tcpFinSynEnable": true, "tcpFinNoackEnable": true,
			"pingDeathEnable": true, "pingLargeEnable": false, "pingWanEnable": true,
			"ipOptionEnable": true, "ipoptSecureEnable": true,
			"ipoptLooseRouteEnable": true, "ipoptStrictRouteEnable": true,
			"ipoptRecordRouteEnable": true, "ipoptStreamEnable": true,
			"ipoptTimestampEnable": true, "ipoptNoopEnable": true,
			"unmodelledKey": "keep-me",
		},
	}
	// The gateway device document. PATCH is a genuine partial update on the real
	// controller, so the mock applies only the keys it receives — a provider
	// that sent a whole read-back document would clobber unrelated settings
	// here and the "untouched" checks would catch it.
	gatewayDoc := map[string]any{
		"mac": "F0-09-0D-D0-97-76", "name": "Mordor", "model": "ER707-M2", "ip": "10.10.99.1",
		"ledSetting": float64(2), "lldpEnable": true, "lldpSetting": float64(1),
		"hwOffloadEnable": true, "ippt": false, "supportIppt": false,
		"snmpSeting":  map[string]any{"location": "", "contact": ""},
		"iptvSetting": map[string]any{"igmpEnable": true, "igmpVersion": "2"},
		// The gateway's physical ports. The provider must never write these.
		"portConfigs":   []any{map[string]any{"port": float64(1), "pvid": float64(0)}},
		"unmodelledKey": "keep-me",
	}
	// --- access point -------------------------------------------------------
	// radioWrites counts how many times a radio document was sent. On real
	// hardware each such write restarts that radio and drops its clients, so
	// the count is the property the acceptance test actually cares about.
	apRadioWrites := map[string]int{}
	apDoc := map[string]any{
		"mac": "10-5A-95-89-C3-64", "name": "10-5A-95-89-C3-64",
		"model": "EAP670", "ip": "192.168.53.100",
		"ledSetting": float64(2), "lldpEnable": float64(2),
		"ofdmaEnable2g": true, "ofdmaEnable5g": true,
		"snmp":            map[string]any{"location": "", "contact": ""},
		"l3AccessSetting": map[string]any{"enable": true},
		"radioSetting2g": map[string]any{
			"radioEnable": true, "channelWidth": "4", "channel": "0",
			"txPower": float64(25), "txPowerLevel": float64(4),
			"freq": float64(0), "wirelessMode": float64(-2),
		},
		"radioSetting5g": map[string]any{
			"radioEnable": true, "channelWidth": "6", "channel": "0",
			"txPower": float64(28), "txPowerLevel": float64(4),
			"freq": float64(0), "wirelessMode": float64(-2),
		},
		"lbSetting2g":   map[string]any{"lbEnable": false, "maxClients": float64(1)},
		"lbSetting5g":   map[string]any{"lbEnable": false, "maxClients": float64(1)},
		"rssiSetting2g": map[string]any{"rssiEnable": false, "threshold": float64(-95)},
		"rssiSetting5g": map[string]any{"rssiEnable": false, "threshold": float64(-95)},
		"unmodelledKey": "keep-me",
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/eaps/10-5A-95-89-C3-64", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			for _, key := range []string{"radioSetting2g", "radioSetting5g"} {
				radio, sent := in[key].(map[string]any)
				if !sent {
					continue
				}
				// The live controller accepts channel, reports success and
				// ignores it. The provider must never send one.
				if _, hasChannel := radio["channel"]; hasChannel {
					writeEnvelope(w, -1001, "must not write "+key+".channel", nil)
					return
				}
				apRadioWrites[key]++
			}
			if _, sent := in["mvlanEnable"]; sent {
				if _, paired := in["mvlanNetworkId"]; !paired {
					writeEnvelope(w, -1001, "mvlanEnable without mvlanNetworkId", nil)
					return
				}
			}
			for k, v := range in {
				apDoc[k] = v
			}
		default: // PUT, POST
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		writeEnvelope(w, 0, "", apDoc)
	})

	// A single-band AP: no radioSetting5g key at all, not merely a zero value —
	// the shape a real single-band model (or a band that isn't adopted) reports.
	// Exists to catch a resource that silently drops a radio_5g_* config value
	// instead of erroring when the device has nothing to compare it against.
	apDoc2G := map[string]any{
		"mac": "AA-BB-CC-11-22-33", "name": "AA-BB-CC-11-22-33",
		"model": "EAP225-Outdoor", "ip": "192.168.53.101",
		"ledSetting": float64(2), "lldpEnable": float64(2),
		"ofdmaEnable2g": true,
		"snmp":          map[string]any{"location": "", "contact": ""},
		"radioSetting2g": map[string]any{
			"radioEnable": true, "channelWidth": "4", "channel": "0",
			"txPower": float64(25), "txPowerLevel": float64(4),
			"freq": float64(0), "wirelessMode": float64(-2),
		},
		"lbSetting2g":   map[string]any{"lbEnable": false, "maxClients": float64(1)},
		"rssiSetting2g": map[string]any{"rssiEnable": false, "threshold": float64(-95)},
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/eaps/AA-BB-CC-11-22-33", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if _, sent := in["radioSetting5g"]; sent {
				writeEnvelope(w, -1001, "no 5g radio on this device", nil)
				return
			}
			for k, v := range in {
				apDoc2G[k] = v
			}
		default: // PUT, POST
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		writeEnvelope(w, 0, "", apDoc2G)
	})

	mux.HandleFunc("/abc123/api/v2/sites/site-1/gateways/F0-09-0D-D0-97-76", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			for _, k := range []string{"portConfigs", "portGeneralConfigs"} {
				if _, sent := in[k]; sent {
					writeEnvelope(w, -1001, "must not write "+k+" here", nil)
					return
				}
			}
			for k, v := range in {
				gatewayDoc[k] = v
			}
		default: // PUT, POST
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		out := map[string]any{}
		for k, v := range gatewayDoc {
			out[k] = v
		}
		writeEnvelope(w, 0, "", out)
	})

	mux.HandleFunc("/debug/accessPoint", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		radios := map[string]any{}
		for k, v := range apRadioWrites {
			radios[k] = float64(v)
		}
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{
			"accessPoint": apDoc,
			"radioWrites": radios,
		})
	})

	mux.HandleFunc("/debug/gateway", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]map[string]any{"gateway": gatewayDoc})
	})

	// DNS proxy. defaultServers is the firmware's list — the provider may flip
	// each entry's `enable` but must never add, drop or renumber one, so the
	// handler rejects a write whose type set does not match. customizedServers
	// is the practitioner's and is replaced wholesale.
	dnsProxyDoc := map[string]any{
		"enable": true,
		"dohSetting": map[string]any{
			"defaultServers": []any{
				map[string]any{"type": float64(0), "enable": false},
				map[string]any{"type": float64(1), "enable": false},
				map[string]any{"type": float64(5), "enable": false},
			},
			"customizedServers": []any{
				map[string]any{"name": "filtered", "enable": true, "servers": []any{"https://dns.example.com/dns-query"}},
			},
		},
		"supportDnsOverride": true,
		"existDnsOverride":   false,
		"dohServerLimit":     float64(32),
		"dotServerLimit":     float64(32),
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/dns-proxy", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			inDoH, _ := in["dohSetting"].(map[string]any)
			curDoH, _ := dnsProxyDoc["dohSetting"].(map[string]any)
			inDef, _ := inDoH["defaultServers"].([]any)
			curDef, _ := curDoH["defaultServers"].([]any)
			if len(inDef) != len(curDef) {
				writeEnvelope(w, -1001, "the firmware server list may not be added to or removed from", nil)
				return
			}
			for i, d := range inDef {
				dm, _ := d.(map[string]any)
				cm, _ := curDef[i].(map[string]any)
				if dm["type"] != cm["type"] {
					writeEnvelope(w, -1001, "the firmware server list may not be renumbered", nil)
					return
				}
			}
			dnsProxyDoc["enable"] = in["enable"]
			curDoH["defaultServers"] = inDef
			curDoH["customizedServers"] = inDoH["customizedServers"]
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		writeEnvelope(w, 0, "", dnsProxyDoc)
	})

	// IPTV. The port list belongs to the controller: the provider may flip each
	// row's `status` but must never invent, drop or rename a row, so the handler
	// rejects a write whose port set does not match.
	iptvDoc := map[string]any{
		"igmpSetting": map[string]any{"igmpProxyEnable": false, "igmpVersion": "2"},
		"iptvSetting": map[string]any{
			"iptvEnable": false,
			"portConfig": []any{
				map[string]any{"name": "WAN/LAN3", "type": "1", "port": "3_aaa", "status": false, "supportIptv": true, "existIptv": false},
				map[string]any{"name": "WAN/LAN4", "type": "1", "port": "4_bbb", "status": false, "supportIptv": true, "existIptv": false},
			},
		},
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/iptv", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			inIPTV, _ := in["iptvSetting"].(map[string]any)
			curIPTV, _ := iptvDoc["iptvSetting"].(map[string]any)
			inPorts, _ := inIPTV["portConfig"].([]any)
			curPorts, _ := curIPTV["portConfig"].([]any)
			if len(inPorts) != len(curPorts) {
				writeEnvelope(w, -1001, "the port list may not be added to or removed from", nil)
				return
			}
			for i, p := range inPorts {
				pm, _ := p.(map[string]any)
				cm, _ := curPorts[i].(map[string]any)
				if pm["port"] != cm["port"] || pm["name"] != cm["name"] {
					writeEnvelope(w, -1001, "the port list may not be rewritten", nil)
					return
				}
			}
			iptvDoc["igmpSetting"] = in["igmpSetting"]
			iptvDoc["iptvSetting"] = inIPTV
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		writeEnvelope(w, 0, "", iptvDoc)
	})

	// IoT telemetry servers: full CRUD on the web API.
	iotServers := map[string]map[string]any{}
	iotNextID := 1
	iotBase := "/abc123/api/v2/sites/site-1/setting/iot/servers"

	mux.HandleFunc(iotBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if classes, _ := in["deviceClasses"].([]any); len(classes) == 0 {
				writeEnvelope(w, -1001, "The parameter Device Class is required.", nil)
				return
			}
			for _, srv := range iotServers {
				if srv["name"] == in["name"] {
					writeEnvelope(w, -33249, "This transport stream name already exists.", nil)
					return
				}
			}
			id := fmt.Sprintf("iot-%d", iotNextID)
			iotNextID++
			in["id"] = id
			// `filters` is controller-owned and never modelled by the provider;
			// seeding it here proves the read-modify-write preserves it.
			in["filters"] = map[string]any{"seeded": true}
			in["resource"] = float64(0)
			iotServers[id] = in
			// The real controller stores the server and *then* answers -1
			// "General error". Reproduced here so the provider's list-is-
			// authoritative recovery is exercised in CI: a version that
			// trusted the error code would orphan this row and fail.
			writeEnvelope(w, -1, "General error.", nil)
		default: // GET
			data := make([]map[string]any, 0, len(iotServers))
			for _, srv := range iotServers {
				data = append(data, srv)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})

	mux.HandleFunc(iotBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		id := pathpkg.Base(r.URL.Path)
		cur, ok := iotServers[id]
		if !ok {
			writeEnvelope(w, -1001, "no such iot server", nil)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			// The controller owns `resource`; sending it back is a hard error
			// here so a regression fails in CI.
			if _, sent := in["resource"]; sent {
				writeEnvelope(w, -1001, "read-only key resource must not be sent", nil)
				return
			}
			for k, v := range in {
				cur[k] = v
			}
			writeEnvelope(w, 0, "", cur)
		case http.MethodDelete:
			delete(iotServers, id)
			writeEnvelope(w, 0, "", nil)
		default: // PATCH and friends
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// iBeacon profiles. The mock carries an IoT-capable device inventory and
	// enforces it, because that check is exactly what could not be exercised on
	// the development site: its EAP610s have no BLE radio, so the controller's
	// own inventory is empty and every create is refused. Emulating it here is
	// the only place the create path runs at all.
	iotCapableAPs := map[string]bool{"60-83-E7-4B-1B-40": true, "DC-62-79-97-7B-82": true}
	iotBeacons := map[string]map[string]any{
		"beacon-default": {
			"id": "beacon-default", "name": "Default", "enable": false,
			"macList": []any{}, "uuid": "00000000000000000000000000000000",
			"major": "0000", "minor": "0000",
			"transmitPower": float64(0), "measurePower": float64(-65), "advInterval": float64(500),
			"boundDeviceNum": float64(0), "buildIn": float64(0),
			"unmodelledKey": "keep-me",
		},
	}
	beaconNextID := 1
	beaconBase := "/abc123/api/v2/sites/site-1/setting/iot/devices/config"

	mux.HandleFunc(beaconBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			macs, _ := in["macList"].([]any)
			if len(macs) == 0 {
				writeEnvelope(w, -33283, "Device List cannot be empty.", nil)
				return
			}
			for _, m := range macs {
				if s, _ := m.(string); !iotCapableAPs[s] {
					writeEnvelope(w, -33284, "The devices in the device list are not in the current site.", nil)
					return
				}
			}
			id := fmt.Sprintf("beacon-%d", beaconNextID)
			beaconNextID++
			in["id"] = id
			in["boundDeviceNum"] = float64(len(macs))
			in["buildIn"] = float64(0)
			iotBeacons[id] = in
			writeEnvelope(w, 0, "", in)
		default: // GET
			data := make([]map[string]any, 0, len(iotBeacons))
			for _, b := range iotBeacons {
				data = append(data, b)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})

	mux.HandleFunc(beaconBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		id := pathpkg.Base(r.URL.Path)
		cur, ok := iotBeacons[id]
		if !ok {
			writeEnvelope(w, -1001, "no such beacon profile", nil)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			// Controller-owned; the provider must not send them back.
			for _, k := range []string{"boundDeviceNum", "buildIn", "resource"} {
				if _, sent := in[k]; sent {
					writeEnvelope(w, -1001, "read-only key "+k+" must not be sent", nil)
					return
				}
			}
			for k, v := range in {
				cur[k] = v
			}
			if macs, _ := cur["macList"].([]any); macs != nil {
				cur["boundDeviceNum"] = float64(len(macs))
			}
			writeEnvelope(w, 0, "", cur)
		case http.MethodDelete:
			delete(iotBeacons, id)
			writeEnvelope(w, 0, "", nil)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	mux.HandleFunc("/debug/iotBeacons", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(iotBeacons)
	})

	// The IoT radio is served *only* by the Open API — the web API answers
	// -1600 for the same path — so it gets its own handler rather than joining
	// the loop below. The mock enforces both halves of that: no web-API route
	// is registered for it at all, and this one refuses a request without a
	// bearer token. A provider that tried to read it on the web API would fail
	// here rather than only against real hardware.
	iotRadio := map[string]any{
		// Seeded with a passcode so the test can assert it reaches the
		// controller and never reaches Terraform state.
		"enable": true, "consoleMode": float64(0),
		"passcode":      "seeded-passcode", //nolint:gosec // test fixture, not a real credential
		"transmitPower": float64(0), "agingTime": float64(30), "format": float64(0),
		"resource": float64(0), "unmodelledKey": "keep-me",
	}
	mux.HandleFunc("/openapi/v1/abc123/sites/site-1/setting/iot/radio", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "AccessToken=oa-token" {
			writeEnvelope(w, -44116, "Open API Authorized failed", nil)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if _, sent := in["resource"]; sent {
				writeEnvelope(w, -1001, "read-only key resource must not be sent", nil)
				return
			}
			for k, v := range in {
				iotRadio[k] = v
			}
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		out := map[string]any{}
		for k, v := range iotRadio {
			out[k] = v
		}
		writeEnvelope(w, 0, "", out)
	})

	// Read-only metadata the controller adds to every read of these documents.
	singletonMeta := map[string]any{
		"resource": float64(0), "supportTcpScanReject": true, "existTcpScanReject": true,
	}
	for suffix, doc := range singletons {
		wantVerb := singletonVerb[suffix]
		mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/"+suffix, func(w http.ResponseWriter, r *http.Request) {
			if !requireToken(w, r) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Method != http.MethodGet && r.Method != wantVerb:
				writeEnvelope(w, -1600, "Unsupported request path.", nil)
				return
			case r.Method == wantVerb:
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				// bandwidthControls is written flat but read back nested, so
				// fold flat writes into the nested object the way the
				// controller does — otherwise a read after write would return
				// the stale nested values and the apply would look
				// inconsistent.
				if nested, ok := doc["bandwidthControl"].(map[string]any); ok {
					for _, k := range []string{"bandwidthControlEnable", "thresholdControlEnable", "thresholdValue"} {
						if v, present := in[k]; present {
							nested[k] = v
							delete(in, k)
						}
					}
				}
				for k := range in {
					if _, isMeta := singletonMeta[k]; isMeta {
						writeEnvelope(w, -1001, "read-only key "+k+" must not be sent", nil)
						return
					}
					switch k {
					case "table", "bandwidthControl",
						"preAuthAccessPolicies", "freeAuthClientPolicies",
						"lowCategories", "mediumCategories", "highCategories", "allCategories":
						writeEnvelope(w, -1001, "controller-owned key "+k+" must not be sent", nil)
						return
					}
				}
				for k, v := range in {
					doc[k] = v
				}
			}
			out := map[string]any{}
			for k, v := range doc {
				out[k] = v
			}
			for k, v := range singletonMeta {
				out[k] = v
			}
			writeEnvelope(w, 0, "", out)
		})
	}
	// Time-range profiles. Two controller quirks are emulated because the client
	// depends on both: the list envelope carries `data` but **no** `totalRows`
	// (the endpoint does not paginate), and create answers with the new id under
	// `profileId` rather than echoing the object.
	timeRanges := map[string]map[string]any{}
	trNext := 1
	const trBase = "/abc123/api/v2/sites/site-1/setting/profiles/timeranges"
	mux.HandleFunc(trBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("tr-%d", trNext)
			trNext++
			in["id"] = id
			// The controller stamps a ruleId onto each slot; the provider must
			// tolerate reading it back without diffing on it.
			if slots, ok := in["timeList"].([]any); ok {
				for i, sl := range slots {
					if m, ok := sl.(map[string]any); ok {
						m["ruleId"] = float64(1000 + i)
					}
				}
			}
			timeRanges[id] = in
			writeEnvelope(w, 0, "", map[string]any{"profileId": id})
		default: // GET
			data := make([]map[string]any, 0, len(timeRanges))
			for _, t := range timeRanges {
				data = append(data, t)
			}
			writeEnvelope(w, 0, "", map[string]any{"data": data})
		}
	})
	mux.HandleFunc(trBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, trBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(timeRanges, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			timeRanges[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// Disable-NAT rules. The asymmetric paths are the point of this handler:
	// the collection is plural (disable-nats) while create and the item path
	// are singular (disable-nat, disable-nat/{id}), and update is PUT — PATCH
	// is rejected, exactly as the controller does.
	disableNats := map[string]map[string]any{}
	dnNext := 1
	const dnList = "/abc123/api/v2/sites/site-1/setting/wired-networks/disable-nats"
	const dnItem = "/abc123/api/v2/sites/site-1/setting/wired-networks/disable-nat"
	mux.HandleFunc(dnList, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		data := make([]map[string]any, 0, len(disableNats))
		for _, d := range disableNats {
			data = append(data, d)
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
		})
	})
	mux.HandleFunc(dnItem, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		iface, _ := in["interface"].(string)
		// One rule per WAN port, like the real controller.
		for _, d := range disableNats {
			if cur, _ := d["interface"].(string); cur == iface {
				writeEnvelope(w, -34247, "Only one Disable NAT rule is allowed for one WAN port.", nil)
				return
			}
		}
		id := fmt.Sprintf("dn-%d", dnNext)
		dnNext++
		in["id"] = id
		disableNats[id] = in
		// The controller does not echo the object; the client resolves by name.
		writeEnvelope(w, 0, "", nil)
	})
	mux.HandleFunc(dnItem+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, dnItem+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(disableNats, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			disableNats[id] = in
			writeEnvelope(w, 0, "", in)
		default: // PATCH and anything else
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// DHCP reservations. The trap this reproduces is that the item path is
	// keyed on the **MAC**, not the id, and the controller answers 0 for a key
	// that matched nothing — so a provider keyed on the id would look like it
	// worked while doing nothing at all.
	reservations := map[string]map[string]any{} // by MAC
	resNext := 1
	const dhcpBase = "/abc123/api/v2/sites/site-1/setting/service/dhcp"
	mux.HandleFunc(dhcpBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			mac, _ := in["mac"].(string)
			in["id"] = fmt.Sprintf("res-%d", resNext)
			resNext++
			in["netName"] = "SERVICE"
			// Forced on by the controller no matter what was sent.
			in["exportToIpMacBinding"] = true
			reservations[mac] = in
			writeEnvelope(w, 0, "", in["id"])
		default: // GET
			data := make([]map[string]any, 0, len(reservations))
			for _, d := range reservations {
				data = append(data, d)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})
	mux.HandleFunc(dhcpBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		key := strings.TrimPrefix(r.URL.Path, dhcpBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			// Note: success regardless of whether the key matched, exactly
			// like the controller.
			delete(reservations, key)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPut:
			cur, ok := reservations[key]
			if !ok {
				writeEnvelope(w, -1001, "DHCP Reservation is not exist, please check path param.", nil)
				return
			}
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = cur["id"]
			in["netName"] = "SERVICE"
			in["exportToIpMacBinding"] = true
			delete(reservations, key)
			mac, _ := in["mac"].(string)
			reservations[mac] = in
			writeEnvelope(w, 0, "", in)
		default: // PATCH
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// RADIUS profiles. Like /setting/portals this is a BARE ARRAY, not a
	// paginated envelope. The secret at authServer[].radiusPwd is stored here
	// so the test can assert it reaches the controller, survives an update
	// that does not re-supply it, and never reaches Terraform state.
	radiusProfiles := map[string]map[string]any{}
	radNext := 1
	const radBase = "/abc123/api/v2/sites/site-1/setting/radiusProfiles"
	mux.HandleFunc(radBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("rad-%d", radNext)
			radNext++
			in["radiusProfileId"] = id
			in["builtInServer"] = false
			radiusProfiles[id] = in
			writeEnvelope(w, 0, "", map[string]any{"radiusProfileId": id})
		default: // GET — bare array
			data := make([]map[string]any, 0, len(radiusProfiles))
			for _, p := range radiusProfiles {
				data = append(data, p)
			}
			writeEnvelope(w, 0, "", data)
		}
	})
	mux.HandleFunc(radBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, radBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(radiusProfiles, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["radiusProfileId"] = id
			radiusProfiles[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})
	mux.HandleFunc("/debug/radius", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(radiusProfiles)
	})

	// IPS whitelist. The asymmetric paths are the point: the list is served
	// from a /grid/ view that only answers GET, while create and delete live
	// one level up at /setting/ips/whitelist.
	ipsWhitelist := map[string]map[string]any{}
	ipsNext := 1
	const ipsList = "/abc123/api/v2/sites/site-1/setting/ips/grid/whitelist"
	const ipsItem = "/abc123/api/v2/sites/site-1/setting/ips/whitelist"
	mux.HandleFunc(ipsList, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		data := make([]map[string]any, 0, len(ipsWhitelist))
		for _, e := range ipsWhitelist {
			data = append(data, e)
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
		})
	})
	mux.HandleFunc(ipsItem, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := fmt.Sprintf("ipsw-%d", ipsNext)
		ipsNext++
		in["id"] = id
		ipsWhitelist[id] = in
		// Null result, like the controller: the client resolves by matching.
		writeEnvelope(w, 0, "", nil)
	})
	mux.HandleFunc(ipsItem+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, ipsItem+"/")
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodDelete {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		delete(ipsWhitelist, id)
		writeEnvelope(w, 0, "", nil)
	})

	// Notification documents. PATCH-only with the whole document, and the
	// point of the handler is the keyed entry lists: the provider must patch
	// individual entries by key and leave every other entry — and the
	// controller-owned descriptive fields — untouched.
	notifications := map[string]any{
		"alertEmailSetting": map[string]any{"alertEmailEnable": false, "delayEnable": false, "delay": float64(60)},
		"eventEmailSetting": map[string]any{"eventEmailEnable": false, "delayEnable": false, "delay": float64(60)},
		"webhookSetting":    map[string]any{"webhookEnable": false},
		"recipients":        []any{},
		"alertNotifications": []any{
			map[string]any{"key": "OSW_DET_STORM", "shortMsg": "Switch Detected Storm", "module": "Device",
				"email": true, "webhook": false, "enable": true, "level": "Warning", "deviceTypes": []any{"switch"}},
			map[string]any{"key": "OSW_DET_LOOP", "shortMsg": "Switch Detected Loop", "module": "Device",
				"email": false, "webhook": false, "enable": true, "level": "Warning", "deviceTypes": []any{"switch"}},
		},
		"eventNotifications": []any{
			map[string]any{"key": "DEV_IP_C", "shortMsg": "Device IP Changed", "module": "Device",
				"email": false, "webhook": false, "enable": false, "deviceTypes": []any{"ap", "gateway"}},
		},
	}
	auditNotifications := map[string]any{
		"webhookSetting": map[string]any{"webhookEnable": false},
		"logNotifications": []any{
			map[string]any{"key": "AUTHENTICATION", "shortMsg": "Authentication", "webhook": false},
			map[string]any{"key": "CLIENTS", "shortMsg": "Clients", "webhook": false},
		},
	}
	notifDoc := func(store map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !requireToken(w, r) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch r.Method {
			case http.MethodGet:
			case http.MethodPatch:
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				if len(in) == 0 {
					writeEnvelope(w, -1001, "Invalid request parameters.", nil)
					return
				}
				for k, v := range in {
					store[k] = v
				}
			default:
				writeEnvelope(w, -1600, "Unsupported request path.", nil)
				return
			}
			out := map[string]any{"resource": float64(0)}
			for k, v := range store {
				out[k] = v
			}
			writeEnvelope(w, 0, "", out)
		}
	}
	mux.HandleFunc("/abc123/api/v2/sites/site-1/logs/notification", notifDoc(notifications))
	mux.HandleFunc("/abc123/api/v2/sites/site-1/site/audit-notification", notifDoc(auditNotifications))
	mux.HandleFunc("/debug/notifications", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": notifications, "audit": auditNotifications})
	})

	// Service types. Seeded with a built-in so the test can assert the provider
	// reports `defaultServiceType` without trying to manage it. Create answers
	// with the new id as a BARE STRING; update is PUT (PATCH -> -1600).
	serviceTypes := map[string]map[string]any{
		"BI-ST-SSH": {"id": "BI-ST-SSH", "name": "SSH", "defaultServiceType": true,
			"protocol": float64(0), "sourcePorts": "0-65535", "destPorts": "22-22", "description": "SSH"},
	}
	stNext := 1
	const stBase = "/abc123/api/v2/sites/site-1/setting/profiles/service-type"
	mux.HandleFunc(stBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("st-%d", stNext)
			stNext++
			in["id"] = id
			in["defaultServiceType"] = false
			serviceTypes[id] = in
			writeEnvelope(w, 0, "", id)
		default:
			data := make([]map[string]any, 0, len(serviceTypes))
			for _, v := range serviceTypes {
				data = append(data, v)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})
	mux.HandleFunc(stBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, stBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(serviceTypes, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			in["defaultServiceType"] = false
			serviceTypes[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// mDNS custom service profiles (/setting/profiles/mdns) — distinct from the
	// reflector-rule collection at /setting/service/mdns. Seeded with a
	// built-in so the test can assert the provider reports `defaultProfile`
	// without trying to manage it. Create answers {"profileId": "..."};
	// update is PATCH to the item path.
	mdnsProfiles := map[string]map[string]any{
		"buildIn-1": {"id": "buildIn-1", "name": "AirPlay", "defaultProfile": true,
			"serviceId": []string{"_airplay._tcp.local", "_raop._tcp.local", "_appletv-v2._tcp.local"}},
	}
	mpNext := 1
	const mpBase = "/abc123/api/v2/sites/site-1/setting/profiles/mdns"
	mux.HandleFunc(mpBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("mp-%d", mpNext)
			mpNext++
			in["id"] = id
			in["defaultProfile"] = false
			mdnsProfiles[id] = in
			// Neither service-type's bare id string nor the created object —
			// see CreateMDNSProfile.
			writeEnvelope(w, 0, "", map[string]any{"profileId": id})
		default:
			data := make([]map[string]any, 0, len(mdnsProfiles))
			for _, v := range mdnsProfiles {
				data = append(data, v)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
				"mdnsCustomCurrentProfileNum": len(mdnsProfiles) - 1, "mdnsCustomMaxProfileNum": 5,
			})
		}
	})
	mux.HandleFunc(mpBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, mpBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(mdnsProfiles, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			in["defaultProfile"] = false
			mdnsProfiles[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// QoS bandwidth control. Two controller behaviours matter and are both
	// reproduced: one rule per WAN port (-43310 for a second), and a create
	// that returns a NULL result, so the client must resolve the new rule by
	// its WAN rather than reading an id out of the response.
	bwc := map[string]map[string]any{}
	bwcNext := 1
	const bwcBase = "/abc123/api/v2/sites/site-1/setting/qos/gateway/bwc"
	mux.HandleFunc(bwcBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			wan, _ := in["wan"].(string)
			for _, e := range bwc {
				if cur, _ := e["wan"].(string); cur == wan {
					writeEnvelope(w, -43310, "The WAN Port is being used by other bandwidths.", nil)
					return
				}
			}
			id := fmt.Sprintf("bwc-%d", bwcNext)
			bwcNext++
			in["id"] = id
			bwc[id] = in
			writeEnvelope(w, 0, "", nil)
		default:
			data := make([]map[string]any, 0, len(bwc))
			for _, v := range bwc {
				data = append(data, v)
			}
			writeEnvelope(w, 0, "", map[string]any{
				"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data,
			})
		}
	})
	mux.HandleFunc(bwcBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, bwcBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(bwc, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			bwc[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// Rate-limit profiles. Bare array, create returns {"id": ...}, update is
	// PATCH. The seeded built-in exists so the test can check `is_builtin` is
	// reported; the mock also drops a limit whose enable flag is false, which
	// is what the controller does and what the plan-time validation guards.
	rateLimits := map[string]map[string]any{
		"rl-default": {"id": "rl-default", "name": "Default", "isDefault": true,
			"downLimitEnable": false, "upLimitEnable": false},
	}
	rlNext := 1
	const rlBase = "/abc123/api/v2/sites/site-1/setting/profiles/rateLimits"
	normaliseRL := func(m map[string]any) {
		if e, _ := m["downLimitEnable"].(bool); !e {
			delete(m, "downLimit")
		}
		if e, _ := m["upLimitEnable"].(bool); !e {
			delete(m, "upLimit")
		}
	}
	mux.HandleFunc(rlBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := fmt.Sprintf("rl-%d", rlNext)
			rlNext++
			in["id"] = id
			in["isDefault"] = false
			normaliseRL(in)
			rateLimits[id] = in
			writeEnvelope(w, 0, "", map[string]any{"id": id})
		default:
			data := make([]map[string]any, 0, len(rateLimits))
			for _, v := range rateLimits {
				data = append(data, v)
			}
			writeEnvelope(w, 0, "", data)
		}
	})
	mux.HandleFunc(rlBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, rlBase+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			delete(rateLimits, id)
			writeEnvelope(w, 0, "", nil)
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = id
			in["isDefault"] = false
			normaliseRL(in)
			rateLimits[id] = in
			writeEnvelope(w, 0, "", in)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	// Controller settings: the one CONTROLLER-scoped document, so the path has
	// no site in it. Two live behaviours are emulated because the provider
	// depends on both:
	//
	//   - A section sent PARTIALLY is rejected. On the real controller
	//     `general`, `deviceManage`, `loggingLevel` and `certificate` answer
	//     -1/-1001 to a body carrying only some of their fields, so the
	//     provider reads and sends each section whole. A regression that sent
	//     a single changed field would pass a permissive mock and fail on
	//     hardware, so this one counts keys and refuses.
	//   - Sections NOT named in the body are untouched, and `unmodelledKey`
	//     inside a section must survive an update of that section.
	controllerSettings := map[string]any{
		"general": map[string]any{
			"name": "Omada Controller", "timeZone": "UTC", "region": "United States",
			"ntpEnable": false, "ntpServers": []any{},
		},
		"webPort": map[string]any{
			"manageHttpPort": float64(8088), "manageHttpsPort": float64(8043),
			"portalHttpPort": float64(8088), "portalHttpsPort": float64(8843),
			"hostName": "", "autoRefresh": true, "autoPortalIpEnable": true,
			"portalHttpsRedirect": false,
		},
		"deviceManage": map[string]any{
			"deviceHostEnable": false, "deviceHost": "",
		},
		"deviceAccessManagement": map[string]any{
			"webControlHttp": false, "webControlHttps": true, "appDiscovery": true,
			"unmodelledKey": "keep-me",
		},
		"firmware":     map[string]any{"controllerNotification": true},
		"loggingLevel": map[string]any{"type": "AUTO", "other": "INFO", "manager": "INFO", "client": "INFO", "device": "INFO", "monitor": "INFO", "system": "INFO", "account": "INFO", "log": "INFO"},
		// Never modelled by the provider: it carries SMTP credentials. It must
		// survive every update untouched.
		"mailServer": map[string]any{"enable": false, "password": "seeded-smtp-password"}, //nolint:gosec // test fixture, not a real credential
	}
	// Fields each section must carry in full when it is written, mirroring the
	// live controller's refusal of a partial section.
	controllerSectionArity := map[string]int{
		"general": 5, "webPort": 8, "deviceManage": 2,
		"deviceAccessManagement": 3, "firmware": 1, "loggingLevel": 9,
	}

	mux.HandleFunc("/abc123/api/v2/controller/setting", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			writeEnvelope(w, 0, "", controllerSettings)
		case http.MethodPatch:
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeEnvelope(w, -1001, "bad body", nil)
				return
			}
			for name, raw := range in {
				sec, ok := raw.(map[string]any)
				if !ok {
					writeEnvelope(w, -1001, "section "+name+" is not an object", nil)
					return
				}
				want, known := controllerSectionArity[name]
				if !known {
					writeEnvelope(w, -1001, "unknown section "+name, nil)
					return
				}
				if len(sec) < want {
					// What the live controller does to a partial section.
					writeEnvelope(w, -1, "General error.", nil)
					return
				}
				cur, _ := controllerSettings[name].(map[string]any)
				merged := map[string]any{}
				for k, v := range cur {
					merged[k] = v
				}
				for k, v := range sec {
					merged[k] = v
				}
				controllerSettings[name] = merged
			}
			writeEnvelope(w, 0, "", nil)
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})

	mux.HandleFunc("/debug/controllerSettings", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(controllerSettings)
	})

	mux.HandleFunc("/debug/iotServers", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(iotServers)
	})

	mux.HandleFunc("/debug/singletons", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(singletons)
	})

	// Unauthenticated debug endpoints so tests can assert on the RAW stored
	// objects — specifically that keys the provider never models (STP
	// `instances`, and the WiFi `pskSetting.securityKey`) survive updates.
	mux.HandleFunc("/debug/ssids", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(ssids)
	})
	mux.HandleFunc("/debug/profiles", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(profs)
	})

	// Firmware auto-upgrade schedules (controller-scoped). Mirrors the live
	// controller: no GET by id, a PATCH missing any top-level field is refused
	// with -1001, DELETE succeeds for an unknown id, and the list reports sites
	// by name only (ids come from /sites/{id}). The models endpoint returns
	// what site-1 has adopted.
	fwSchedules := map[string]map[string]any{}
	fwNext := 1
	siteNamesByID := map[string]string{"site-1": "Default"}
	fwModels := []map[string]any{
		{"compoundModel": "EAP670(US) v2.0", "showModel": "EAP670(US) v2.0", "version": "1.1.3"},
		{"compoundModel": "ES205G v1.20", "showModel": "ES205G v1.20", "version": "1.0.0"},
	}
	const fwBase = "/abc123/api/v2/upgrade/autoCheck"
	fwComplete := func(in map[string]any) bool {
		for _, k := range []string{"siteIds", "modelTypeInfos", "occurrence", "channel"} {
			if in[k] == nil {
				return false
			}
		}
		return true
	}
	mux.HandleFunc("/abc123/api/v2/upgrade/models", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{"modelTypeInfos": fwModels})
	})
	mux.HandleFunc(fwBase, func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !fwComplete(in) {
				writeEnvelope(w, -1001, "must not be null", nil)
				return
			}
			id := fmt.Sprintf("fw-%d", fwNext)
			fwNext++
			fwSchedules[id] = in
			writeEnvelope(w, 0, "", map[string]any{"autoCheckId": id})
		case http.MethodGet:
			data := make([]map[string]any, 0, len(fwSchedules))
			for id, s := range fwSchedules {
				names := []string{}
				ids, _ := s["siteIds"].([]any)
				for _, sid := range ids {
					names = append(names, siteNamesByID[fmt.Sprint(sid)])
				}
				data = append(data, map[string]any{
					"id": id, "siteNames": names, "siteNum": len(names),
					"modelTypeInfos": s["modelTypeInfos"], "occurrence": s["occurrence"],
					"channel": s["channel"], "autoCheckTime": "Sep 27, 2026 04:15:00 am",
				})
			}
			writeEnvelope(w, 0, "", map[string]any{"totalRows": len(data), "currentPage": 1, "currentSize": 100, "data": data})
		default:
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})
	mux.HandleFunc(fwBase+"/", func(w http.ResponseWriter, r *http.Request) {
		if !requireToken(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, fwBase+"/")
		mu.Lock()
		defer mu.Unlock()
		if id, ok := strings.CutPrefix(rest, "sites/"); ok && r.Method == http.MethodGet {
			s, found := fwSchedules[id]
			if !found {
				writeEnvelope(w, -34326, "object does not exist", nil)
				return
			}
			sites := []map[string]any{}
			ids, _ := s["siteIds"].([]any)
			for _, sid := range ids {
				sites = append(sites, map[string]any{"id": sid, "name": siteNamesByID[fmt.Sprint(sid)]})
			}
			writeEnvelope(w, 0, "", map[string]any{"sites": sites})
			return
		}
		switch r.Method {
		case http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !fwComplete(in) {
				writeEnvelope(w, -1001, "must not be null", nil)
				return
			}
			if _, found := fwSchedules[rest]; !found {
				writeEnvelope(w, -34326, "object does not exist", nil)
				return
			}
			fwSchedules[rest] = in
			writeEnvelope(w, 0, "", nil)
		case http.MethodDelete:
			// Live: success whether or not the id exists.
			delete(fwSchedules, rest)
			writeEnvelope(w, 0, "", nil)
		default:
			// Live: there is no GET by id.
			writeEnvelope(w, -1600, "Unsupported request path.", nil)
		}
	})
	// Test-only hook: the schedules as stored, so a test can assert what was
	// actually sent (e.g. that update carried the whole body).
	mux.HandleFunc("/debug/firmware-schedules", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(fwSchedules)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// rawStore fetches one of the mock's debug endpoints (e.g. "ssids", "profiles").
func rawStore(t *testing.T, base, kind string) map[string]map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/debug/" + kind)
	if err != nil {
		t.Fatalf("debug fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("debug decode: %v", err)
	}
	return out
}

// validPortForwardProtocol mirrors the live controller's "must be between 0
// and 2" range check on protocol (0=TCP+UDP, 1=TCP, 2=UDP) — see issue #58.
func validPortForwardProtocol(v any) bool {
	n, ok := v.(float64)
	return ok && n >= 0 && n <= 2
}

func writeEnvelope(w http.ResponseWriter, code int, msg string, result any) {
	var raw json.RawMessage
	if result != nil {
		raw, _ = json.Marshal(result)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errorCode": code, "msg": msg, "result": raw})
}

// testProviderConfig renders a provider block pointed at the mock controller.
// testProviderConfigOpenAPI adds Open API credentials, which the operations
// that go through that surface need: network create, and omada_switch_port.
func testProviderConfigOpenAPI(url string) string {
	return fmt.Sprintf(`
provider "omada" {
  url                   = %q
  username              = "admin"
  password              = "secret"
  openapi_client_id     = "oa-client"
  openapi_client_secret = "oa-secret"
  skip_tls_verify       = true
}
`, url)
}

func testProviderConfig(url string) string {
	return fmt.Sprintf(`
provider "omada" {
  url             = %q
  username        = "admin"
  password        = "secret"
  skip_tls_verify = true
}
`, url)
}
