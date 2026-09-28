// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dns

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.yaml.in/yaml/v2"

	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestDNS(t *testing.T) {
	testCases := []struct {
		name   string
		config SDConfig
		lookup func(name string, qtype uint16, logger *slog.Logger) (*dns.Msg, error)

		expected []*targetgroup.Group
	}{
		{
			name: "A record query with error",
			config: SDConfig{
				Names:           []string{"web.example.com."},
				RefreshInterval: model.Duration(time.Minute),
				Port:            80,
				Type:            "A",
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return nil, errors.New("some error")
			},
			expected: []*targetgroup.Group{},
		},
		{
			name: "A record query",
			config: SDConfig{
				Names:           []string{"web.example.com."},
				RefreshInterval: model.Duration(time.Minute),
				Port:            80,
				Type:            "A",
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{
						Answer: []dns.RR{
							&dns.A{A: net.IPv4(192, 0, 2, 2)},
						},
					},
					nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "web.example.com.",
					Targets: []model.LabelSet{
						{
							"__address__":                  "192.0.2.2:80",
							"__meta_dns_name":              "web.example.com.",
							"__meta_dns_srv_record_target": "",
							"__meta_dns_srv_record_port":   "",
							"__meta_dns_mx_record_target":  "",
							"__meta_dns_ns_record_target":  "",
						},
					},
				},
			},
		},
		{
			name: "AAAA record query",
			config: SDConfig{
				Names:           []string{"web.example.com."},
				RefreshInterval: model.Duration(time.Minute),
				Port:            80,
				Type:            "AAAA",
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{
						Answer: []dns.RR{
							&dns.AAAA{AAAA: net.IPv6loopback},
						},
					},
					nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "web.example.com.",
					Targets: []model.LabelSet{
						{
							"__address__":                  "[::1]:80",
							"__meta_dns_name":              "web.example.com.",
							"__meta_dns_srv_record_target": "",
							"__meta_dns_srv_record_port":   "",
							"__meta_dns_mx_record_target":  "",
							"__meta_dns_ns_record_target":  "",
						},
					},
				},
			},
		},
		{
			name: "SRV record query",
			config: SDConfig{
				Names:           []string{"_mysql._tcp.db.example.com."},
				Type:            "SRV",
				RefreshInterval: model.Duration(time.Minute),
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{
						Answer: []dns.RR{
							&dns.SRV{Port: 3306, Target: "db1.example.com."},
							&dns.SRV{Port: 3306, Target: "db2.example.com."},
						},
					},
					nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "_mysql._tcp.db.example.com.",
					Targets: []model.LabelSet{
						{
							"__address__":                  "db1.example.com:3306",
							"__meta_dns_name":              "_mysql._tcp.db.example.com.",
							"__meta_dns_srv_record_target": "db1.example.com.",
							"__meta_dns_srv_record_port":   "3306",
							"__meta_dns_mx_record_target":  "",
							"__meta_dns_ns_record_target":  "",
						},
						{
							"__address__":                  "db2.example.com:3306",
							"__meta_dns_name":              "_mysql._tcp.db.example.com.",
							"__meta_dns_srv_record_target": "db2.example.com.",
							"__meta_dns_srv_record_port":   "3306",
							"__meta_dns_mx_record_target":  "",
							"__meta_dns_ns_record_target":  "",
						},
					},
				},
			},
		},
		{
			name: "SRV record query with unsupported resource records",
			config: SDConfig{
				Names:           []string{"_mysql._tcp.db.example.com."},
				RefreshInterval: model.Duration(time.Minute),
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{
						Answer: []dns.RR{
							&dns.SRV{Port: 3306, Target: "db1.example.com."},
							&dns.TXT{Txt: []string{"this should be discarded"}},
						},
					},
					nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "_mysql._tcp.db.example.com.",
					Targets: []model.LabelSet{
						{
							"__address__":                  "db1.example.com:3306",
							"__meta_dns_name":              "_mysql._tcp.db.example.com.",
							"__meta_dns_srv_record_target": "db1.example.com.",
							"__meta_dns_srv_record_port":   "3306",
							"__meta_dns_mx_record_target":  "",
							"__meta_dns_ns_record_target":  "",
						},
					},
				},
			},
		},
		{
			name: "SRV record query with empty answer (NXDOMAIN)",
			config: SDConfig{
				Names:           []string{"_mysql._tcp.db.example.com."},
				RefreshInterval: model.Duration(time.Minute),
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{}, nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "_mysql._tcp.db.example.com.",
				},
			},
		},
		{
			name: "MX record query",
			config: SDConfig{
				Names:           []string{"example.com."},
				Type:            "MX",
				Port:            25,
				RefreshInterval: model.Duration(time.Minute),
			},
			lookup: func(string, uint16, *slog.Logger) (*dns.Msg, error) {
				return &dns.Msg{
						Answer: []dns.RR{
							&dns.MX{Preference: 0, Mx: "smtp1.example.com."},
							&dns.MX{Preference: 10, Mx: "smtp2.example.com."},
						},
					},
					nil
			},
			expected: []*targetgroup.Group{
				{
					Source: "example.com.",
					Targets: []model.LabelSet{
						{
							"__address__":                  "smtp1.example.com:25",
							"__meta_dns_name":              "example.com.",
							"__meta_dns_srv_record_target": "",
							"__meta_dns_srv_record_port":   "",
							"__meta_dns_mx_record_target":  "smtp1.example.com.",
							"__meta_dns_ns_record_target":  "",
						},
						{
							"__address__":                  "smtp2.example.com:25",
							"__meta_dns_name":              "example.com.",
							"__meta_dns_srv_record_target": "",
							"__meta_dns_srv_record_port":   "",
							"__meta_dns_mx_record_target":  "smtp2.example.com.",
							"__meta_dns_ns_record_target":  "",
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := prometheus.NewRegistry()
			refreshMetrics := discovery.NewRefreshMetrics(reg)
			metrics := tc.config.NewDiscovererMetrics(reg, refreshMetrics)
			require.NoError(t, metrics.Register())

			sd, err := NewDiscovery(tc.config, discovery.DiscovererOptions{
				Logger:  nil,
				Metrics: metrics,
				SetName: "dns",
			})
			require.NoError(t, err)
			sd.lookupFn = tc.lookup

			tgs, err := sd.refresh(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.expected, tgs)

			metrics.Unregister()
		})
	}
}

func TestSDConfigUnmarshalYAML(t *testing.T) {
	marshal := func(c SDConfig) []byte {
		d, err := yaml.Marshal(c)
		if err != nil {
			panic(err)
		}
		return d
	}

	unmarshal := func(d []byte) func(any) error {
		return func(o any) error {
			return yaml.Unmarshal(d, o)
		}
	}

	cases := []struct {
		name      string
		input     SDConfig
		expectErr bool
	}{
		{
			name: "valid srv",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "SRV",
			},
			expectErr: false,
		},
		{
			name: "valid a",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "A",
				Port:  5300,
			},
			expectErr: false,
		},
		{
			name: "valid aaaa",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "AAAA",
				Port:  5300,
			},
			expectErr: false,
		},
		{
			name: "invalid a without port",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "A",
			},
			expectErr: true,
		},
		{
			name: "invalid aaaa without port",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "AAAA",
			},
			expectErr: true,
		},
		{
			name: "invalid empty names",
			input: SDConfig{
				Names: []string{},
				Type:  "AAAA",
			},
			expectErr: true,
		},
		{
			name: "invalid unknown dns type",
			input: SDConfig{
				Names: []string{"a.example.com", "b.example.com"},
				Type:  "PTR",
			},
			expectErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var config SDConfig
			d := marshal(c.input)
			err := config.UnmarshalYAML(unmarshal(d))
			require.Equal(t, c.expectErr, err != nil)
		})
	}
}

