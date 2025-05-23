package azure_identity

import (
	"context"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/file"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

var log = clog.NewWithPlugin("azure_identity")

type AzureIdentity struct {
	resourceGroupName string
	dnsZone           string
	tenantId          string
	subscriptionId    string
	provider          *AzureProvider
	Next              plugin.Handler
}

func (az AzureIdentity) Name() string { return "azure_identity" }

func (az AzureIdentity) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {

	state := request.Request{W: w, Req: r}
	qname := state.Name()

	zone := plugin.Zones(az.provider.zoneNames).Matches(qname)

	if zone == "" {
		log.Infof("%s is not part of our current zone %s.", qname, zone)
		return plugin.NextOrFailure(az.Name(), az.Next, ctx, w, r)
	}

	mz := az.provider.findZone(zone)

	if mz == nil {
		log.Errorf("Doesn't find the correct zone %s in the cache.", zone)
		return dns.RcodeServerFailure, nil
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	var result file.Result
	m.Answer, m.Ns, m.Extra, result = mz.z.Lookup(ctx, state, qname)

	if len(m.Answer) == 0 && result != file.NoData {
		log.Warningf("No data found for qname %s.", qname)
		return plugin.NextOrFailure(az.Name(), az.Next, ctx, w, r)
	}

	switch result {
	case file.Success:
	case file.NoData:
	case file.NameError:
		m.Rcode = dns.RcodeNameError
	case file.Delegation:
		m.Authoritative = false
	case file.ServerFailure:
		return dns.RcodeServerFailure, nil
	}

	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}
