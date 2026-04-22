package discovery

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
)

// fakeResolver is a table-driven Resolver used by tests.
type fakeResolver struct {
	srv  map[string][]*net.SRV
	host map[string][]string
	err  map[string]error
}

func (f *fakeResolver) LookupSRV(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
	if err, ok := f.err["srv:"+name]; ok {
		return "", nil, err
	}
	return name, f.srv[name], nil
}

func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if err, ok := f.err["host:"+host]; ok {
		return nil, err
	}
	return f.host[host], nil
}

func TestExpandSeedsPassesThroughPlain(t *testing.T) {
	t.Parallel()
	got := ExpandSeeds(context.Background(), []string{"10.0.0.1:7443", "node-2:7443"}, &fakeResolver{}, nil)
	want := []string{"10.0.0.1:7443", "node-2:7443"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandSeedsResolvesSRV(t *testing.T) {
	t.Parallel()
	r := &fakeResolver{
		srv: map[string][]*net.SRV{
			"_cluster._tcp.lobslaw.default.svc.cluster.local": {
				{Target: "lobslaw-0.lobslaw.default.svc.cluster.local.", Port: 7443},
				{Target: "lobslaw-1.lobslaw.default.svc.cluster.local.", Port: 7443},
				{Target: "lobslaw-2.lobslaw.default.svc.cluster.local.", Port: 7443},
			},
		},
	}
	got := ExpandSeeds(context.Background(),
		[]string{"srv:_cluster._tcp.lobslaw.default.svc.cluster.local"},
		r, nil)
	want := []string{
		"lobslaw-0.lobslaw.default.svc.cluster.local:7443",
		"lobslaw-1.lobslaw.default.svc.cluster.local:7443",
		"lobslaw-2.lobslaw.default.svc.cluster.local:7443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandSeedsResolvesDNSA(t *testing.T) {
	t.Parallel()
	r := &fakeResolver{
		host: map[string][]string{
			"lobslaw.home.arpa": {"10.0.0.10", "10.0.0.11", "10.0.0.12"},
		},
	}
	got := ExpandSeeds(context.Background(),
		[]string{"dns:lobslaw.home.arpa:7443"},
		r, nil)
	want := []string{
		"10.0.0.10:7443",
		"10.0.0.11:7443",
		"10.0.0.12:7443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandSeedsDedupesAcrossForms(t *testing.T) {
	t.Parallel()
	r := &fakeResolver{
		srv: map[string][]*net.SRV{
			"_cluster._tcp.example": {
				{Target: "node-a.", Port: 7443},
			},
		},
		host: map[string][]string{
			"node-a": {"10.0.0.5"},
		},
	}
	got := ExpandSeeds(context.Background(), []string{
		"node-a:7443",               // plain
		"srv:_cluster._tcp.example", // same target, same port
		"dns:node-a:7443",           // same host name, different resolution path
		"node-a:7443",               // exact dup
	}, r, nil)
	// After de-dupe: node-a:7443 (from plain + srv) and 10.0.0.5:7443 (from dns).
	want := []string{
		"10.0.0.5:7443",
		"node-a:7443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandSeedsSkipsFailedLookups(t *testing.T) {
	t.Parallel()
	r := &fakeResolver{
		host: map[string][]string{
			"good.example": {"10.0.0.1"},
		},
		err: map[string]error{
			"srv:_cluster._tcp.broken.example": errors.New("NXDOMAIN"),
			"host:broken.example":              errors.New("NXDOMAIN"),
		},
	}
	got := ExpandSeeds(context.Background(), []string{
		"srv:_cluster._tcp.broken.example",
		"dns:broken.example:7443",
		"dns:good.example:7443",
		"plain:7443",
	}, r, nil)
	want := []string{
		"10.0.0.1:7443",
		"plain:7443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandSeedsRejectsMalformedDNS(t *testing.T) {
	t.Parallel()
	// dns: form requires host:port
	got := ExpandSeeds(context.Background(), []string{
		"dns:no-port-here",
	}, &fakeResolver{}, nil)
	if len(got) != 0 {
		t.Errorf("malformed dns: should be skipped, got %v", got)
	}
}

func TestExpandSeedsEmptyAndWhitespace(t *testing.T) {
	t.Parallel()
	got := ExpandSeeds(context.Background(), []string{
		"",
		"   ",
		"   node-a:7443   ",
	}, &fakeResolver{}, nil)
	want := []string{"node-a:7443"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