// TestLookupWithConfig documents how names are resolved: only the configured
// nameservers are queried, with ordinary unicast queries, so names that are
// only known to /etc/hosts or to multicast DNS (such as ".local" names served
// by Avahi) are not resolved. See https://github.com/prometheus/prometheus/issues/2537.
func TestLookupWithConfig(t *testing.T) {
	testCases := []struct {
		name       string
		resolvConf string
		lookup     string
		// answers maps query names to their rcode and records. Other names get NXDOMAIN.
		answers map[string]fakeAnswer

		expectQueries []string
		expectAnswer  []string
		expectErr     bool
	}{
		{
			name:          ".local name answered by the configured nameserver",
			resolvConf:    "nameserver 127.0.0.1\n",
			lookup:        "printer.local",
			answers:       map[string]fakeAnswer{"printer.local.": {rrs: []string{"printer.local. 60 IN A 192.0.2.10"}}},
			expectQueries: []string{"printer.local."},
			expectAnswer:  []string{"printer.local.\t60\tIN\tA\t192.0.2.10"},
		},
		{
			// Without a nameserver answering for it, a .local name yields no targets and no error.
			name:          ".local name unknown to the configured nameserver",
			resolvConf:    "nameserver 127.0.0.1\n",
			lookup:        "printer.local",
			expectQueries: []string{"printer.local."},
		},
		{
			// localhost is in every /etc/hosts, but the answer only comes from the nameserver.
			name:          "/etc/hosts is not consulted",
			resolvConf:    "nameserver 127.0.0.1\n",
			lookup:        "localhost.",
			expectQueries: []string{"localhost."},
		},
		{
			name:       "search domains are tried before the name without enough dots",
			resolvConf: "nameserver 127.0.0.1\nsearch a.example b.example\n",
			lookup:     "web",
			answers:    map[string]fakeAnswer{"web.b.example.": {rrs: []string{"web.b.example. 60 IN A 192.0.2.20"}}},
			// The first successful answer ends the lookup.
			expectQueries: []string{"web.a.example.", "web.b.example."},
			expectAnswer:  []string{"web.b.example.\t60\tIN\tA\t192.0.2.20"},
		},
		{
			name:          "name with enough dots is tried before search domains",
			resolvConf:    "nameserver 127.0.0.1\nsearch a.example\n",
			lookup:        "printer.local",
			expectQueries: []string{"printer.local.", "printer.local.a.example."},
		},
		{
			name:          "ndots option is honoured",
			resolvConf:    "nameserver 127.0.0.1\nsearch a.example\noptions ndots:2\n",
			lookup:        "printer.local",
			expectQueries: []string{"printer.local.a.example.", "printer.local."},
		},
		{
			name:          "fully qualified name skips search domains",
			resolvConf:    "nameserver 127.0.0.1\nsearch a.example\n",
			lookup:        "printer.local.",
			expectQueries: []string{"printer.local."},
		},
		{
			name:          "server failure for any name is an error",
			resolvConf:    "nameserver 127.0.0.1\nsearch a.example\n",
			lookup:        "web",
			answers:       map[string]fakeAnswer{"web.": {rcode: dns.RcodeServerFailure}},
			expectQueries: []string{"web.a.example.", "web."},
			expectErr:     true,
		},
		{
			name:       "no nameserver is an error",
			resolvConf: "search a.example\n",
			lookup:     "web",
			expectErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newFakeDNSServer(t, tc.answers)

			conf, err := dns.ClientConfigFromReader(strings.NewReader(tc.resolvConf))
			require.NoError(t, err)
			conf.Port = srv.port

			msg, err := lookupWithConfig(tc.lookup, dns.TypeA, conf, promslog.NewNopLogger())
			require.Equal(t, tc.expectQueries, srv.queries())
			if tc.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			answer := make([]string, 0, len(msg.Answer))
			for _, rr := range msg.Answer {
				answer = append(answer, rr.String())
			}
			require.ElementsMatch(t, tc.expectAnswer, answer)
		})
	}
}

