package main

import (
	"net"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
)

const base = `"cniVersion":"1.0.0","name":"vxlan","type":"vxlan","remote":"192.168.99.129"`

func conf(t *testing.T, extra string) *NetConf {
	t.Helper()
	n, err := loadConf([]byte("{" + base + "," + extra + "}"))
	if err != nil {
		t.Fatalf("loadConf: %v", err)
	}
	return n
}

func TestLoadConfErrors(t *testing.T) {
	cases := map[string]string{
		"missing allowedVlans": `{` + base + `}`,
		"bad remote":           `{"remote":"fg","allowedVlans":[10]}`,
		"ipv6 remote":          `{"remote":"2001:db8::1","allowedVlans":[10]}`,
		"bad peer":             `{` + base + `,"allowedVlans":[10],"peers":["x"]}`,
		"vlan out of range":    `{` + base + `,"allowedVlans":[4095]}`,
		"reversed range":       `{` + base + `,"allowedVlans":["45-42"]}`,
		"bad port":             `{` + base + `,"allowedVlans":[10],"port":70000}`,
	}
	for name, in := range cases {
		if _, err := loadConf([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadConfDefaults(t *testing.T) {
	n := conf(t, `"allowedVlans":[10]`)
	if n.Port != 4789 {
		t.Errorf("port = %d, want 4789", n.Port)
	}
}

func TestResolveVLAN(t *testing.T) {
	allow := `"allowedVlans":[10, "20", "42-45"]`
	cases := []struct {
		name    string
		extra   string
		cniArgs string
		want    int
		err     string
	}{
		{"cni-args number", `"args":{"cni":{"vlan":20}}`, "", 20, ""},
		{"cni-args string", `"args":{"cni":{"vlan":"43"}}`, "", 43, ""},
		{"CNI_ARGS", ``, "IgnoreUnknown=1;VLAN=45", 45, ""},
		{"default", `"vlan":10`, "", 10, ""},
		{"cni-args wins", `"vlan":10,"args":{"cni":{"vlan":20}}`, "VLAN=42", 20, ""},
		{"none", ``, "", 0, "no VLAN"},
		{"not allowed", `"args":{"cni":{"vlan":99}}`, "", 0, "not in allowedVlans"},
		{"range edge", `"args":{"cni":{"vlan":46}}`, "", 0, "not in allowedVlans"},
		{"invalid", `"args":{"cni":{"vlan":0}}`, "", 0, "invalid VLAN"},
	}
	for _, c := range cases {
		extra := allow
		if c.extra != "" {
			extra += "," + c.extra
		}
		got, err := conf(t, extra).resolveVLAN(c.cniArgs)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, %v; want %d", c.name, got, err, c.want)
		}
	}
}

func TestNames(t *testing.T) {
	if vxlanName(4094) != "vxlan4094" || bridgeNameOf(4094) != "vxbr4094" {
		t.Fatal("unexpected names")
	}
}

func TestFilterRoutes(t *testing.T) {
	_, def, _ := net.ParseCIDR("0.0.0.0/0")
	_, sub, _ := net.ParseCIDR("10.8.0.0/16")
	routes := []*types.Route{{Dst: *def}, {Dst: *sub}}
	if got := filterRoutes(routes, false); len(got) != 1 || got[0].Dst.String() != "10.8.0.0/16" {
		t.Errorf("default route not dropped: %v", got)
	}
	if got := filterRoutes(routes, true); len(got) != 2 {
		t.Errorf("keepDefaultRoute ignored: %v", got)
	}
}