type fakeAnswer struct {
	rcode int
	rrs   []string
}

// fakeDNSServer is a unicast DNS server on 127.0.0.1 answering A queries
// from a fixed set of answers and recording the names it was asked for.
type fakeDNSServer struct {
	port string

	mtx   sync.Mutex
	names []string
}

func newFakeDNSServer(t *testing.T, answers map[string]fakeAnswer) *fakeDNSServer {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(pc.LocalAddr().String())
	require.NoError(t, err)

	f := &fakeDNSServer{port: port}
	started := make(chan struct{})
	srv := &dns.Server{
		PacketConn:        pc,
		NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
			q := req.Question[0]
			f.mtx.Lock()
			f.names = append(f.names, q.Name)
			f.mtx.Unlock()

			resp := &dns.Msg{}
			resp.SetReply(req)
			resp.Rcode = dns.RcodeNameError
			if a, ok := answers[q.Name]; ok {
				resp.Rcode = a.rcode
				for _, s := range a.rrs {
					rr, err := dns.NewRR(s)
					if err != nil {
						panic(err)
					}
					resp.Answer = append(resp.Answer, rr)
				}
			}
			_ = w.WriteMsg(resp)
		}),
	}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { require.NoError(t, srv.Shutdown()) })
	return f
}

func (f *fakeDNSServer) queries() []string {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.names
}
